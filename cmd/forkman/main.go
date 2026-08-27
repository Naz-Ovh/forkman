// Command forkman keeps an organization's forks in sync with their upstream
// parents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/naz-ovh/forkman/internal/config"
	"github.com/naz-ovh/forkman/internal/github"
	"github.com/naz-ovh/forkman/internal/plain"
	"github.com/naz-ovh/forkman/internal/preflight"
	fsync "github.com/naz-ovh/forkman/internal/sync"
	"github.com/naz-ovh/forkman/internal/tui"
)

// version is overridden at build time via -ldflags -X main.version=... It must
// stay initialised to a constant string: the linker's -X only rewrites string
// variables that are uninitialised or set to a constant expression.
var version = "dev"

// init recovers a version for binaries the linker never stamped. `go install
// <module>/cmd/forkman@vX.Y.Z` cannot pass ldflags, but the toolchain records
// the module version in the build info, so read it back from there.
func init() {
	if version != "dev" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
}

// exit codes not owned by other packages
const (
	exitOK       = 0
	exitInternal = 1
	exitConfig   = 2
	exitFailures = 6
	exitSignal   = 130
)

const noOrgMsg = "no organization configured; run: forkman configure --org=<name>"

const usageText = `forkman ` + `— keep an organization's forks in sync with upstream

Usage:
  forkman sync       [--plain] [--json] [--dry-run] [--concurrency N] [--mode api|git] [--dir PATH]
  forkman clone      [--plain] [--json] [--dry-run] [--concurrency N] [--full] [--dir PATH]
  forkman configure  [--org NAME] [--exclude a,b] [--exclude-add a,b] [--exclude-remove a,b]
                     [--concurrency N] [--clone-dir PATH] [--mode api|git] [--protocol ssh|https]
  forkman doctor
  forkman --version

Environment:
  FORKMAN_TOKEN, GH_TOKEN, GITHUB_TOKEN   GitHub token (else ` + "`gh auth token`" + `)
  NO_COLOR / FORCE_COLOR                  colour control
  FORKMAN_DEBUG=1                         debug logging to stderr
`

func main() {
	code := exitInternal
	defer func() {
		if r := recover(); r != nil {
			// Never a stack trace: users get one line.
			fmt.Fprintln(os.Stderr, "forkman: internal error:", r)
			os.Exit(exitInternal)
		}
		os.Exit(code)
	}()
	code = run(os.Args[1:])
}

func run(args []string) int {
	setupLogging()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return exitConfig
	}
	switch args[0] {
	case "--version", "-version", "version":
		fmt.Printf("forkman %s\n", version)
		return exitOK
	case "--help", "-h", "-help", "help":
		fmt.Fprint(os.Stdout, usageText)
		return exitOK
	case "sync":
		return cmdRun(args[1:], fsync.KindSync)
	case "clone":
		return cmdRun(args[1:], fsync.KindClone)
	case "configure":
		return cmdConfigure(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	}
	fmt.Fprintf(os.Stderr, "forkman: unknown command %q\n\n", args[0])
	fmt.Fprint(os.Stderr, usageText)
	return exitConfig
}

func setupLogging() {
	level := slog.LevelWarn
	if os.Getenv("FORKMAN_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// isTTY reports whether f is a character device.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func loadConfig() (*config.Config, string, int) {
	path, err := config.Path()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return nil, "", exitConfig
	}
	cfg, err := config.Load(path)
	switch {
	case errors.Is(err, config.ErrNotFound):
		fmt.Fprintln(os.Stderr, "forkman: "+noOrgMsg)
		return nil, path, exitConfig
	case err != nil:
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return nil, path, exitConfig
	}
	cfg.Normalize()
	if cfg.Org == "" {
		fmt.Fprintln(os.Stderr, "forkman: "+noOrgMsg)
		return nil, path, exitConfig
	}
	return cfg, path, exitOK
}

