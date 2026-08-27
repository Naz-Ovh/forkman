package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"forkman/internal/github"
	"forkman/internal/sync"
)

var updateGolden = flag.Bool("update", false, "update golden files")

// ---- helpers ----

func step[T tea.Model](t *testing.T, m T, msgs ...tea.Msg) T {
	t.Helper()
	var cur tea.Model = m
	for _, msg := range msgs {
		cur, _ = cur.Update(msg)
	}
	out, ok := cur.(T)
	if !ok {
		t.Fatalf("Update returned %T, want %T", cur, m)
	}
	return out
}

func kc(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func kr(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

var (
	keyEnter = kc(tea.KeyEnter)
	keyEsc   = kc(tea.KeyEscape)
	keyUp    = kc(tea.KeyUp)
	keyDown  = kc(tea.KeyDown)
	keySpace = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
)

func typeRunes(s string) []tea.Msg {
	out := make([]tea.Msg, 0, len(s))
	for _, r := range s {
		out = append(out, kr(r))
	}
	return out
}

func mkTasks(names ...string) []sync.Task {
	out := make([]sync.Task, 0, len(names))
	for _, n := range names {
		out = append(out, sync.Task{Fork: github.Fork{Name: n}})
	}
	return out
}

func doneEv(name string, st sync.Status, detail string, d time.Duration, log ...string) eventMsg {
	return eventMsg{sync.Event{Name: name, Kind: sync.EvDone, Result: &sync.Result{
		Name: name, Status: st, Detail: detail, Duration: d, Log: log,
	}}}
}

func startEv(name string) eventMsg {
	return eventMsg{sync.Event{Name: name, Kind: sync.EvStarted}}
}

func logEv(name, line string) eventMsg {
	return eventMsg{sync.Event{Name: name, Kind: sync.EvLog, Line: line}}
}

// expandedLines returns the log lines rendered under row i.
func expandedLines(m appModel, i int) []string {
	var out []string
	seen := false
	for _, l := range m.displayLines() {
		switch {
		case l.row == i:
			seen = true
		case l.row >= 0:
			if seen {
				return out
			}
		default:
			if seen {
				out = append(out, strings.TrimSpace(l.text))
			}
		}
	}
	return out
}

// ---- tests ----

func TestViewIsFullscreenAndTitled(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("a")})
	v := m.View()
	if !v.AltScreen {
		t.Fatal("View().AltScreen is false; the TUI must run fullscreen")
	}
	if v.WindowTitle != "forkman" {
		t.Fatalf("WindowTitle = %q, want %q", v.WindowTitle, "forkman")
	}
}

func TestTinyAndZeroTerminalSizes(t *testing.T) {
	for _, sz := range []tea.WindowSizeMsg{{Width: 0, Height: 0}, {Width: -5, Height: -2}, {Width: 1, Height: 1}, {Width: 3, Height: 2}, {Width: 12, Height: 5}} {
		m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha", "bravo", "charlie")})
		m = step(t, m, sz, doneEv("alpha", sync.Synced, "fast-forward · 3 commits", time.Second))
		content := m.content()
		if got, want := len(strings.Split(content, "\n")), m.h(); got > want {
			t.Fatalf("size %v: rendered %d lines, terminal height is %d", sz, got, want)
		}
		if !m.View().AltScreen {
			t.Fatalf("size %v: AltScreen lost", sz)
		}
	}
}

