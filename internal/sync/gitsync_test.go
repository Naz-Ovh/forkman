package sync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forkman/internal/config"
	"forkman/internal/github"
)

// requireGit skips a test when git is unavailable.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// isolateGit keeps the fixture repositories away from the developer's own git
// configuration and supplies a committer identity.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "forkman test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "forkman test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a pair of bare repositories on disk: origin is a fork of upstream
// at the first commit, so no network is involved anywhere in these tests.
type fixture struct {
	root, seed, upstream, origin, cloneDir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireGit(t)
	isolateGit(t)
	root := t.TempDir()
	f := &fixture{
		root:     root,
		seed:     filepath.Join(root, "seed"),
		upstream: filepath.Join(root, "upstream.git"),
		origin:   filepath.Join(root, "origin.git"),
		cloneDir: filepath.Join(root, "forks"),
	}
	gitCmd(t, root, "init", "-b", "main", "seed")
	writeFile(t, filepath.Join(f.seed, "README.md"), "one\n")
	gitCmd(t, f.seed, "add", ".")
	gitCmd(t, f.seed, "commit", "-m", "one")
	gitCmd(t, root, "clone", "--bare", f.seed, f.upstream)
	gitCmd(t, root, "clone", "--bare", f.upstream, f.origin)
	return f
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// advanceUpstream puts n new commits on upstream's main.
func (f *fixture) advanceUpstream(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		writeFile(t, filepath.Join(f.seed, fmt.Sprintf("up%d.txt", i)), "upstream\n")
		gitCmd(t, f.seed, "add", ".")
		gitCmd(t, f.seed, "commit", "-m", fmt.Sprintf("upstream %d", i))
	}
	gitCmd(t, f.seed, "push", f.upstream, "main")
}

// advanceOrigin puts a commit only the fork has, which makes it diverge.
func (f *fixture) advanceOrigin(t *testing.T) {
	t.Helper()
	work := filepath.Join(f.root, "fork-work")
	gitCmd(t, f.root, "clone", f.origin, work)
	writeFile(t, filepath.Join(work, "fork.txt"), "fork\n")
	gitCmd(t, work, "add", ".")
	gitCmd(t, work, "commit", "-m", "fork only")
	gitCmd(t, work, "push", "origin", "main")
}