func cmdRun(args []string, kind fsync.Kind) int {
	name := "sync"
	if kind == fsync.KindClone {
		name = "clone"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		plainOut = fs.Bool("plain", false, "one line per repository, no TUI")
		jsonOut  = fs.Bool("json", false, "one JSON object per line")
		dryRun   = fs.Bool("dry-run", false, "print the plan and exit")
		conc     = fs.Int("concurrency", 0, "override configured concurrency")
		dir      = fs.String("dir", "", "override the configured clone directory")
		full     *bool
		mode     *string
	)
	if kind == fsync.KindClone {
		full = fs.Bool("full", false, "clone the files too, instead of history only (blob:none, no checkout)")
	} else {
		mode = fs.String("mode", "", "sync mode: api (merge-upstream) or git (local clones + push)")
	}
	if err := fs.Parse(args); err != nil {
		return exitConfig
	}

	cfg, cfgPath, code := loadConfig()
	if code != exitOK {
		return code
	}
	if *conc > 0 {
		cfg.Concurrency = *conc
	}
	if mode != nil && *mode != "" {
		cfg.SyncMode = *mode
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return exitConfig
	}
	cfg.Normalize()

	cloneDir := cfg.CloneDir
	if *dir != "" {
		resolved, err := config.ResolveDir(*dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "forkman:", err)
			return exitConfig
		}
		cloneDir = resolved
	}
	gitMode := kind == fsync.KindSync && cfg.GitMode()
	if (kind == fsync.KindClone || gitMode) && cloneDir == "" {
		fmt.Fprintln(os.Stderr, "forkman: no clone directory configured; run: forkman configure --clone-dir=PATH")
		return exitConfig
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess := &session{
		cfg:      cfg,
		cfgPath:  cfgPath,
		kind:     kind,
		cloneDir: cloneDir,
		gitMode:  gitMode,
		full:     full != nil && *full,
	}
	w := &plain.Writer{W: os.Stdout, JSON: *jsonOut}
	interactive := isTTY(os.Stdout) && isTTY(os.Stdin)
	if *plainOut || *jsonOut || !interactive {
		return runPlain(ctx, sess, w, *dryRun)
	}
	return runTUI(ctx, sess, w, *dryRun)
}