func TestStableRowOrderWhenResultsArriveOutOfOrder(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha", "bravo", "charlie", "delta")})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24},
		doneEv("delta", sync.Synced, "merge", 400*time.Millisecond),
		doneEv("bravo", sync.Failed, "422 · branch protection", 800*time.Millisecond),
		doneEv("alpha", sync.Synced, "fast-forward · 1 commit", 200*time.Millisecond),
		doneEv("charlie", sync.Skipped, "archived", 0),
	)

	var got []string
	for _, r := range m.rows {
		got = append(got, r.name)
	}
	want := []string{"alpha", "bravo", "charlie", "delta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("row order = %v, want %v", got, want)
	}

	var resNames []string
	for _, r := range m.collect() {
		resNames = append(resNames, r.Name)
	}
	if strings.Join(resNames, ",") != strings.Join(want, ",") {
		t.Fatalf("collected results = %v, want task order %v", resNames, want)
	}

	// The rendered rows appear in task order too.
	content := m.content()
	last := -1
	for _, n := range want {
		i := strings.Index(content, n)
		if i < 0 {
			t.Fatalf("row %q missing from view", n)
		}
		if i < last {
			t.Fatalf("row %q rendered out of order", n)
		}
		last = i
	}
}

func TestExpandCollapseAndExpandAllFailures(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha", "bravo", "charlie")})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24},
		doneEv("alpha", sync.Synced, "fast-forward · 1 commit", time.Second, "alpha-log-line"),
		doneEv("bravo", sync.Failed, "422 · branch protection", time.Second, "bravo-log-line"),
		doneEv("charlie", sync.Diverged, "409 · diverged", time.Second, "charlie-log-line"),
	)

	if strings.Contains(m.content(), "bravo-log-line") {
		t.Fatal("nothing should auto-expand during the run")
	}
	if !strings.Contains(m.content(), "▸") {
		t.Fatal("collapsed rows with logs must show ▸")
	}

	// enter toggles the row under the cursor (row 0 = alpha).
	m = step(t, m, keyEnter)
	if !m.rows[0].expanded || !strings.Contains(m.content(), "alpha-log-line") {
		t.Fatal("enter did not expand the cursor row")
	}
	if !strings.Contains(m.content(), "▾") {
		t.Fatal("expanded row must show ▾")
	}
	m = step(t, m, keySpace)
	if m.rows[0].expanded {
		t.Fatal("space did not collapse the cursor row")
	}

	// e expands every failure and leaves successes alone.
	m = step(t, m, kr('e'))
	if !m.rows[1].expanded || !m.rows[2].expanded {
		t.Fatal("e must expand all failing rows")
	}
	if m.rows[0].expanded {
		t.Fatal("e must not expand a successful row")
	}
	m = step(t, m, kr('e'))
	if m.rows[1].expanded || m.rows[2].expanded {
		t.Fatal("e must collapse failures again when all are expanded")
	}
}

// Enter must do something useful on a row that is still running: the runner
// streams a line per step for exactly this reason.
func TestExpandRunningRowShowsStreamedLines(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha", "bravo")})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24},
		startEv("alpha"),
		logEv("alpha", "compare upstream/main…"),
		logEv("alpha", "behind 12 · ahead 0"),
		keyEnter,
	)
	if !m.rows[0].expanded {
		t.Fatal("enter did not expand the running row")
	}
	got := m.content()
	for _, want := range []string{"compare upstream/main…", "behind 12 · ahead 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("streamed line %q not rendered while running", want)
		}
	}

	// EvDone repeats the streamed lines in Result.Log; they must not double up.
	m = step(t, m, doneEv("alpha", sync.Synced, "fast-forward · 12 commits", time.Second,
		"compare upstream/main…", "behind 12 · ahead 0", "POST merge-upstream (main)", "200 fast-forward"))
	lines := expandedLines(m, 0)
	want := []string{"compare upstream/main…", "behind 12 · ahead 0", "POST merge-upstream (main)", "200 fast-forward"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("expanded log after EvDone =\n%v\nwant\n%v", lines, want)
	}
	if n := strings.Count(m.content(), "behind 12 · ahead 0"); n != 1 {
		t.Errorf("line rendered %d times, want 1 (EvLog then Result.Log duplication)", n)
	}
}

