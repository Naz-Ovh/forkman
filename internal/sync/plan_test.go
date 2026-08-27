package sync

import (
	"strings"
	"testing"

	"github.com/naz-ovh/forkman/internal/config"
	"github.com/naz-ovh/forkman/internal/github"
)

func fork(name string, mut ...func(*github.Fork)) github.Fork {
	f := github.Fork{
		Name:                name,
		NameWithOwner:       "acme/" + name,
		ViewerPermission:    "WRITE",
		DefaultBranch:       "main",
		HeadOID:             "fork-oid",
		HasParent:           true,
		ParentNameWithOwner: "upstream/" + name,
		ParentDefaultBranch: "main",
		ParentHeadOID:       "parent-oid",
	}
	for _, m := range mut {
		m(&f)
	}
	return f
}

func TestPlanClassification(t *testing.T) {
	cfg := &config.Config{Excluded: []string{"banned", "test-*"}}
	forks := []github.Fork{
		fork("zeta"),
		fork("banned"),
		fork("test-thing"),
		fork("archived", func(f *github.Fork) { f.Archived = true }),
		fork("orphan", func(f *github.Fork) { f.HasParent = false }),
		fork("readonly", func(f *github.Fork) { f.ViewerPermission = "READ" }),
		fork("triage", func(f *github.Fork) { f.ViewerPermission = "TRIAGE" }),
		fork("current", func(f *github.Fork) { f.ParentHeadOID = "fork-oid" }),
		fork("admin", func(f *github.Fork) { f.ViewerPermission = "ADMIN" }),
		fork("maintain", func(f *github.Fork) { f.ViewerPermission = "MAINTAIN" }),
		fork("noheads", func(f *github.Fork) { f.HeadOID, f.ParentHeadOID = "", "" }),
	}
	tasks := Plan(forks, cfg, KindSync)

	if len(tasks) != len(forks) {
		t.Fatalf("Plan returned %d tasks, want %d", len(tasks), len(forks))
	}
	// Sorted by name ascending, stable.
	for i := 1; i < len(tasks); i++ {
		if tasks[i-1].Fork.Name > tasks[i].Fork.Name {
			t.Fatalf("order not ascending at %d: %q then %q", i, tasks[i-1].Fork.Name, tasks[i].Fork.Name)
		}
	}

	byName := map[string]Task{}
	for _, tk := range tasks {
		byName[tk.Fork.Name] = tk
	}
	want := map[string]struct {
		skip     bool
		reason   string
		upToDate bool
	}{
		"banned":     {true, "excluded by config", false},
		"test-thing": {true, "excluded by config", false},
		"archived":   {true, "archived", false},
		"orphan":     {true, "no parent", false},
		"readonly":   {true, "read-only (READ) · cannot push", false},
		"triage":     {true, "read-only (TRIAGE) · cannot push", false},
		"current":    {false, "", true},
		"zeta":       {false, "", false},
		"admin":      {false, "", false},
		"maintain":   {false, "", false},
		"noheads":    {false, "", false},
	}
	for name, w := range want {
		got, ok := byName[name]
		if !ok {
			t.Fatalf("missing task %q", name)
		}
		if got.Skip != w.skip || got.SkipReason != w.reason || got.UpToDate != w.upToDate {
			t.Errorf("%s: got skip=%v reason=%q upToDate=%v, want skip=%v reason=%q upToDate=%v",
				name, got.Skip, got.SkipReason, got.UpToDate, w.skip, w.reason, w.upToDate)
		}
	}
}

