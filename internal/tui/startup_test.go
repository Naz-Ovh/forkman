package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/naz-ovh/forkman/internal/sync"
)

// clock returns a clock that advances by tick on every read, so the rendered
// durations are deterministic.
func clock(t0 time.Time, tick time.Duration) func() time.Time {
	var advanced time.Duration
	return func() time.Time {
		now := t0.Add(advanced)
		advanced += tick
		return now
	}
}

// keyPress builds the key message whose String() is s.
func keyPress(s string) tea.KeyPressMsg {
	switch s {
	case "esc":
		return keyEsc
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	default:
		return kr(rune(s[0]))
	}
}

// startupModel builds a model parked on the startup checklist. Prepare is only
// there to select that state; the test feeds messages itself.
func startupModel(t *testing.T, now func() time.Time) appModel {
	t.Helper()
	m := newAppModel(Options{
		Org:   "0x-fork",
		Plain: true,
		Now:   now,
		Prepare: func(context.Context, func(Step)) (*Prepared, error) {
			t.Fatal("Prepare must not run: the test drives the model directly")
			return nil, nil
		},
	})
	if m.state != stateStartup {
		t.Fatalf("state = %v, want stateStartup when Prepare is set", m.state)
	}
	return step(t, m, tea.WindowSizeMsg{Width: 80, Height: 20})
}

func stepMsgs(steps ...Step) []tea.Msg {
	out := make([]tea.Msg, 0, len(steps))
	for _, s := range steps {
		out = append(out, stepMsg{s})
	}
	return out
}

func TestStartupShowsEveryStepWithItsElapsedTime(t *testing.T) {
	now := clock(time.Unix(0, 0), 400*time.Millisecond)
	m := startupModel(t, now)
	m = step(t, m, stepMsgs(
		Step{Key: "token", Label: "token", State: StepRunning},
		Step{Key: "token", Detail: "from gh auth token", State: StepOK},
		Step{Key: "discover", Label: "discover forks", Detail: "46 forks · branches and parents 20/46", State: StepRunning},
	)...)

	content := m.content()
	for _, want := range []string{"Preparing 0x-fork", "token", "from gh auth token", "discover forks", "20/46"} {
		if !strings.Contains(content, want) {
			t.Errorf("startup view is missing %q:\n%s", want, content)
		}
	}
	// A finished step keeps the time it took; the running one keeps counting.
	if !strings.Contains(content, "0.4s") {
		t.Errorf("startup view shows no step duration:\n%s", content)
	}
	if lines := strings.Count(content, "\n") + 1; lines != 20 {
		t.Errorf("startup view is %d lines, want exactly the terminal height (20)", lines)
	}
}

func TestStartupUpdatesAStepInPlace(t *testing.T) {
	now := clock(time.Unix(0, 0), 0)
	m := startupModel(t, now)
	m = step(t, m, stepMsgs(
		Step{Key: "discover", Label: "discover forks", Detail: "12 forks", State: StepRunning},
		Step{Key: "discover", Detail: "46 forks", State: StepOK},
	)...)

	if n := len(m.startup.lines); n != 1 {
		t.Fatalf("%d checklist lines, want 1: the same key updates in place", n)
	}
	l := m.startup.lines[0]
	if l.State != StepOK || l.Detail != "46 forks" {
		t.Errorf("line = %+v, want the finished detail", l.Step)
	}
	if l.label() != "discover forks" {
		t.Errorf("label = %q, want the first report's label to survive an update", l.label())
	}
}