// A truncated Result.Log must not throw away the longer streamed log.
func TestShorterResultLogDoesNotReplaceStreamedLines(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha")})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24},
		startEv("alpha"), logEv("alpha", "one"), logEv("alpha", "two"), logEv("alpha", "three"),
		doneEv("alpha", sync.Synced, "cloned", time.Second, "one"),
		keyEnter,
	)
	if got := expandedLines(m, 0); strings.Join(got, "|") != "one|two|three" {
		t.Errorf("expanded log = %v, want the streamed lines", got)
	}
}

// Skipped rows never run a worker; their explanation arrives with EvDone and
// must be reachable with enter.
func TestSkippedRowExpandsToItsExplanation(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha", "readonly")})
	m = step(t, m, tea.WindowSizeMsg{Width: 90, Height: 24},
		doneEv("readonly", sync.Skipped, "read-only (READ) · cannot push", 0,
			"viewerPermission: READ — need WRITE to push",
			"ask an org owner for write access, or exclude it: forkman configure --exclude-add readonly"),
		keyDown, keyEnter,
	)
	if !m.rows[1].expanded {
		t.Fatal("enter did not expand the skipped row")
	}
	if got := expandedLines(m, 1); len(got) != 2 || !strings.Contains(got[0], "viewerPermission: READ") {
		t.Errorf("expanded skipped row = %v", got)
	}
	if !strings.Contains(m.content(), "viewerPermission: READ") {
		t.Error("skip explanation not rendered")
	}
}

// Every row shows a marker so it is obvious they all expand, and a row with
// no output at all says so instead of silently ignoring enter.
func TestEveryRowIsExpandableWithNoOutputFallback(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha", "bravo")})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if n := strings.Count(m.content(), "▸"); n != 2 {
		t.Errorf("%d expand markers, want one per row", n)
	}

	m = step(t, m, keyEnter)
	if !m.rows[0].expanded {
		t.Fatal("enter must expand a row with no log")
	}
	if got := expandedLines(m, 0); len(got) != 1 || got[0] != "(no output)" {
		t.Errorf("expanded empty row = %v, want [(no output)]", got)
	}
	if !strings.Contains(m.content(), "▾") {
		t.Error("expanded row must show ▾")
	}
	m = step(t, m, keyEnter)
	if m.rows[0].expanded {
		t.Error("enter must collapse again")
	}
}

// Expanded log lines take up viewport rows, so windowing has to account for
// them or the cursor row scrolls off screen.
func TestExpandedLinesCountTowardWindowing(t *testing.T) {
	names := make([]string, 12)
	msgs := []tea.Msg{tea.WindowSizeMsg{Width: 60, Height: 14}}
	for i := range names {
		names[i] = fmt.Sprintf("repo%02d", i)
	}
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks(names...)})
	m = step(t, m, msgs...)
	for i := range names {
		m = step(t, m, doneEv(names[i], sync.Failed, "boom", time.Second,
			"line one", "line two", "line three", "line four"))
	}
	// Expand everything, then walk to the bottom.
	m = step(t, m, kr('e'))
	for range names {
		m = step(t, m, keyDown)
	}
	content := m.content()
	if !strings.Contains(content, names[len(names)-1]) {
		t.Fatal("cursor row scrolled out of view once logs were expanded")
	}
	if got := len(strings.Split(content, "\n")); got != 14 {
		t.Fatalf("rendered %d lines at height 14", got)
	}
}

func TestAutoExpandFirstFailureOnSummary(t *testing.T) {
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks("alpha", "bravo", "charlie")})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24},
		doneEv("alpha", sync.Synced, "fast-forward", time.Second),
		doneEv("bravo", sync.Failed, "422 · branch protection", time.Second, "bravo-log-line"),
		doneEv("charlie", sync.Failed, "403 · permission denied", time.Second, "charlie-log-line"),
		eventsClosedMsg{},
	)
	if m.state != stateSummary {
		t.Fatal("closed events channel must move the model to Summary")
	}
	if !m.rows[1].expanded {
		t.Fatal("first failure should auto-expand on Summary")
	}
	if m.rows[2].expanded {
		t.Fatal("only the first failure should auto-expand")
	}
	if !strings.Contains(m.content(), "bravo-log-line") {
		t.Fatal("auto-expanded log not rendered")
	}
}

