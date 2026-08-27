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
	"sort"
	"strings"
	"syscall"
	"time"

	"forkman/internal/config"
	"forkman/internal/github"
	"forkman/internal/plain"
	"forkman/internal/preflight"
	fsync "forkman/internal/sync"
	"forkman/internal/tui"
)

// version is overridden at build time via -ldflags -X main.version=...
var version = "dev"

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

	pf, fail := preflight.Run(ctx, preflight.Options{
		NeedGit:  kind == fsync.KindClone,
		GitMode:  gitMode,
		Protocol: cfg.Protocol,
		CloneDir: cloneDir,
		Org:      cfg.Org,
		Version:  version,
	})
	if fail != nil {
		reportFailure(fail)
		return fail.Code
	}

	forks, err := pf.Client.ListForks(ctx, cfg.Org)
	if err != nil {
		if ctx.Err() != nil {
			return exitSignal
		}
		fmt.Fprintln(os.Stderr, "forkman: discover forks:", err)
		return discoverCode(err)
	}

	interactive := isTTY(os.Stdout) && isTTY(os.Stdin)
	usePlain := *plainOut || *jsonOut || !interactive
	tasks := fsync.Plan(forks, cfg, kind)

	// First run: the config has never carried an "excluded" key.
	if !usePlain && cfg.Excluded == nil {
		selected, cancelled, serr := tui.RunSelector(ctx, cfg.Org, selectorItems(tasks), nil)
		if serr != nil {
			fmt.Fprintln(os.Stderr, "forkman:", serr)
			return exitInternal
		}
		if cancelled {
			if ctx.Err() != nil {
				return exitSignal
			}
			fmt.Fprintln(os.Stderr, "forkman: cancelled")
			return exitOK
		}
		if selected == nil {
			selected = []string{}
		}
		cfg.Excluded = selected
		if err := config.Save(cfgPath, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "forkman:", err)
			return exitInternal
		}
		tasks = fsync.Plan(forks, cfg, kind)
	}

	w := &plain.Writer{W: os.Stdout, JSON: *jsonOut}
	if *dryRun {
		return dryRunReport(w, tasks, kind, gitMode)
	}

	runner := &fsync.Runner{
		Client:      pf.Client,
		Concurrency: cfg.Concurrency,
		Kind:        kind,
		CloneDir:    cloneDir,
		FullClone:   full != nil && *full,
		Org:         cfg.Org,
		GitMode:     gitMode,
		Protocol:    cfg.Protocol,
	}
	events := make(chan fsync.Event, 128)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go runner.Run(runCtx, tasks, events)

	var (
		results     []fsync.Result
		interrupted bool
	)
	if usePlain {
		results, interrupted = consumePlain(runCtx, events, w)
	} else {
		results, interrupted, err = tui.Run(runCtx, tui.Options{
			Org:           cfg.Org,
			Kind:          kind,
			Tasks:         tasks,
			Events:        events,
			Cancel:        cancel,
			RateRemaining: pf.Client.RateRemaining,
			Plain:         tui.PlainFromEnv(os.Getenv),
			Version:       version,
			ScopesKnown:   pf.ScopesKnown,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "forkman:", err)
			return exitInternal
		}
		results = append(results, drain(events, 2*time.Second)...)
	}
	if ctx.Err() != nil {
		interrupted = true
	}

	summary := fsync.Summarize(results)
	if usePlain {
		w.Summary(summary)
	} else {
		printSummary(os.Stdout, summary, pf)
	}
	return fsync.ExitCode(summary, interrupted)
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

	pf, fail := preflight.Run(ctx, preflight.Options{Org: cfg.Org, Version: version})
	if fail != nil {
		reportFailure(fail)
		return fail.Code
	}
	forks, err := pf.Client.ListForks(ctx, cfg.Org)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkman: discover forks:", err)
		return discoverCode(err)
	}
	tasks := fsync.Plan(forks, cfg, fsync.KindSync)
	selected, cancelled, err := tui.RunSelector(ctx, cfg.Org, selectorItems(tasks), cfg.Excluded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return exitInternal
	}
	if cancelled {
		fmt.Fprintln(os.Stderr, "forkman: cancelled, config unchanged")
		if ctx.Err() != nil {
			return exitSignal
		}
		return exitOK
	}
	if selected == nil {
		selected = []string{}
	}
	cfg.Excluded = selected
	if err := config.Save(path, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "forkman:", err)
		return exitInternal
	}
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