// runPlain is the pipe-friendly path: no alt screen, one line per repository.
// Startup progress goes to stderr, and only when a person is watching, so a
// pipe still gets nothing but the data.
func runPlain(ctx context.Context, s *session, w *plain.Writer, dryRun bool) int {
	prep, err := s.prepare(ctx, stderrSteps())
	if err != nil {
		if ctx.Err() != nil {
			return exitSignal
		}
		return reportPrepareFailure(err)
	}
	if dryRun {
		return dryRunReport(w, prep.Tasks, s.kind, s.gitMode)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results, interrupted := consumePlain(runCtx, s.start(runCtx, prep.Tasks), w)
	if ctx.Err() != nil {
		interrupted = true
	}
	summary := fsync.Summarize(results)
	w.Summary(summary)
	return fsync.ExitCode(summary, interrupted)
}

// runTUI opens the alt screen first and runs the preflight checks, fork
// discovery and the work itself inside it, so none of the waiting happens on a
// blank terminal.
func runTUI(ctx context.Context, s *session, w *plain.Writer, dryRun bool) int {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	opts := tui.Options{
		Org:     s.cfg.Org,
		Kind:    s.kind,
		Plain:   tui.PlainFromEnv(os.Getenv),
		Version: version,
		Cancel:  cancel,
		Prepare: s.prepare,
		Confirm: s.confirm,
	}
	if !dryRun {
		opts.Start = func(tasks []fsync.Task) <-chan fsync.Event {
			return s.start(runCtx, tasks)
		}
	}
	out, err := tui.Run(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return exitInternal
	}
	// A failure leaves no plan behind either, so it is asked about before the
	// "nothing ran" case — unless the user's own cancellation is what caused it.
	switch {
	case out.Err != nil && !out.Cancelled:
		return reportPrepareFailure(out.Err)
	case out.Cancelled || out.Tasks == nil:
		// Nothing ran, so there is nothing to summarise.
		fmt.Fprintln(os.Stderr, "forkman: cancelled")
		if out.Interrupted {
			return exitSignal
		}
		return exitOK
	case dryRun:
		return dryRunReport(w, out.Tasks, s.kind, s.gitMode)
	}

	results := out.Results
	if s.events != nil {
		results = append(results, drain(s.events, 2*time.Second)...)
	}
	summary := fsync.Summarize(results)
	printSummary(os.Stdout, summary, s.pf)
	return fsync.ExitCode(summary, out.Interrupted || ctx.Err() != nil)
}

// session holds what one run was invoked with, plus the preflight and
// discovery results once they exist. Both front-ends drive the same three
// methods, which is what lets the TUI run preflight from inside the alt screen
// while the plain path runs it straight through.
type session struct {
	cfg      *config.Config
	cfgPath  string
	kind     fsync.Kind
	cloneDir string
	gitMode  bool
	full     bool
	// alwaysSelect offers the exclusion selector even when the config already
	// carries a list, which is what `forkman configure` with no flags does.
	alwaysSelect bool

	pf     *preflight.Result
	forks  []github.Fork
	events <-chan fsync.Event
}

// prepare runs the preflight checks and fork discovery, reporting each step
// through report as it starts and finishes. report may be nil.
func (s *session) prepare(ctx context.Context, report func(tui.Step)) (*tui.Prepared, error) {
	step := func(key, label, detail string, state tui.StepState) {
		if report != nil {
			report(tui.Step{Key: key, Label: label, Detail: detail, State: state})
		}
	}

	pf, fail := preflight.Run(ctx, preflight.Options{
		NeedGit:  s.kind == fsync.KindClone,
		GitMode:  s.gitMode,
		Protocol: s.cfg.Protocol,
		CloneDir: s.cloneDir,
		Org:      s.cfg.Org,
		Version:  version,
		Begin:    func(name string) { step(name, name, "", tui.StepRunning) },
		Done: func(c preflight.Check) {
			state := tui.StepOK
			if !c.OK {
				state = tui.StepFail
			}
			step(c.Name, c.Name, c.Detail, state)
		},
	})
	s.pf = pf
	if fail != nil {
		return nil, fail
	}

	// Discovery is the long pole — resolving every fork's upstream parent is
	// what GitHub spends seconds on — so it reports as it lands.
	const dk, dl = "discover", "discover forks"
	step(dk, dl, "listing "+s.cfg.Org+"'s forks", tui.StepRunning)
	forks, err := pf.Client.ListForksProgress(ctx, s.cfg.Org, &github.Discovery{
		OnListed: func(found int) {
			step(dk, dl, fmt.Sprintf("%d forks · reading branches and parents", found), tui.StepRunning)
		},
		OnDetail: func(done, total int) {
			step(dk, dl, fmt.Sprintf("%d forks · branches and parents %d/%d", total, done, total), tui.StepRunning)
		},
	})
	if err != nil {
		step(dk, dl, err.Error(), tui.StepFail)
		return nil, fmt.Errorf("discover forks: %w", err)
	}
	step(dk, dl, fmt.Sprintf("%d forks", len(forks)), tui.StepOK)
	s.forks = forks

	tasks := fsync.Plan(forks, s.cfg, s.kind)
	step("plan", "plan", planDetail(tasks, s.kind), tui.StepOK)
	return &tui.Prepared{
		Tasks: tasks,
		// A config that has never carried an "excluded" key is a first run.
		NeedSelect:    s.alwaysSelect || s.cfg.Excluded == nil,
		Items:         selectorItems(tasks),
		Preselected:   s.cfg.Excluded,
		RateRemaining: pf.Client.RateRemaining,
		ScopesKnown:   pf.ScopesKnown,
	}, nil
}

// confirm stores the exclusions the selector produced and re-plans with them.
func (s *session) confirm(excluded []string) ([]fsync.Task, error) {
	if excluded == nil {
		excluded = []string{}
	}
	s.cfg.Excluded = excluded
	if err := config.Save(s.cfgPath, s.cfg); err != nil {
		return nil, err
	}
	return fsync.Plan(s.forks, s.cfg, s.kind), nil
}

// start launches the runner and hands back the channel it publishes on.
func (s *session) start(ctx context.Context, tasks []fsync.Task) <-chan fsync.Event {
	runner := &fsync.Runner{
		Client:      s.pf.Client,
		Concurrency: s.cfg.Concurrency,
		Kind:        s.kind,
		CloneDir:    s.cloneDir,
		FullClone:   s.full,
		Org:         s.cfg.Org,
		GitMode:     s.gitMode,
		Protocol:    s.cfg.Protocol,
	}
	events := make(chan fsync.Event, 128)
	s.events = events
	go runner.Run(ctx, tasks, events)
	return events
}

// planDetail summarises the plan for the startup checklist.
func planDetail(tasks []fsync.Task, kind fsync.Kind) string {
	todo, current, skipped := 0, 0, 0
	for _, t := range tasks {
		switch {
		case t.Skip:
			skipped++
		case kind == fsync.KindSync && t.UpToDate:
			current++
		default:
			todo++
		}
	}
	verb := "sync"
	if kind == fsync.KindClone {
		verb = "clone"
	}
	parts := []string{fmt.Sprintf("%d to %s", todo, verb)}
	if current > 0 {
		parts = append(parts, fmt.Sprintf("%d up to date", current))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	return strings.Join(parts, " · ")
}

// stderrSteps prints startup progress for the plain path, one line per
// finished step. It returns nil when stderr is not a terminal: --plain and
// --json are meant to be piped, and a pipe should get data, not progress.
func stderrSteps() func(tui.Step) {
	if !isTTY(os.Stderr) {
		return nil
	}
	return func(s tui.Step) {
		if s.State == tui.StepRunning {
			return
		}
		mark := "·"
		if s.State == tui.StepFail {
			mark = "✖"
		}
		label := s.Label
		if label == "" {
			label = s.Key
		}
		fmt.Fprintf(os.Stderr, "%s %-14s %s\n", mark, label, s.Detail)
	}
}

// reportPrepareFailure prints a preflight or discovery failure on the normal
// screen, once the alt screen is gone, and maps it to an exit code.
func reportPrepareFailure(err error) int {
	var f *preflight.Failure
	if errors.As(err, &f) {
		reportFailure(f)
		return f.Code
	}
	fmt.Fprintln(os.Stderr, "forkman:", err)
	return discoverCode(err)
}

func discoverCode(err error) int {
	var ae *github.APIError
	if errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 403) {
		return preflight.CodeOrg
	}
	return exitInternal
}

func reportFailure(f *preflight.Failure) {
	fmt.Fprintln(os.Stderr, "forkman:", f.Msg)
	if f.Fix != "" {
		fmt.Fprintln(os.Stderr, "  Fix:", f.Fix)
	}
}

func selectorItems(tasks []fsync.Task) []tui.SelectorItem {
	items := make([]tui.SelectorItem, 0, len(tasks))
	for _, t := range tasks {
		it := tui.SelectorItem{Name: t.Fork.Name}
		switch {
		case t.Skip:
			it.Note = t.SkipReason
		case t.UpToDate:
			it.Note = "up to date"
		}
		items = append(items, it)
	}
	return items
}

// consumePlain streams finished results and enforces the 2s grace period
// after an interrupt.
func consumePlain(ctx context.Context, events <-chan fsync.Event, w *plain.Writer) ([]fsync.Result, bool) {
	var results []fsync.Result
	interrupted := false
	done := ctx.Done()
	var grace <-chan time.Time
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return results, interrupted
			}
			if ev.Kind == fsync.EvDone && ev.Result != nil {
				results = append(results, *ev.Result)
				w.Result(*ev.Result)
			}
		case <-done:
			done = nil
			interrupted = true
			grace = time.After(2 * time.Second)
		case <-grace:
			return results, true
		}
	}
}