func TestPlanRulePrecedence(t *testing.T) {
	// An excluded, archived, parentless, read-only repo reports "excluded"
	// because exclusion is checked first.
	cfg := &config.Config{Excluded: []string{"everything"}}
	f := fork("everything", func(f *github.Fork) {
		f.Archived, f.HasParent, f.ViewerPermission = true, false, "READ"
	})
	tasks := Plan([]github.Fork{f}, cfg, KindSync)
	if tasks[0].SkipReason != "excluded by config" {
		t.Errorf("SkipReason = %q, want %q", tasks[0].SkipReason, "excluded by config")
	}

	// Archived beats parentless and permission.
	f2 := fork("arch", func(f *github.Fork) {
		f.Archived, f.HasParent, f.ViewerPermission = true, false, "READ"
	})
	if got := Plan([]github.Fork{f2}, &config.Config{}, KindSync)[0].SkipReason; got != "archived" {
		t.Errorf("SkipReason = %q, want %q", got, "archived")
	}

	// Parentless beats permission.
	f3 := fork("orph", func(f *github.Fork) { f.HasParent, f.ViewerPermission = false, "READ" })
	if got := Plan([]github.Fork{f3}, &config.Config{}, KindSync)[0].SkipReason; got != "no parent" {
		t.Errorf("SkipReason = %q, want %q", got, "no parent")
	}
}

func TestPlanNilConfig(t *testing.T) {
	tasks := Plan([]github.Fork{fork("a")}, nil, KindSync)
	if len(tasks) != 1 || tasks[0].Skip {
		t.Errorf("Plan with nil config = %+v", tasks)
	}
}

// A read-only fork can still be cloned: the permission rule exists because
// syncing pushes, and cloning does not.
func TestPlanPermissionRuleIsSyncOnly(t *testing.T) {
	f := fork("readonly", func(f *github.Fork) { f.ViewerPermission = "READ" })

	sync := Plan([]github.Fork{f}, nil, KindSync)[0]
	if !sync.Skip {
		t.Fatal("sync must skip a fork the viewer cannot push to")
	}
	if sync.SkipReason != "read-only (READ) · cannot push" {
		t.Errorf("SkipReason = %q", sync.SkipReason)
	}
	if len(sync.Log) != 2 ||
		!strings.Contains(sync.Log[0], "viewerPermission: READ") ||
		!strings.Contains(sync.Log[1], "--exclude-add readonly") {
		t.Errorf("skip explanation = %#v", sync.Log)
	}

	cl := Plan([]github.Fork{f}, nil, KindClone)[0]
	if cl.Skip {
		t.Errorf("clone must not skip a read-only fork: %q", cl.SkipReason)
	}
	if cl.UpToDate {
		t.Error("clone tasks are never up to date")
	}
}

// An unset viewerPermission is still not writable, but must not render as an
// empty pair of brackets.
func TestPlanUnknownPermission(t *testing.T) {
	f := fork("mystery", func(f *github.Fork) { f.ViewerPermission = "" })
	got := Plan([]github.Fork{f}, nil, KindSync)[0]
	if !got.Skip || got.SkipReason != "read-only (UNKNOWN) · cannot push" {
		t.Errorf("skip=%v reason=%q", got.Skip, got.SkipReason)
	}
}

func TestPlanUpToDateIsSyncOnly(t *testing.T) {
	f := fork("current", func(f *github.Fork) {
		f.HeadOID, f.ParentHeadOID = "abcdef1234567890", "abcdef1234567890"
	})

	sync := Plan([]github.Fork{f}, nil, KindSync)[0]
	if !sync.UpToDate {
		t.Fatal("equal head OIDs must be up to date for sync")
	}
	if len(sync.Log) != 1 || sync.Log[0] != "fork abcdef1 == upstream abcdef1" {
		t.Errorf("up-to-date explanation = %#v", sync.Log)
	}

	cl := Plan([]github.Fork{f}, nil, KindClone)[0]
	if cl.UpToDate || cl.Skip {
		t.Errorf("clone task = skip:%v upToDate:%v, want work to be done", cl.Skip, cl.UpToDate)
	}
}

func TestPlanCloneStillSkipsExcludedArchivedAndOrphans(t *testing.T) {
	cfg := &config.Config{Excluded: []string{"drop-*"}}
	forks := []github.Fork{
		fork("drop-me"),
		fork("archived", func(f *github.Fork) { f.Archived = true }),
		fork("orphan", func(f *github.Fork) { f.HasParent = false }),
	}
	want := map[string]string{
		"drop-me":  "excluded by config",
		"archived": "archived",
		"orphan":   "no parent",
	}
	for _, tk := range Plan(forks, cfg, KindClone) {
		if !tk.Skip || tk.SkipReason != want[tk.Fork.Name] {
			t.Errorf("%s: skip=%v reason=%q, want %q", tk.Fork.Name, tk.Skip, tk.SkipReason, want[tk.Fork.Name])
		}
		if len(tk.Log) == 0 {
			t.Errorf("%s: skipped task has no explanation", tk.Fork.Name)
		}
	}
}

