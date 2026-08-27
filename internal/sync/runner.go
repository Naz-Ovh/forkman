package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"forkman/internal/clone"
	"forkman/internal/config"
	"forkman/internal/github"
)

const maxLogLines = 200

// Runner executes a plan with a bounded worker pool. Workers only ever send
// Events; they never touch UI state.
type Runner struct {
	Client      *github.Client
	Concurrency int
	Kind        Kind
	CloneDir    string
	FullClone   bool
	Org         string
	Now         func() time.Time

	// GitMode syncs through local clones and plain git instead of the API.
	GitMode bool
	// Protocol is "ssh" or "https" and shapes the remote URLs in git mode.
	Protocol string
	// URLFor overrides remote URL construction; tests point it at local
	// repositories so no network is involved.
	URLFor func(nameWithOwner string) string
}

// Run works through tasks and closes events when every task has produced
// exactly one EvDone. Skipped and already-current tasks are reported straight
// away; tasks abandoned because ctx was cancelled come back as Failed with
// Detail "interrupted".
func (r *Runner) Run(ctx context.Context, tasks []Task, events chan<- Event) {
	defer close(events)

	work := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		switch {
		case t.Skip:
			events <- doneEvent(Result{Name: t.Fork.Name, Status: Skipped, Detail: t.SkipReason, Log: t.Log})
		case r.Kind == KindSync && t.UpToDate:
			// Clone tasks are never short-circuited: cloning always has work
			// to do, however current the fork is.
			events <- doneEvent(Result{Name: t.Fork.Name, Status: UpToDate, Detail: "already up to date", Log: t.Log})
		default:
			work = append(work, t)
		}
	}
	if len(work) == 0 {
		return
	}

	n := r.Concurrency
	if n <= 0 {
		n = 4
	}
	if n > len(work) {
		n = len(work)
	}

	jobs := make(chan Task)
	var wg stdsync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				events <- Event{Name: t.Fork.Name, Kind: EvStarted}
				events <- doneEvent(r.execute(ctx, t, events))
			}
		}()
	}
	for _, t := range work {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
}

func doneEvent(r Result) Event {
	return Event{Name: r.Name, Kind: EvDone, Result: &r}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) execute(ctx context.Context, t Task, events chan<- Event) Result {
	start := r.now()
	res := Result{Name: t.Fork.Name}
	if err := ctx.Err(); err != nil {
		res.Status, res.Detail = Failed, interruptedDetail
		return res
	}
	switch {
	case r.Kind == KindClone:
		res = r.doClone(ctx, t, events)
	case r.GitMode:
		res = r.doGitSync(ctx, t, events)
	default:
		res = r.doSync(ctx, t, events)
	}
	res.Name = t.Fork.Name
	res.Duration = r.now().Sub(start)
	if ctx.Err() != nil && (res.Status == Failed || res.Status == Pending) && res.Message == "" {
		res.Status, res.Detail = Failed, interruptedDetail
		res.Err = ctx.Err()
	}
	return res
}

// doSync compares against the parent, then calls merge-upstream. Every step
// is streamed as an EvLog line and kept in Result.Log, so a row can be
// expanded while it is still running.
func (r *Runner) doSync(ctx context.Context, t Task, events chan<- Event) Result {
	f := t.Fork
	res := &Result{Name: f.Name}
	// Only this goroutine touches res, so no locking is needed.
	logf := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		if len(res.Log) < maxLogLines {
			res.Log = append(res.Log, line)
		}
		events <- Event{Name: f.Name, Kind: EvLog, Line: line}
	}

	parentOwner := github.ParentOwner(f.ParentNameWithOwner)
	parentBranch := f.ParentDefaultBranch
	if parentBranch == "" {
		parentBranch = f.DefaultBranch
	}

	if parentOwner != "" && f.DefaultBranch != "" {
		logf("compare upstream/%s…", parentBranch)
		cmp, err := r.Client.Compare(ctx, r.Org, f.Name, parentOwner, parentBranch, f.DefaultBranch)
		switch {
		case err != nil:
			// Compare is advisory; merge-upstream is the source of truth.
			logf("compare unavailable: %s", err.Error())
		default:
			res.Ahead, res.Behind = cmp.AheadBy, cmp.BehindBy
			logf("behind %d · ahead %d", cmp.BehindBy, cmp.AheadBy)
			if cmp.Status == "diverged" {
				res.Status = Diverged
				res.Detail = fmt.Sprintf("diverged · %d ahead, %d behind", cmp.AheadBy, cmp.BehindBy)
				for _, l := range divergedHelp(cmp.AheadBy, cmp.BehindBy, parentBranch) {
					logf("%s", l)
				}
				return *res
			}
		}
	}

	logf("POST merge-upstream (%s)", f.DefaultBranch)
	mr, err := r.Client.MergeUpstream(ctx, r.Org, f.Name, f.DefaultBranch)
	if err != nil {
		r.mergeFailure(res, err, parentBranch, logf)
		return *res
	}
	logf("200 %s", mr.MergeType)
	switch mr.MergeType {
	case "fast-forward":
		res.Status, res.MergeType = Synced, mr.MergeType
		res.Commits = res.Behind
		if res.Commits > 0 {
			res.Detail = fmt.Sprintf("fast-forward · %d commits", res.Commits)
		} else {
			res.Detail = "fast-forward"
		}
	case "merge":
		res.Status, res.MergeType, res.Detail = Synced, mr.MergeType, "merge"
		res.Commits = res.Behind
	case "none":
		res.Status, res.MergeType, res.Detail = UpToDate, mr.MergeType, "already up to date"
	default:
		res.Status, res.MergeType = Synced, mr.MergeType
		res.Detail = strings.TrimSpace(mr.MergeType)
		if res.Detail == "" {
			res.Detail = "synced"
		}
	}
	res.Message = mr.Message
	return *res
}