// drain collects whatever the runner still emits, for at most d.
func drain(events <-chan fsync.Event, d time.Duration) []fsync.Result {
	var extra []fsync.Result
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return extra
			}
			if ev.Kind == fsync.EvDone && ev.Result != nil {
				extra = append(extra, *ev.Result)
			}
		case <-deadline:
			return extra
		}
	}
}

func dryRunReport(w *plain.Writer, tasks []fsync.Task, kind fsync.Kind, gitMode bool) int {
	for _, t := range tasks {
		r := fsync.Result{Name: t.Fork.Name}
		switch {
		case t.Skip:
			r.Status, r.Detail = fsync.Skipped, t.SkipReason
		case kind == fsync.KindSync && t.UpToDate:
			r.Status, r.Detail = fsync.UpToDate, "already up to date"
		default:
			r.Status, r.Detail = fsync.Pending, "would "+kindVerb(kind, gitMode)
		}
		w.Result(r)
	}
	return exitOK
}

func kindVerb(k fsync.Kind, gitMode bool) string {
	switch {
	case k == fsync.KindClone:
		return "clone"
	case gitMode:
		return "sync with git: fetch upstream, fast-forward push to origin"
	}
	return "sync via merge-upstream"
}

func printSummary(out io.Writer, s fsync.Summary, pf *preflight.Result) {
	fmt.Fprintf(out, "%d repos: %d synced, %d up to date, %d skipped, %d diverged, %d failed",
		s.Total, s.Synced, s.UpToDate, s.Skipped, s.Diverged, s.Failed)
	if s.Interrupted > 0 {
		fmt.Fprintf(out, ", %d interrupted", s.Interrupted)
	}
	fmt.Fprintln(out)
	if pf != nil && !pf.ScopesKnown {
		fmt.Fprintln(out, "note: token does not report scopes; workflow-file capability could not be verified in advance")
	}
}