func TestPlanExcludedNamesThePattern(t *testing.T) {
	cfg := &config.Config{Excluded: []string{"Test-*"}}
	got := Plan([]github.Fork{fork("test-thing")}, cfg, KindSync)[0]
	if len(got.Log) == 0 || got.Log[0] != `excluded by config pattern "Test-*"` {
		t.Errorf("explanation = %#v", got.Log)
	}
}

func TestStatusString(t *testing.T) {
	want := map[Status]string{
		Pending: "pending", Running: "running", Synced: "synced",
		UpToDate: "up to date", Skipped: "skipped", Diverged: "diverged", Failed: "failed",
		Status(99): "unknown",
	}
	for s, w := range want {
		if got := s.String(); got != w {
			t.Errorf("Status(%d).String() = %q, want %q", s, got, w)
		}
	}
}

func TestSummarizeAndExitCode(t *testing.T) {
	results := []Result{
		{Status: Synced}, {Status: Synced},
		{Status: UpToDate},
		{Status: Skipped},
		{Status: Diverged},
		{Status: Failed, Detail: "422 · branch protection"},
		{Status: Failed, Detail: interruptedDetail},
	}
	s := Summarize(results)
	want := Summary{Total: 7, Synced: 2, UpToDate: 1, Skipped: 1, Diverged: 1, Failed: 1, Interrupted: 1}
	if s != want {
		t.Errorf("Summarize = %+v, want %+v", s, want)
	}
	if got := ExitCode(s, false); got != 130 {
		t.Errorf("ExitCode with interrupted result = %d, want 130", got)
	}

	clean := Summarize([]Result{{Status: Synced}, {Status: UpToDate}, {Status: Skipped}})
	if got := ExitCode(clean, false); got != 0 {
		t.Errorf("ExitCode(clean) = %d, want 0", got)
	}
	if got := ExitCode(clean, true); got != 130 {
		t.Errorf("ExitCode(clean, interrupted) = %d, want 130", got)
	}
	failed := Summarize([]Result{{Status: Failed, Detail: "boom"}})
	if got := ExitCode(failed, false); got != 6 {
		t.Errorf("ExitCode(failed) = %d, want 6", got)
	}
	diverged := Summarize([]Result{{Status: Diverged}})
	if got := ExitCode(diverged, false); got != 6 {
		t.Errorf("ExitCode(diverged) = %d, want 6", got)
	}
}

func TestPlanSkipsUnresolvedForks(t *testing.T) {
	// Discovery could not read this fork's branch and parent, so nothing can
	// be planned for it — and saying "not a fork" would be a lie.
	f := fork("vanished", func(f *github.Fork) {
		f.Unresolved, f.HasParent, f.DefaultBranch, f.HeadOID = true, false, "", ""
	})
	for _, kind := range []Kind{KindSync, KindClone} {
		task := Plan([]github.Fork{f}, &config.Config{}, kind)[0]
		if !task.Skip || task.SkipReason != "detail unavailable" {
			t.Errorf("kind %v: task = %+v, want skipped as \"detail unavailable\"", kind, task)
		}
		if len(task.Log) == 0 || !strings.Contains(strings.Join(task.Log, " "), "renamed or removed") {
			t.Errorf("kind %v: log = %v, want it to explain what happened", kind, task.Log)
		}
	}
	// Exclusion still wins: the user asked not to hear about it at all.
	cfg := &config.Config{Excluded: []string{"vanished"}}
	if got := Plan([]github.Fork{f}, cfg, KindSync)[0].SkipReason; got != "excluded by config" {
		t.Errorf("SkipReason = %q, want %q", got, "excluded by config")
	}
}