func (r *Runner) mergeFailure(res *Result, err error, parentBranch string, logf func(string, ...any)) {
	var ae *github.APIError
	if !errors.As(err, &ae) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			res.Status, res.Detail, res.Err = Failed, interruptedDetail, err
			return
		}
		res.Status, res.Detail, res.Err = Failed, "request failed", err
		res.Message = err.Error()
		logf("request failed: %s", err.Error())
		return
	}
	res.Err = ae
	res.Message = ae.Message
	logf("%d from merge-upstream", ae.Status)
	switch {
	case ae.Status == 409:
		res.Status, res.Detail = Diverged, "409 · diverged"
		for _, l := range divergedHelp(res.Ahead, res.Behind, parentBranch) {
			logf("%s", l)
		}
	case ae.Status == 422:
		res.Status = Failed
		res.Detail = "422 · " + shortReason(ae.Message)
		logf("%s", ae.Message)
	case ae.Status == 403:
		res.Status = Failed
		switch {
		case ae.SecondaryLimit:
			res.Detail = "403 · secondary rate limit"
		case ae.RateLimited:
			res.Detail = "403 · rate limited"
		default:
			res.Detail = "403 · permission denied"
		}
		logf("%s", ae.Message)
	default:
		res.Status = Failed
		res.Detail = fmt.Sprintf("%d · %s", ae.Status, shortReason(ae.Message))
		logf("%s", ae.Message)
	}
}

func divergedHelp(ahead, behind int, parentBranch string) []string {
	if parentBranch == "" {
		parentBranch = "main"
	}
	return []string{
		fmt.Sprintf("Your branch is %d ahead, %d behind upstream:%s.", ahead, behind, parentBranch),
		"merge-upstream cannot fast-forward a diverged branch.",
		"Resolve locally:",
		fmt.Sprintf("  git fetch upstream && git rebase upstream/%s", parentBranch),
	}
}

// shortReason condenses a GitHub message into a few words for the status row.
// The full message is always kept verbatim in Result.Message.
func shortReason(msg string) string {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "protected branch"), strings.Contains(l, "branch protection"):
		return "branch protection"
	case strings.Contains(l, "workflow"):
		return "workflow scope"
	case strings.Contains(l, "diverg"):
		return "diverged"
	case strings.Contains(l, "not permitted"), strings.Contains(l, "permission"):
		return "permission denied"
	}
	if i := strings.IndexAny(msg, ".\n"); i > 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 40 {
		msg = strings.TrimSpace(msg[:40]) + "…"
	}
	if msg == "" {
		return "failed"
	}
	return msg
}

// doClone clones the fork and wires the upstream remote, streaming git output.
func (r *Runner) doClone(ctx context.Context, t Task, events chan<- Event) Result {
	f := t.Fork
	res := Result{Name: f.Name}
	dir := filepath.Join(r.CloneDir, f.Name)
	existed := clone.IsRepo(dir)

	var mu stdsync.Mutex
	onLine := func(line string) {
		mu.Lock()
		if len(res.Log) < maxLogLines {
			res.Log = append(res.Log, line)
		}
		mu.Unlock()
		events <- Event{Name: f.Name, Kind: EvLog, Line: line}
	}
	onPercent := func(p float64) {
		events <- Event{Name: f.Name, Kind: EvProgress, Percent: p}
	}

	if existed {
		onLine("already cloned: " + dir)
	} else {
		onLine("git clone " + f.NameWithOwner + " → " + dir)
	}
	err := clone.Run(ctx, clone.Options{
		ForkURL:     r.cloneURL(f.NameWithOwner),
		UpstreamURL: r.cloneURL(f.ParentNameWithOwner),
		Dir:         dir,
		Full:        r.FullClone,
	}, onLine, onPercent)

	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			res.Status, res.Detail, res.Err = Failed, interruptedDetail, ctx.Err()
			return res
		}
		res.Status, res.Detail, res.Err = Failed, "git failed", err
		res.Message = err.Error()
		return res
	}
	res.Status = Synced
	if existed {
		res.Detail = "already cloned"
	} else {
		res.Detail = "cloned"
	}
	return res
}