func TestCancelDuringWorkThenSummary(t *testing.T) {
	cancelled := false
	m := newAppModel(Options{
		Org: "acme", Plain: true, Tasks: mkTasks("alpha", "bravo"),
		Cancel: func() { cancelled = true },
	})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24}, kr('q'))
	if !cancelled {
		t.Fatal("q during Work must call Cancel()")
	}
	if m.state != stateCancelling {
		t.Fatalf("state = %v, want stateCancelling", m.state)
	}
	if !strings.Contains(m.content(), "cancelling…") {
		t.Fatal("cancelling state must be visible in the view")
	}
	if !m.interrupted {
		t.Fatal("interrupted flag not set")
	}
	m = step(t, m, eventsClosedMsg{})
	if m.state != stateSummary {
		t.Fatal("channel close during cancellation must reach Summary")
	}
	if _, cmd := m.Update(keyEnter); cmd == nil {
		t.Fatal("enter in Summary must quit")
	}
	if _, cmd := m.Update(keyEsc); cmd == nil {
		t.Fatal("esc in Summary must quit")
	}
	if _, cmd := m.Update(kr('q')); cmd == nil {
		t.Fatal("q in Summary must quit")
	}
}

func TestRateRemainingReadOnEventOnly(t *testing.T) {
	calls := 0
	m := newAppModel(Options{
		Org: "acme", Plain: true, Tasks: mkTasks("alpha"),
		RateRemaining: func() int { calls++; return 4832 },
	})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	for range 5 {
		_ = m.content()
	}
	if calls != 0 {
		t.Fatalf("RateRemaining called %d times while only rendering; want 0", calls)
	}
	m = step(t, m, startEv("alpha"))
	if calls != 1 {
		t.Fatalf("RateRemaining called %d times after one event; want 1", calls)
	}
	if !strings.Contains(m.content(), "api: 4832 left") {
		t.Fatal("footer must show the remaining rate limit")
	}
}

func TestViewportWindowingFollowsCursorOnResize(t *testing.T) {
	names := make([]string, 40)
	for i := range names {
		names[i] = "repo" + string(rune('a'+i/10)) + string(rune('0'+i%10))
	}
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks(names...)})
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	vh := m.viewportHeight()
	if vh != 24-3-2 {
		t.Fatalf("viewportHeight = %d, want %d", vh, 24-3-2)
	}
	if strings.Contains(m.content(), names[vh]) {
		t.Fatalf("row %q should be below the fold", names[vh])
	}

	// Walk the cursor to the last row; the window must follow.
	for range len(names) {
		m = step(t, m, keyDown)
	}
	if m.cursor != len(names)-1 {
		t.Fatalf("cursor = %d, want %d", m.cursor, len(names)-1)
	}
	if !strings.Contains(m.content(), names[len(names)-1]) {
		t.Fatal("window did not follow the cursor to the last row")
	}
	if strings.Contains(m.content(), names[0]) {
		t.Fatal("first row should have scrolled out of view")
	}

	// Shrinking the terminal must keep the cursor row visible and the output
	// within the new height.
	m = step(t, m, tea.WindowSizeMsg{Width: 40, Height: 10})
	lines := strings.Split(m.content(), "\n")
	if len(lines) > 10 {
		t.Fatalf("rendered %d lines at height 10", len(lines))
	}
	if !strings.Contains(m.content(), names[len(names)-1]) {
		t.Fatal("cursor row not visible after shrink")
	}

	// Growing it shows more rows again.
	m = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 60})
	if !strings.Contains(m.content(), names[0]) {
		t.Fatal("tall terminal should show the whole list")
	}
	if got := len(strings.Split(m.content(), "\n")); got != 60 {
		t.Fatalf("rendered %d lines at height 60, want 60", got)
	}
}

