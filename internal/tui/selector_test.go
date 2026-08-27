package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func mkItems(names ...string) []SelectorItem {
	out := make([]SelectorItem, 0, len(names))
	for i, n := range names {
		it := SelectorItem{Name: n}
		if i%2 == 0 {
			it.Behind = (i + 1) * 7
		} else {
			it.Note = "in sync"
		}
		out = append(out, it)
	}
	return out
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		ok         bool
		pos        []int
	}{
		{"tmpo", "tempo", true, []int{0, 2, 3, 4}},
		{"tempo", "tempo", true, []int{0, 1, 2, 3, 4}},
		{"TMPO", "tempo", true, []int{0, 2, 3, 4}},
		{"tmpo", "TEMPO", true, []int{0, 2, 3, 4}},
		{"tmp", "tempo-archive", true, []int{0, 2, 3}},
		{"", "tempo", true, nil},
		{"", "", true, nil},
		{"zzz", "tempo", false, nil},
		{"opmet", "tempo", false, nil},
		{"tempox", "tempo", false, nil},
		{"vc", "vault-core", true, []int{0, 6}},
	}
	for _, c := range cases {
		ok, pos := Match(c.pattern, c.s)
		if ok != c.ok {
			t.Errorf("Match(%q, %q) ok = %v, want %v", c.pattern, c.s, ok, c.ok)
			continue
		}
		if len(pos) != len(c.pos) {
			t.Errorf("Match(%q, %q) positions = %v, want %v", c.pattern, c.s, pos, c.pos)
			continue
		}
		for i := range pos {
			if pos[i] != c.pos[i] {
				t.Errorf("Match(%q, %q) positions = %v, want %v", c.pattern, c.s, pos, c.pos)
				break
			}
		}
	}
}

func TestSelectorSelectionPersistsAcrossFilterChanges(t *testing.T) {
	m := newSelectorModel("0x-fork",
		mkItems("tempo", "tempo-archive", "tempo-experiments", "old-vault", "sonic"),
		nil, NewTheme(true))
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Filter down to the tempo* repos and select the second one.
	m = step(t, m, kr('/'))
	m = step(t, m, typeRunes("tem")...)
	if len(m.filtered) != 3 {
		t.Fatalf("filter %q matched %d items, want 3", m.filter.Value(), len(m.filtered))
	}
	m = step(t, m, keyEsc) // blur the filter, keep the pattern
	if m.filtering {
		t.Fatal("esc should blur the filter input")
	}
	m = step(t, m, keyDown, keySpace)
	if !m.sel["tempo-archive"] {
		t.Fatalf("expected tempo-archive selected, got %v", m.selected())
	}

	// Clear the filter and select something that was previously filtered out.
	m = step(t, m, kr('/'))
	m = step(t, m, kc(tea.KeyBackspace), kc(tea.KeyBackspace), kc(tea.KeyBackspace))
	if m.filter.Value() != "" {
		t.Fatalf("filter = %q, want empty", m.filter.Value())
	}
	if len(m.filtered) != 5 {
		t.Fatalf("empty filter matched %d items, want 5", len(m.filtered))
	}
	m = step(t, m, keyEsc)
	m.cursor = 3 // old-vault
	m = step(t, m, keySpace)

	got := m.selected()
	want := []string{"old-vault", "tempo-archive"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selected = %v, want %v (selections must survive filter changes)", got, want)
	}

	// enter confirms; the model reports the sorted selection.
	m2, cmd := m.Update(keyEnter)
	if cmd == nil {
		t.Fatal("enter must quit the selector")
	}
	sm := m2.(selectorModel)
	if sm.cancelled {
		t.Fatal("enter must not cancel")
	}
	if strings.Join(sm.selected(), ",") != strings.Join(want, ",") {
		t.Fatalf("confirmed selection = %v, want %v", sm.selected(), want)
	}
}

func TestSelectorEscCancels(t *testing.T) {
	m := newSelectorModel("0x-fork", mkItems("a", "b"), []string{"a"}, NewTheme(true))
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m2, cmd := m.Update(keyEsc)
	if cmd == nil {
		t.Fatal("esc must quit")
	}
	if !m2.(selectorModel).cancelled {
		t.Fatal("esc must mark the selector cancelled")
	}
}

func TestSelectorAllAndNone(t *testing.T) {
	m := newSelectorModel("0x-fork",
		mkItems("tempo", "tempo-archive", "old-vault"), nil, NewTheme(true))
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24}, kr('/'))
	m = step(t, m, typeRunes("tem")...)
	m = step(t, m, keyEsc, kr('a'))
	if got := m.selected(); len(got) != 2 {
		t.Fatalf("a selected %v, want only the two visible tempo repos", got)
	}
	m = step(t, m, kr('n'))
	if got := m.selected(); len(got) != 0 {
		t.Fatalf("n left %v selected, want none", got)
	}
}