func cmdConfigure(args []string) int {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		org      = fs.String("org", "", "GitHub organization")
		exclude  = fs.String("exclude", "", "replace the exclusion list (comma separated)")
		addEx    = fs.String("exclude-add", "", "add to the exclusion list")
		rmEx     = fs.String("exclude-remove", "", "remove from the exclusion list")
		conc     = fs.Int("concurrency", 0, "worker count (1-16)")
		cloneDir = fs.String("clone-dir", "", "directory holding the forks, stored as an absolute path")
		mode     = fs.String("mode", "", "sync mode: api (merge-upstream) or git (local clones + push)")
		protocol = fs.String("protocol", "", "git remote protocol in git mode: ssh or https")
	)
	if err := fs.Parse(args); err != nil {
		return exitConfig
	}

	path, err := config.Path()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return exitConfig
	}
	cfg, err := config.Load(path)
	fresh := false
	switch {
	case errors.Is(err, config.ErrNotFound):
		cfg, fresh = &config.Config{}, true
	case err != nil:
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return exitConfig
	}

	changed := false
	if *org != "" {
		cfg.Org, changed = *org, true
	}
	if fresh && cfg.Org == "" {
		fmt.Fprintln(os.Stderr, "forkman: "+noOrgMsg)
		return exitConfig
	}
	if isSet(fs, "exclude") {
		cfg.Excluded, changed = splitList(*exclude), true
	}
	if *addEx != "" {
		cfg.Excluded, changed = mergeList(cfg.Excluded, splitList(*addEx)), true
	}
	if *rmEx != "" {
		cfg.Excluded, changed = removeList(cfg.Excluded, splitList(*rmEx)), true
	}
	if *conc > 0 {
		cfg.Concurrency, changed = *conc, true
	}
	if isSet(fs, "clone-dir") {
		resolved, rerr := config.ResolveDir(*cloneDir)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "forkman:", rerr)
			return exitConfig
		}
		cfg.CloneDir, changed = resolved, true
	}
	if *mode != "" {
		cfg.SyncMode, changed = *mode, true
	}
	if *protocol != "" {
		cfg.Protocol, changed = *protocol, true
	}
	if verr := cfg.Validate(); verr != nil {
		fmt.Fprintln(os.Stderr, "forkman:", verr)
		return exitConfig
	}
	cfg.Normalize()

	if changed {
		if err := config.Save(path, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "forkman:", err)
			return exitInternal
		}
		printConfig(os.Stdout, path, cfg)
		return exitOK
	}

	// No flags: show the current settings, then offer the exclusion selector.
	printConfig(os.Stdout, path, cfg)
	if !isTTY(os.Stdout) || !isTTY(os.Stdin) {
		return exitOK
	}
	return configureInteractive(path, cfg)
}

