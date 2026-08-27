package sync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forkman/internal/config"
	"forkman/internal/github"
)

// forkServer serves compare and merge-upstream per repository name.
type forkServer struct {
	compare map[string]string
	merge   map[string]struct {
		status int
		body   string
	}
	hits chan string
}

func (fs *forkServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" {
		http.NotFound(w, r)
		return
	}
	repo := parts[2]
	fs.hits <- r.Method + " " + repo + " " + parts[3]
	switch {
	case parts[3] == "compare":
		body, ok := fs.compare[repo]
		if !ok {
			body = `{"ahead_by":0,"behind_by":7,"status":"behind"}`
		}
		fmt.Fprint(w, body)
	case parts[3] == "merge-upstream":
		m, ok := fs.merge[repo]
		if !ok {
			m.status, m.body = 200, `{"merge_type":"fast-forward"}`
		}
		w.WriteHeader(m.status)
		fmt.Fprint(w, m.body)
	default:
		http.NotFound(w, r)
	}
}

func newRunner(t *testing.T, fs *forkServer) *Runner {
	t.Helper()
	fs.hits = make(chan string, 256)
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	fixed := time.Unix(1700000000, 0)
	c := github.New("tok", "test",
		github.WithBaseURL(srv.URL),
		github.WithHTTPClient(srv.Client()),
		github.WithClock(func() time.Time { return fixed }, func(context.Context, time.Duration) error { return nil }),
	)
	return &Runner{Client: c, Concurrency: 3, Kind: KindSync, Org: "acme", Now: func() time.Time { return fixed }}
}

// collect drains events until the channel is closed and returns the results
// plus a per-name EvDone count.
func collect(ch <-chan Event) ([]Result, map[string]int, map[string]int) {
	var results []Result
	done := map[string]int{}
	started := map[string]int{}
	for ev := range ch {
		switch ev.Kind {
		case EvStarted:
			started[ev.Name]++
		case EvDone:
			done[ev.Name]++
			if ev.Result != nil {
				results = append(results, *ev.Result)
			}
		}
	}
	return results, done, started
}

func TestRunnerOneDonePerTask(t *testing.T) {
	fs := &forkServer{
		compare: map[string]string{
			"diverged-cmp": `{"ahead_by":3,"behind_by":12,"status":"diverged"}`,
			"ff":           `{"ahead_by":0,"behind_by":1181,"status":"behind"}`,
		},
		merge: map[string]struct {
			status int
			body   string
		}{
			"ff":         {200, `{"merge_type":"fast-forward"}`},
			"merged":     {200, `{"merge_type":"merge"}`},
			"nochange":   {200, `{"merge_type":"none"}`},
			"conflict":   {409, `{"message":"There are merge conflicts"}`},
			"protected":  {422, `{"message":"Protected branch update failed for refs/heads/main"}`},
			"nopermit":   {403, `{"message":"Resource not accessible by integration"}`},
			"secondlim":  {403, `{"message":"You have exceeded a secondary rate limit"}`},
			"weirdstate": {418, `{"message":"I am a teapot"}`},
		},
	}
	r := newRunner(t, fs)

	cfg := &config.Config{Excluded: []string{"banned"}}
	forks := []github.Fork{
		fork("ff"), fork("merged"), fork("nochange"), fork("conflict"),
		fork("protected"), fork("nopermit"), fork("secondlim"), fork("weirdstate"),
		fork("diverged-cmp"),
		fork("banned"),
		fork("archived", func(f *github.Fork) { f.Archived = true }),
		fork("current", func(f *github.Fork) { f.ParentHeadOID = "fork-oid" }),
	}
	tasks := Plan(forks, cfg)

	events := make(chan Event, 1)
	go r.Run(context.Background(), tasks, events)
	results, done, started := collect(events)

	if len(results) != len(tasks) {
		t.Fatalf("got %d results, want %d", len(results), len(tasks))
	}
	for _, tk := range tasks {
		if done[tk.Fork.Name] != 1 {
			t.Errorf("%s: %d EvDone events, want exactly 1", tk.Fork.Name, done[tk.Fork.Name])
		}
	}
	for _, name := range []string{"banned", "archived", "current"} {
		if started[name] != 0 {
			t.Errorf("%s: pre-classified task should not emit EvStarted", name)
		}
	}

	got := map[string]Result{}
	for _, res := range results {
		got[res.Name] = res
	}
	want := map[string]struct {
		status  Status
		detail  string
		message string
	}{
		"ff":           {Synced, "fast-forward · 1181 commits", ""},
		"merged":       {Synced, "merge", ""},
		"nochange":     {UpToDate, "already up to date", ""},
		"conflict":     {Diverged, "409 · diverged", "There are merge conflicts"},
		"protected":    {Failed, "422 · branch protection", "Protected branch update failed for refs/heads/main"},
		"nopermit":     {Failed, "403 · permission denied", "Resource not accessible by integration"},
		"secondlim":    {Failed, "403 · secondary rate limit", "You have exceeded a secondary rate limit"},
		"weirdstate":   {Failed, "418 · I am a teapot", "I am a teapot"},
		"diverged-cmp": {Diverged, "diverged · 3 ahead, 12 behind", ""},
		"banned":       {Skipped, "excluded by config", ""},
		"archived":     {Skipped, "archived", ""},
		"current":      {UpToDate, "already up to date", ""},
	}
	for name, w := range want {
		res, ok := got[name]
		if !ok {
			t.Errorf("no result for %s", name)
			continue
		}
		if res.Status != w.status {
			t.Errorf("%s: Status = %v, want %v", name, res.Status, w.status)
		}
		if res.Detail != w.detail {
			t.Errorf("%s: Detail = %q, want %q", name, res.Detail, w.detail)
		}
		if w.message != "" && res.Message != w.message {
			t.Errorf("%s: Message = %q, want verbatim %q", name, res.Message, w.message)
		}
	}

	if c := got["ff"].Commits; c != 1181 {
		t.Errorf("ff Commits = %d, want 1181 (from compare behind_by)", c)
	}
	if log := got["diverged-cmp"].Log; len(log) != 4 ||
		log[0] != "Your branch is 3 ahead, 12 behind upstream:main." ||
		!strings.Contains(log[3], "git fetch upstream && git rebase upstream/main") {
		t.Errorf("diverged log = %#v", got["diverged-cmp"].Log)
	}
	if len(got["conflict"].Log) == 0 {
		t.Error("409 result should carry resolution instructions")
	}

	// A compare that reports "diverged" must not attempt a merge.
	close(fs.hits)
	for h := range fs.hits {
		if h == "POST diverged-cmp merge-upstream" {
			t.Error("merge attempted on a diverged repo")
		}
	}
}