func TestStartupHandsOverToTheWorkView(t *testing.T) {
	now := clock(time.Unix(0, 0), 500*time.Millisecond)
	m := startupModel(t, now)

	events := make(chan sync.Event)
	var started []sync.Task
	m.o.Start = func(tasks []sync.Task) <-chan sync.Event {
		started = tasks
		return events
	}
	tasks := mkTasks("alpha", "bravo")
	m = step(t, m, preparedMsg{p: &Prepared{Tasks: tasks, ScopesKnown: true}})

	if m.state != stateWork {
		t.Fatalf("state = %v, want stateWork", m.state)
	}
	if len(started) != 2 {
		t.Fatalf("runner started with %d tasks, want 2", len(started))
	}
	if len(m.rows) != 2 || m.rows[0].name != "alpha" {
		t.Fatalf("rows = %+v, want one per task in plan order", m.rows)
	}
	// The startup total stays on screen, so a slow preflight is not forgotten.
	if !strings.Contains(m.content(), "prep ") {
		t.Errorf("work header does not report the preparation time:\n%s", m.content())
	}
}

func TestStartupWithNoRunnerQuits(t *testing.T) {
	m := startupModel(t, nil)
	next, cmd := m.Update(preparedMsg{p: &Prepared{Tasks: mkTasks("alpha")}})
	if cmd == nil {
		t.Fatal("no command returned; --dry-run must end the program after preparing")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("command = %T, want tea.QuitMsg", cmd())
	}
	if got := next.(appModel).tasks; len(got) != 1 {
		t.Errorf("tasks = %v, want the plan to survive for the caller to print", got)
	}
}

func TestPrepareFailureLeavesTheScreenWithoutReporting(t *testing.T) {
	m := startupModel(t, nil)
	boom := errors.New("no GitHub token")
	next, cmd := m.Update(preparedMsg{err: boom})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("command = %T, want tea.QuitMsg", cmd())
	}
	fm := next.(appModel)
	if !errors.Is(fm.fail, boom) {
		t.Errorf("fail = %v, want the prepare error verbatim", fm.fail)
	}
	if fm.cancelled {
		t.Error("a failure is not a cancellation")
	}
}

func TestCancelDuringStartupStopsPrepare(t *testing.T) {
	for _, key := range []string{"esc", "q", "ctrl+c"} {
		m := startupModel(t, nil)
		cancelled := false
		m.cancelPrepare = func() { cancelled = true }

		next, cmd := m.Update(keyPress(key))
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: command = %T, want tea.QuitMsg", key, cmd())
		}
		fm := next.(appModel)
		if !cancelled {
			t.Errorf("%s: prepare was not cancelled; an in-flight request would be waited out", key)
		}
		if !fm.cancelled {
			t.Errorf("%s: cancelled = false", key)
		}
		if want := key == "ctrl+c"; fm.interrupted != want {
			t.Errorf("%s: interrupted = %v, want %v", key, fm.interrupted, want)
		}
	}
}

func TestFirstRunSelectorRunsBeforeTheWork(t *testing.T) {
	m := startupModel(t, nil)
	events := make(chan sync.Event)
	m.o.Start = func([]sync.Task) <-chan sync.Event { return events }

	var saved []string
	m.o.Confirm = func(excluded []string) ([]sync.Task, error) {
		saved = excluded
		return mkTasks("alpha"), nil // bravo is excluded, so it is not planned
	}

	m = step(t, m, preparedMsg{p: &Prepared{
		Tasks:      mkTasks("alpha", "bravo"),
		NeedSelect: true,
		Items:      []SelectorItem{{Name: "alpha"}, {Name: "bravo"}},
	}})
	if m.state != stateSelect {
		t.Fatalf("state = %v, want stateSelect on a first run", m.state)
	}
	if !strings.Contains(m.content(), "EXCLUDE") {
		t.Errorf("selector is not on screen:\n%s", m.content())
	}

	// Exclude bravo, confirm, and the run starts with the re-planned tasks.
	m = step(t, m, keyDown, keySpace, keyEnter)
	if m.state != stateWork {
		t.Fatalf("state = %v, want stateWork after confirming", m.state)
	}
	if len(saved) != 1 || saved[0] != "bravo" {
		t.Errorf("saved exclusions = %v, want [bravo]", saved)
	}
	if len(m.rows) != 1 || m.rows[0].name != "alpha" {
		t.Errorf("rows = %+v, want only the re-planned task", m.rows)
	}
}