// blockPushes installs a pre-receive hook on origin that refuses everything.
func (f *fixture) blockPushes(t *testing.T) {
	t.Helper()
	hook := filepath.Join(f.origin, "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\necho 'pushes are blocked by policy' >&2\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) head(t *testing.T, repo string) string {
	t.Helper()
	return gitCmd(t, repo, "rev-parse", "main")
}

func (f *fixture) localDir() string { return filepath.Join(f.cloneDir, "repo") }

func (f *fixture) runner() *Runner {
	return &Runner{
		Concurrency: 1,
		Kind:        KindSync,
		CloneDir:    f.cloneDir,
		FullClone:   true, // a partial clone over the local transport buys nothing
		Org:         "org",
		GitMode:     true,
		Protocol:    config.ProtoSSH,
		URLFor: func(nameWithOwner string) string {
			switch nameWithOwner {
			case "org/repo":
				return f.origin
			case "up/repo":
				return f.upstream
			}
			return ""
		},
	}
}

func (f *fixture) task() Task {
	return Task{Fork: github.Fork{
		Name:                "repo",
		NameWithOwner:       "org/repo",
		DefaultBranch:       "main",
		HasParent:           true,
		ViewerPermission:    "WRITE",
		ParentNameWithOwner: "up/repo",
		ParentDefaultBranch: "main",
	}}
}

// runOne drives the runner over a single task and returns its result.
func runOne(t *testing.T, r *Runner, task Task) Result {
	t.Helper()
	events := make(chan Event, 1024)
	type out struct {
		results []Result
		logs    int
	}
	ch := make(chan out, 1)
	go func() {
		var o out
		for ev := range events {
			switch ev.Kind {
			case EvDone:
				if ev.Result != nil {
					o.results = append(o.results, *ev.Result)
				}
			case EvLog:
				o.logs++
			}
		}
		ch <- o
	}()
	r.Run(context.Background(), []Task{task}, events)
	o := <-ch
	if len(o.results) != 1 {
		t.Fatalf("got %d results, want 1", len(o.results))
	}
	if o.logs == 0 {
		t.Error("no EvLog events; git output was not streamed")
	}
	return o.results[0]
}

func TestGitSyncFastForward(t *testing.T) {
	f := newFixture(t)
	f.advanceUpstream(t, 2)
	want := f.head(t, f.upstream)

	res := runOne(t, f.runner(), f.task())
	if res.Status != Synced {
		t.Fatalf("Status = %v (%s), want synced\n%s", res.Status, res.Detail, strings.Join(res.Log, "\n"))
	}
	if res.Commits != 2 || res.Behind != 2 {
		t.Errorf("Commits/Behind = %d/%d, want 2/2", res.Commits, res.Behind)
	}
	if res.MergeType != "fast-forward" {
		t.Errorf("MergeType = %q", res.MergeType)
	}
	if res.Detail != "fast-forward · 2 commits" {
		t.Errorf("Detail = %q", res.Detail)
	}
	if got := f.head(t, f.origin); got != want {
		t.Errorf("origin main = %s, want %s", got, want)
	}
	// The working copy is on main and clean, so it follows along.
	if got := gitCmd(t, f.localDir(), "rev-parse", "HEAD"); got != want {
		t.Errorf("local HEAD = %s, want %s", got, want)
	}
	if !strings.Contains(strings.Join(res.Log, "\n"), "local checkout fast-forwarded") {
		t.Errorf("log does not mention the local fast-forward:\n%s", strings.Join(res.Log, "\n"))
	}
}

func TestGitSyncUpToDate(t *testing.T) {
	f := newFixture(t)
	res := runOne(t, f.runner(), f.task())
	if res.Status != UpToDate {
		t.Fatalf("Status = %v (%s), want up to date", res.Status, res.Detail)
	}
	if res.Commits != 0 {
		t.Errorf("Commits = %d, want 0", res.Commits)
	}
	if res.Detail != "already up to date" {
		t.Errorf("Detail = %q", res.Detail)
	}
}

func TestGitSyncDiverged(t *testing.T) {
	f := newFixture(t)
	f.advanceUpstream(t, 2)
	f.advanceOrigin(t)
	before := f.head(t, f.origin)

	res := runOne(t, f.runner(), f.task())
	if res.Status != Diverged {
		t.Fatalf("Status = %v (%s), want diverged", res.Status, res.Detail)
	}
	if res.Ahead != 1 || res.Behind != 2 {
		t.Errorf("ahead/behind = %d/%d, want 1/2", res.Ahead, res.Behind)
	}
	if res.Detail != "diverged · 1 ahead, 2 behind" {
		t.Errorf("Detail = %q", res.Detail)
	}
	if !strings.Contains(strings.Join(res.Log, "\n"), "git rebase upstream/main") {
		t.Errorf("log lacks the resolution instructions:\n%s", strings.Join(res.Log, "\n"))
	}
	if got := f.head(t, f.origin); got != before {
		t.Error("origin was rewritten; a diverged fork must never be force-pushed")
	}
}

func TestGitSyncPushRejected(t *testing.T) {
	f := newFixture(t)
	f.advanceUpstream(t, 1)
	f.blockPushes(t)
	before := f.head(t, f.origin)

	res := runOne(t, f.runner(), f.task())
	if res.Status != Failed {
		t.Fatalf("Status = %v (%s), want failed", res.Status, res.Detail)
	}
	if !strings.HasPrefix(res.Detail, "push rejected") {
		t.Errorf("Detail = %q, want it to start with \"push rejected\"", res.Detail)
	}
	if !strings.Contains(strings.Join(res.Log, "\n"), "blocked by policy") {
		t.Errorf("hook message missing from the log:\n%s", strings.Join(res.Log, "\n"))
	}
	if !strings.Contains(res.Message, "blocked by policy") {
		t.Errorf("Message = %q, want the hook message", res.Message)
	}
	if got := f.head(t, f.origin); got != before {
		t.Error("origin moved even though the push was rejected")
	}
}

func TestGitSyncLeavesDirtyCheckoutAlone(t *testing.T) {
	f := newFixture(t)
	f.advanceUpstream(t, 1)
	// Pre-clone so the working tree exists, then dirty it.
	gitCmd(t, f.root, "clone", f.origin, f.localDir())
	localBefore := gitCmd(t, f.localDir(), "rev-parse", "HEAD")
	writeFile(t, filepath.Join(f.localDir(), "scratch.txt"), "work in progress\n")

	res := runOne(t, f.runner(), f.task())
	if res.Status != Synced {
		t.Fatalf("Status = %v (%s), want synced", res.Status, res.Detail)
	}
	if got := f.head(t, f.origin); got != f.head(t, f.upstream) {
		t.Error("origin was not fast-forwarded")
	}
	if got := gitCmd(t, f.localDir(), "rev-parse", "HEAD"); got != localBefore {
		t.Error("a dirty working tree must not be moved")
	}
	if !strings.Contains(strings.Join(res.Log, "\n"), "local checkout not updated (branch/dirty)") {
		t.Errorf("log does not explain the skipped checkout update:\n%s", strings.Join(res.Log, "\n"))
	}
}

func TestGitURL(t *testing.T) {
	tests := []struct {
		protocol string
		want     string
	}{
		{config.ProtoSSH, "git@github.com:acme/tempo.git"},
		{config.ProtoHTTPS, "https://github.com/acme/tempo.git"},
		{"", "git@github.com:acme/tempo.git"},
	}
	for _, tc := range tests {
		r := &Runner{Protocol: tc.protocol}
		if got := r.gitURL("acme/tempo"); got != tc.want {
			t.Errorf("gitURL(%q) = %q, want %q", tc.protocol, got, tc.want)
		}
	}
	if got := (&Runner{}).gitURL(""); got != "" {
		t.Errorf("gitURL(\"\") = %q, want empty", got)
	}
	// clone keeps using https regardless of the git-mode protocol.
	if got := (&Runner{Protocol: config.ProtoSSH}).cloneURL("acme/tempo"); got != "https://github.com/acme/tempo.git" {
		t.Errorf("cloneURL = %q", got)
	}
}

func TestGitSyncNoDefaultBranch(t *testing.T) {
	r := &Runner{GitMode: true, Concurrency: 1, CloneDir: t.TempDir(), URLFor: func(string) string { return "" }}
	task := Task{Fork: github.Fork{Name: "repo", NameWithOwner: "org/repo", HasParent: true, ViewerPermission: "WRITE"}}
	events := make(chan Event, 16)
	var res []Result
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if ev.Kind == EvDone && ev.Result != nil {
				res = append(res, *ev.Result)
			}
		}
	}()
	r.Run(context.Background(), []Task{task}, events)
	<-done
	if len(res) != 1 || res[0].Status != Failed || res[0].Detail != "no default branch" {
		t.Errorf("results = %+v, want one failure with \"no default branch\"", res)
	}
}