func TestRunnerClosesChannel(t *testing.T) {
	r := newRunner(t, &forkServer{})
	events := make(chan Event, 32)
	go r.Run(context.Background(), Plan([]github.Fork{fork("a")}, nil), events)
	for range events {
	}
	// A second receive on a closed channel returns immediately.
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("channel not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed")
	}
}

func TestRunnerEmptyPlan(t *testing.T) {
	r := newRunner(t, &forkServer{})
	events := make(chan Event)
	go r.Run(context.Background(), nil, events)
	if _, done, _ := collect(events); len(done) != 0 {
		t.Errorf("events for an empty plan: %v", done)
	}
}

func TestRunnerCancellation(t *testing.T) {
	release := make(chan struct{})
	fs := &forkServer{hits: make(chan string, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, `{"ahead_by":0,"behind_by":1,"status":"behind"}`)
	}))
	defer srv.Close()
	_ = fs

	fixed := time.Unix(1700000000, 0)
	c := github.New("tok", "test", github.WithBaseURL(srv.URL), github.WithHTTPClient(srv.Client()),
		github.WithClock(func() time.Time { return fixed }, func(context.Context, time.Duration) error { return nil }))
	r := &Runner{Client: c, Concurrency: 2, Kind: KindSync, Org: "acme"}

	forks := []github.Fork{fork("a"), fork("b"), fork("c"), fork("d"), fork("e")}
	forks = append(forks, fork("skipme", func(f *github.Fork) { f.Archived = true }))
	tasks := Plan(forks, nil)

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan Event, 256)
	go r.Run(ctx, tasks, events)

	// Let the pool pick up work, then cancel and unblock the server.
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(release)

	results, done, _ := collect(events)
	if len(results) != len(tasks) {
		t.Fatalf("got %d results, want one per task (%d)", len(results), len(tasks))
	}
	for _, tk := range tasks {
		if done[tk.Fork.Name] != 1 {
			t.Errorf("%s: %d EvDone events, want exactly 1", tk.Fork.Name, done[tk.Fork.Name])
		}
	}
	interrupted := 0
	for _, res := range results {
		if res.Status == Failed && res.Detail == interruptedDetail {
			interrupted++
		}
	}
	if interrupted == 0 {
		t.Error("no task reported as interrupted after cancellation")
	}
	s := Summarize(results)
	if s.Interrupted == 0 {
		t.Errorf("Summary.Interrupted = 0, want > 0 (%+v)", s)
	}
	if got := ExitCode(s, true); got != 130 {
		t.Errorf("ExitCode = %d, want 130", got)
	}
}

func TestRunnerCancelledBeforeStart(t *testing.T) {
	r := newRunner(t, &forkServer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tasks := Plan([]github.Fork{fork("a"), fork("b")}, nil)
	events := make(chan Event, 32)
	go r.Run(ctx, tasks, events)
	results, done, _ := collect(events)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for name, n := range done {
		if n != 1 {
			t.Errorf("%s: %d EvDone events, want 1", name, n)
		}
	}
	for _, res := range results {
		if res.Status != Failed || res.Detail != interruptedDetail {
			t.Errorf("%s: got %v/%q, want failed/interrupted", res.Name, res.Status, res.Detail)
		}
	}
}

func TestShortReason(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Protected branch update failed for refs/heads/main", "branch protection"},
		{`refusing to allow an OAuth App to create or update workflow ".github/workflows/ci.yml" without "workflow" scope`, "workflow scope"},
		{"There are merge conflicts", "There are merge conflicts"},
		{"Something went badly wrong in a way nobody predicted", "Something went badly wrong in a way nobo…"},
		{"", "failed"},
		{"Short. Trailing sentence ignored.", "Short"},
	}
	for _, tc := range tests {
		if got := shortReason(tc.in); got != tc.want {
			t.Errorf("shortReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRepoURL(t *testing.T) {
	if got := repoURL("acme/tempo"); got != "https://github.com/acme/tempo.git" {
		t.Errorf("repoURL = %q", got)
	}
	if got := repoURL(""); got != "" {
		t.Errorf("repoURL(\"\") = %q", got)
	}
}