func repoURL(nameWithOwner string) string {
	if nameWithOwner == "" {
		return ""
	}
	return "https://github.com/" + nameWithOwner + ".git"
}

// cloneURL is what `forkman clone` uses: https, which needs no ssh key just to
// read. Git-mode sync uses gitURL instead, because it also pushes.
func (r *Runner) cloneURL(nameWithOwner string) string {
	if r.URLFor != nil {
		return r.URLFor(nameWithOwner)
	}
	return repoURL(nameWithOwner)
}

// gitURL builds the remote URL for git mode from the configured protocol.
func (r *Runner) gitURL(nameWithOwner string) string {
	if r.URLFor != nil {
		return r.URLFor(nameWithOwner)
	}
	if nameWithOwner == "" {
		return ""
	}
	if r.Protocol == config.ProtoHTTPS {
		return repoURL(nameWithOwner)
	}
	return "git@github.com:" + nameWithOwner + ".git"
}

// doGitSync brings one fork up to date with plain git: fetch upstream, then
// fast-forward push to origin. A push over a git remote carries no OAuth
// scopes, so upstream commits touching .github/workflows go through where the
// API's merge-upstream would need the 'workflow' scope. It never force-pushes.
func (r *Runner) doGitSync(ctx context.Context, t Task, events chan<- Event) Result {
	f := t.Fork
	res := Result{Name: f.Name}
	forkBranch := f.DefaultBranch
	if forkBranch == "" {
		res.Status, res.Detail = Failed, "no default branch"
		return res
	}
	upBranch := f.ParentDefaultBranch
	if upBranch == "" {
		upBranch = forkBranch
	}
	o := clone.Options{
		ForkURL:     r.gitURL(f.NameWithOwner),
		UpstreamURL: r.gitURL(f.ParentNameWithOwner),
		Dir:         filepath.Join(r.CloneDir, f.Name),
		Full:        r.FullClone,
	}

	// clone.Stream drains git's pipes before it returns, so every callback
	// happens inside these calls and res needs no locking.
	logf := func(line string) {
		if len(res.Log) < maxLogLines {
			res.Log = append(res.Log, line)
		}
		events <- Event{Name: f.Name, Kind: EvLog, Line: line}
	}
	onPercent := func(p float64) {
		events <- Event{Name: f.Name, Kind: EvProgress, Percent: p}
	}
	failed := func(detail string, g clone.GitResult) Result {
		if ctx.Err() != nil {
			res.Status, res.Detail, res.Err = Failed, interruptedDetail, ctx.Err()
			return res
		}
		res.Status, res.Detail, res.Message = Failed, detail, g.Reason()
		for _, l := range g.Stderr {
			logf(l)
		}
		return res
	}

	if clone.IsRepo(o.Dir) {
		logf("local clone: " + o.Dir)
	} else {
		logf("git clone " + f.NameWithOwner + " → " + o.Dir)
	}
	if err := clone.EnsureClone(ctx, o, logf, onPercent); err != nil {
		if ctx.Err() != nil {
			res.Status, res.Detail, res.Err = Failed, interruptedDetail, ctx.Err()
			return res
		}
		res.Status, res.Detail, res.Err = Failed, "clone failed", err
		res.Message = err.Error()
		return res
	}
	// forkman owns this folder, so keep origin on the configured protocol even
	// when the clone was made earlier over a different one.
	gitStep(logf, "remote", "set-url", "origin", o.ForkURL)
	if g := clone.Git(ctx, o, "remote", "set-url", "origin", o.ForkURL); !g.OK() {
		logf("could not set origin url: " + g.Reason())
	}
	for _, remote := range []string{"upstream", "origin"} {
		gitStep(logf, "fetch", remote, "--prune")
		if err := clone.Stream(ctx, o, logf, onPercent, "fetch", "--progress", remote, "--prune"); err != nil {
			if ctx.Err() != nil {
				res.Status, res.Detail, res.Err = Failed, interruptedDetail, ctx.Err()
				return res
			}
			res.Status, res.Detail, res.Err = Failed, "fetch "+remote+" failed", err
			res.Message = err.Error()
			return res
		}
	}

	originRef := "refs/remotes/origin/" + forkBranch
	upRef := "refs/remotes/upstream/" + upBranch

	switch anc := clone.Git(ctx, o, "merge-base", "--is-ancestor", originRef, upRef); anc.Code {
	case 0: // origin is behind or equal: a fast-forward is possible
	case 1:
		res.Ahead, res.Behind = revCounts(ctx, o, originRef, upRef)
		res.Status = Diverged
		res.Detail = fmt.Sprintf("diverged · %d ahead, %d behind", res.Ahead, res.Behind)
		for _, l := range divergedHelp(res.Ahead, res.Behind, upBranch) {
			logf(l)
		}
		return res
	default:
		return failed("ancestry check failed", anc)
	}

	count := clone.Git(ctx, o, "rev-list", "--count", originRef+".."+upRef)
	if !count.OK() {
		return failed("commit count failed", count)
	}
	n, err := strconv.Atoi(count.Stdout)
	if err != nil {
		res.Status, res.Detail, res.Err = Failed, "commit count failed", err
		return res
	}
	logf(fmt.Sprintf("behind %d · ahead 0", n))
	if n == 0 {
		res.Status, res.Detail = UpToDate, "already up to date"
		return res
	}

	// Fast-forward push straight from the fetched upstream ref: no working
	// tree checkout is needed, and a non-fast-forward is refused by git.
	gitStep(logf, "push", "origin", upRef+":refs/heads/"+forkBranch)
	// mark is set after the command line so only git's own output is searched
	// for the rejection reason.
	mark := len(res.Log)
	if err := clone.Stream(ctx, o, logf, onPercent, "push", "origin", upRef+":refs/heads/"+forkBranch); err != nil {
		if ctx.Err() != nil {
			res.Status, res.Detail, res.Err = Failed, interruptedDetail, ctx.Err()
			return res
		}
		res.Status, res.Err = Failed, err
		if reason := clone.Reason(res.Log[mark:]); reason != "" {
			res.Detail, res.Message = "push rejected · "+shortReason(reason), reason
		} else {
			res.Detail, res.Message = "push rejected", err.Error()
		}
		return res
	}

	gitStep(logf, "fetch", "origin")
	if g := clone.Git(ctx, o, "fetch", "origin"); !g.OK() {
		logf("fetch origin failed: " + g.Reason())
	}
	updateCheckout(ctx, o, forkBranch, upRef, logf)

	res.Status, res.MergeType = Synced, "fast-forward"
	res.Commits, res.Behind = n, n
	res.Detail = fmt.Sprintf("fast-forward · %d commits", n)
	return res
}