func TestSelectorEmptyFilterAndNoMatchRendering(t *testing.T) {
	items := mkItems("tempo", "tempo-archive", "old-vault")
	m := newSelectorModel("0x-fork", items, nil, NewTheme(true))
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	out := m.content()
	if strings.Contains(out, "\x1b") {
		t.Fatal("plain selector output contains ANSI escapes")
	}
	if !strings.Contains(out, "Select repos to EXCLUDE from sync") {
		t.Fatal("header missing")
	}
	if !strings.Contains(out, "0x-fork · 3 forks") {
		t.Fatal("header must show org and fork count")
	}
	for _, n := range []string{"tempo", "tempo-archive", "old-vault"} {
		if !strings.Contains(out, n) {
			t.Fatalf("empty filter should list %q", n)
		}
	}
	if !strings.Contains(out, "7 behind") {
		t.Fatal("behind count column missing")
	}
	if !strings.Contains(out, "in sync") {
		t.Fatal("note column missing when Behind == 0")
	}
	if !strings.Contains(out, "esc cancel") {
		t.Fatal("footer keys missing")
	}
	if strings.Contains(out, "Selected (") {
		t.Fatal("no selected block expected when nothing is selected")
	}

	// No match.
	m = step(t, m, kr('/'))
	m = step(t, m, typeRunes("zzzz")...)
	if len(m.filtered) != 0 {
		t.Fatalf("filter %q matched %d items, want 0", m.filter.Value(), len(m.filtered))
	}
	out = m.content()
	if !strings.Contains(out, "no matches") {
		t.Fatalf("no-match state must be rendered:\n%s", out)
	}
	for _, n := range []string{"tempo-archive", "old-vault"} {
		if strings.Contains(out, n) {
			t.Fatalf("%q should not be listed when nothing matches", n)
		}
	}
	if _, ok := m.current(); ok {
		t.Fatal("no current item expected with an empty result set")
	}
	// Toggling with nothing selected must not panic.
	m = step(t, m, keyEsc, keySpace)
}

func TestSelectorSelectedListCappedAtFive(t *testing.T) {
	names := []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	m := newSelectorModel("0x-fork", mkItems(names...), names, NewTheme(true))
	m = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	out := m.content()
	if !strings.Contains(out, "Selected (7)") {
		t.Fatalf("selected count missing:\n%s", out)
	}
	if !strings.Contains(out, "+2 more") {
		t.Fatalf("selected list must cap at 5 with +N more:\n%s", out)
	}
	if strings.Contains(out, "r6 · r7") {
		t.Fatal("selected list should not spell out the overflow")
	}
}

func TestSelectorViewportWindowingOnResize(t *testing.T) {
	names := make([]string, 30)
	for i := range names {
		names[i] = "repo" + string(rune('a'+i/10)) + string(rune('0'+i%10))
	}
	m := newSelectorModel("0x-fork", mkItems(names...), nil, NewTheme(true))
	m = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	vh := m.viewportHeight()
	if vh != 24-3-2 {
		t.Fatalf("viewportHeight = %d, want %d", vh, 24-3-2)
	}
	if strings.Contains(m.content(), names[vh]) {
		t.Fatalf("%q should be below the fold", names[vh])
	}

	for range len(names) {
		m = step(t, m, keyDown)
	}
	if m.cursor != len(names)-1 {
		t.Fatalf("cursor = %d, want %d", m.cursor, len(names)-1)
	}
	if !strings.Contains(m.content(), names[len(names)-1]) {
		t.Fatal("window must follow the cursor")
	}
	if strings.Contains(m.content(), names[0]) {
		t.Fatal("first row should have scrolled away")
	}

	m = step(t, m, tea.WindowSizeMsg{Width: 30, Height: 9})
	lines := strings.Split(m.content(), "\n")
	if len(lines) > 9 {
		t.Fatalf("rendered %d lines at height 9", len(lines))
	}
	for i, l := range lines {
		if w := len([]rune(l)); w > 30 {
			t.Fatalf("line %d is %d cells wide, terminal is 30: %q", i, w, l)
		}
	}
	if !strings.Contains(m.content(), names[len(names)-1]) {
		t.Fatal("cursor row must stay visible after shrinking")
	}
}

func TestSelectorTinySizes(t *testing.T) {
	for _, sz := range []tea.WindowSizeMsg{{Width: 0, Height: 0}, {Width: -3, Height: -1}, {Width: 1, Height: 1}, {Width: 4, Height: 3}} {
		m := newSelectorModel("0x-fork", mkItems("alpha", "bravo"), []string{"alpha"}, NewTheme(true))
		m = step(t, m, sz, keyDown, keySpace)
		if got := len(strings.Split(m.content(), "\n")); got > m.h() {
			t.Fatalf("size %v: rendered %d lines, height is %d", sz, got, m.h())
		}
		if !m.View().AltScreen {
			t.Fatalf("size %v: selector must be fullscreen", sz)
		}
	}
}

func TestSelectorFuzzyHighlightMarksMatchedRunes(t *testing.T) {
	th := NewTheme(false)
	m := newSelectorModel("0x-fork", mkItems("tempo"), nil, th)
	m = step(t, m, tea.WindowSizeMsg{Width: 60, Height: 12}, kr('/'))
	m = step(t, m, typeRunes("tmpo")...)
	row := m.rowLine(0)
	if !strings.Contains(row, "\x1b") {
		t.Fatal("matched runes should be styled when colors are enabled")
	}
	// The plain text is still recoverable and the row keeps its width.
	if got, want := lipgloss.Width(row), 60; got > want {
		t.Fatalf("row width %d exceeds terminal width %d", got, want)
	}
}