func TestLongLinesTruncatedToWidth(t *testing.T) {
	long := strings.Repeat("very-long-repository-name-", 6)
	m := newAppModel(Options{Org: "acme", Plain: true, Tasks: mkTasks(long)})
	m = step(t, m, tea.WindowSizeMsg{Width: 40, Height: 12},
		doneEv(long, sync.Failed, strings.Repeat("detail ", 40), time.Second, strings.Repeat("log ", 60)),
		keyEnter,
	)
	for i, l := range strings.Split(m.content(), "\n") {
		if w := len([]rune(l)); w > 40 {
			t.Fatalf("line %d is %d cells wide, terminal is 40: %q", i, w, l)
		}
	}
}

func TestSummaryGolden(t *testing.T) {
	m := newAppModel(Options{
		Org:           "0x-fork",
		Plain:         true,
		ScopesKnown:   false,
		Tasks:         mkTasks("tempo", "tempo-alloy", "vault-core", "sonic-mev", "old-tools"),
		RateRemaining: func() int { return 4832 },
	})
	events := []tea.Msg{
		tea.WindowSizeMsg{Width: 80, Height: 24},
		startEv("tempo"),
		doneEv("tempo", sync.Synced, "fast-forward · 1181 commits", 2400*time.Millisecond),
		doneEv("tempo-alloy", sync.Failed, "422 · branch protection", 800*time.Millisecond,
			"refusing to allow a Personal Access Token to create or update workflow"),
		doneEv("vault-core", sync.Diverged, "409 · diverged", 1100*time.Millisecond,
			"Your branch is 3 ahead, 12 behind upstream:main.",
			"merge-upstream cannot fast-forward a diverged branch.",
			"Resolve locally:",
			"  git fetch upstream && git rebase upstream/main"),
		startEv("sonic-mev"),
		doneEv("old-tools", sync.UpToDate, "already up to date", 300*time.Millisecond),
	}
	m = step(t, m, events...)
	work := m.content()

	m = step(t, m, eventsClosedMsg{})
	summary := m.content()

	got := "== work ==\n" + work + "\n== summary ==\n" + summary + "\n"
	if strings.Contains(got, "\x1b") {
		t.Fatal("plain mode output contains ANSI escape sequences")
	}

	path := filepath.Join("testdata", "summary_plain.golden")
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/tui -run TestSummaryGolden -update)", err)
	}
	if got != string(want) {
		t.Errorf("view mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestPlainFromEnv(t *testing.T) {
	cases := []struct {
		noColor, forceColor string
		want                bool
	}{
		{"", "", false},
		{"1", "", true},
		{"anything", "", true},
		{"1", "1", false},
		{"", "1", false},
	}
	for _, c := range cases {
		getenv := func(k string) string {
			switch k {
			case "NO_COLOR":
				return c.noColor
			case "FORCE_COLOR":
				return c.forceColor
			}
			return ""
		}
		if got := PlainFromEnv(getenv); got != c.want {
			t.Errorf("PlainFromEnv(NO_COLOR=%q FORCE_COLOR=%q) = %v, want %v",
				c.noColor, c.forceColor, got, c.want)
		}
	}
	if PlainFromEnv(nil) {
		t.Error("PlainFromEnv(nil) should be false")
	}
}

func TestPlainThemeRendersPureText(t *testing.T) {
	th := NewTheme(true)
	if !th.Plain {
		t.Fatal("NewTheme(true).Plain must be true")
	}
	for name, st := range map[string]interface{ Render(...string) string }{
		"Title": th.Title, "Header": th.Header, "Muted": th.Muted, "OK": th.OK,
		"Fail": th.Fail, "Warn": th.Warn, "Info": th.Info, "Cursor": th.Cursor,
		"Selected": th.Selected, "Match": th.Match, "Bar": th.Bar,
	} {
		if got := st.Render("x"); got != "x" {
			t.Errorf("plain %s.Render(\"x\") = %q, want %q", name, got, "x")
		}
	}
}