func TestCancellingTheSelectorRunsNothing(t *testing.T) {
	m := startupModel(t, nil)
	m.o.Start = func([]sync.Task) <-chan sync.Event {
		t.Fatal("the runner must not start when the selector is cancelled")
		return nil
	}
	m = step(t, m, preparedMsg{p: &Prepared{
		Tasks: mkTasks("alpha"), NeedSelect: true, Items: []SelectorItem{{Name: "alpha"}},
	}})

	next, cmd := m.Update(keyEsc)
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("command = %T, want tea.QuitMsg", cmd())
	}
	if !next.(appModel).cancelled {
		t.Error("cancelled = false after esc in the selector")
	}
}

func TestConfirmFailureIsReportedNotSwallowed(t *testing.T) {
	m := startupModel(t, nil)
	boom := errors.New("write config: read-only file system")
	m.o.Confirm = func([]string) ([]sync.Task, error) { return nil, boom }
	m = step(t, m, preparedMsg{p: &Prepared{
		Tasks: mkTasks("alpha"), NeedSelect: true, Items: []SelectorItem{{Name: "alpha"}},
	}})

	next, cmd := m.Update(keyEnter)
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("command = %T, want tea.QuitMsg", cmd())
	}
	if !errors.Is(next.(appModel).fail, boom) {
		t.Errorf("fail = %v, want the save error", next.(appModel).fail)
	}
}

func TestStepsArrivingAfterHandoverAreIgnored(t *testing.T) {
	m := startupModel(t, nil)
	events := make(chan sync.Event)
	m.o.Start = func([]sync.Task) <-chan sync.Event { return events }
	m = step(t, m, preparedMsg{p: &Prepared{Tasks: mkTasks("alpha")}})

	before := m.content()
	m = step(t, m, stepMsg{Step{Key: "late", Label: "late", Detail: "stale report"}}, stepsClosedMsg{})
	if m.state != stateWork {
		t.Fatalf("state = %v, want stateWork", m.state)
	}
	if m.content() != before {
		t.Error("a late startup report changed the work view")
	}
}

func TestStartupSurvivesATinyTerminal(t *testing.T) {
	for _, sz := range []tea.WindowSizeMsg{{Width: 0, Height: 0}, {Width: 4, Height: 2}, {Width: 20, Height: 3}} {
		m := newAppModel(Options{Org: "0x-fork", Plain: true,
			Prepare: func(context.Context, func(Step)) (*Prepared, error) { return nil, nil }})
		m = step(t, m, sz, stepMsg{Step{Key: "token", Label: "token", Detail: "from gh auth token", State: StepOK}})
		content := m.content()
		if lines := strings.Count(content, "\n") + 1; lines > max(1, sz.Height) {
			t.Errorf("%v: %d lines rendered, want at most %d", sz, lines, max(1, sz.Height))
		}
		for _, line := range strings.Split(content, "\n") {
			if w := len([]rune(line)); sz.Width > 0 && w > sz.Width {
				t.Errorf("%v: line %q is %d cells wide", sz, line, w)
			}
		}
	}
}

func TestResizeDuringStartupIsSafe(t *testing.T) {
	// The selector does not exist yet while the checklist is up, so a resize
	// must not reach into it.
	m := startupModel(t, nil)
	m = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 40},
		stepMsg{Step{Key: "token", Label: "token", Detail: "from GH_TOKEN", State: StepOK}})
	if !strings.Contains(m.content(), "from GH_TOKEN") {
		t.Errorf("checklist did not survive the resize:\n%s", m.content())
	}
	if lines := strings.Count(m.content(), "\n") + 1; lines != 40 {
		t.Errorf("%d lines rendered, want 40", lines)
	}
}