func configureInteractive(path string, cfg *config.Config) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess := &session{cfg: cfg, cfgPath: path, kind: fsync.KindSync, alwaysSelect: true}
	out, err := tui.Run(ctx, tui.Options{
		Org:     cfg.Org,
		Kind:    fsync.KindSync,
		Plain:   tui.PlainFromEnv(os.Getenv),
		Version: version,
		Prepare: sess.prepare,
		Confirm: sess.confirm,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return exitInternal
	}
	switch {
	case out.Err != nil && !out.Cancelled:
		return reportPrepareFailure(out.Err)
	case out.Cancelled || out.Tasks == nil:
		fmt.Fprintln(os.Stderr, "forkman: cancelled, config unchanged")
		if out.Interrupted {
			return exitSignal
		}
		return exitOK
	}
	// confirm saved the selection through the same *config.Config.
	printConfig(os.Stdout, path, cfg)
	return exitOK
}

func printConfig(out io.Writer, path string, cfg *config.Config) {
	fmt.Fprintf(out, "config      %s\n", path)
	fmt.Fprintf(out, "org         %s\n", cfg.Org)
	fmt.Fprintf(out, "mode        %s\n", cfg.SyncMode)
	fmt.Fprintf(out, "protocol    %s\n", cfg.Protocol)
	fmt.Fprintf(out, "concurrency %d\n", cfg.Concurrency)
	dir := cfg.CloneDir
	if dir == "" {
		dir = "(unset)"
	}
	fmt.Fprintf(out, "clone dir   %s\n", dir)
	if len(cfg.Excluded) == 0 {
		fmt.Fprintln(out, "excluded    (none)")
		return
	}
	fmt.Fprintf(out, "excluded    %s\n", strings.Join(cfg.Excluded, ", "))
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := &config.Config{}
	if path, err := config.Path(); err == nil {
		if loaded, lerr := config.Load(path); lerr == nil {
			cfg = loaded
		}
	}
	cfg.Normalize()
	res, fail := preflight.Run(ctx, preflight.Options{
		NeedGit:  true,
		GitMode:  cfg.GitMode(),
		Protocol: cfg.Protocol,
		CloneDir: cfg.CloneDir,
		Org:      cfg.Org,
		Version:  version,
	})

	checks := append([]preflight.Check{modeCheck(cfg)}, res.Checks...)
	width := 0
	for _, c := range checks {
		if n := len(c.Name); n > width {
			width = n
		}
	}
	for _, c := range checks {
		mark := "FAIL"
		if c.OK {
			mark = "ok  "
		}
		fmt.Printf("%s  %-*s  %s\n", mark, width, c.Name, c.Detail)
		if !c.OK && c.Fix != "" {
			fmt.Printf("      %-*s  Fix: %s\n", width, "", c.Fix)
		}
	}
	if fail != nil {
		return fail.Code
	}
	return exitOK
}

// modeCheck is doctor's own informational row: which sync path is configured
// and where the forks live.
func modeCheck(cfg *config.Config) preflight.Check {
	dir := cfg.CloneDir
	if dir == "" {
		dir = "(unset)"
	}
	c := preflight.Check{Name: "mode", OK: true, Detail: "api (merge-upstream) · clone dir " + dir}
	if cfg.GitMode() {
		// The clone dir has its own verified row in git mode.
		c.Detail = "git over " + cfg.Protocol
	}
	return c
}

// isSet reports whether the named flag appeared on the command line, which
// distinguishes `--exclude=` (clear the list) from omitting it.
func isSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mergeList(base, add []string) []string {
	seen := make(map[string]bool, len(base))
	out := make([]string, 0, len(base)+len(add))
	for _, v := range append(append([]string{}, base...), add...) {
		k := strings.ToLower(v)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func removeList(base, drop []string) []string {
	gone := make(map[string]bool, len(drop))
	for _, v := range drop {
		gone[strings.ToLower(v)] = true
	}
	out := []string{}
	for _, v := range base {
		if !gone[strings.ToLower(v)] {
			out = append(out, v)
		}
	}
	return out
}