// gitStep records the git command a worker is about to run, so an expanded
// row reads as the transcript of the work. --progress is an implementation
// detail and is left out.
func gitStep(logf func(string), args ...string) {
	kept := make([]string, 0, len(args)+1)
	kept = append(kept, "git")
	for _, a := range args {
		if a == "--progress" {
			continue
		}
		kept = append(kept, a)
	}
	logf(strings.Join(kept, " "))
}

// revCounts returns how far origin is ahead of, and behind, upstream.
func revCounts(ctx context.Context, o clone.Options, originRef, upRef string) (ahead, behind int) {
	g := clone.Git(ctx, o, "rev-list", "--left-right", "--count", originRef+"..."+upRef)
	if !g.OK() {
		return 0, 0
	}
	f := strings.Fields(g.Stdout)
	if len(f) != 2 {
		return 0, 0
	}
	ahead, _ = strconv.Atoi(f[0])
	behind, _ = strconv.Atoi(f[1])
	return ahead, behind
}

// updateCheckout fast-forwards the working tree too, but only when that is
// safe: on the target branch with nothing uncommitted. Otherwise it just says
// so, because the push already succeeded.
func updateCheckout(ctx context.Context, o clone.Options, branch, upRef string, logf func(string)) {
	head := clone.Git(ctx, o, "rev-parse", "--abbrev-ref", "HEAD")
	st := clone.Git(ctx, o, "status", "--porcelain")
	if !head.OK() || !st.OK() || head.Stdout != branch || st.Stdout != "" {
		logf("local checkout not updated (branch/dirty)")
		return
	}
	if g := clone.Git(ctx, o, "merge", "--ff-only", upRef); !g.OK() {
		logf("local checkout not updated: " + g.Reason())
		return
	}
	logf("local checkout fast-forwarded")
}
