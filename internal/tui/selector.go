package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// SelectorItem is one row of the exclusion selector.
type SelectorItem struct {
	Name   string
	Behind int
	Note   string
}

// Match reports whether pattern occurs in s as a case-insensitive subsequence
// and, if so, the rune indices in s that were matched. An empty pattern
// matches everything with no highlights.
func Match(pattern, s string) (bool, []int) {
	if pattern == "" {
		return true, nil
	}
	pr := []rune(pattern)
	pos := make([]int, 0, len(pr))
	pi := 0
	for i, r := range []rune(s) {
		if unicode.ToLower(r) == unicode.ToLower(pr[pi]) {
			pos = append(pos, i)
			pi++
			if pi == len(pr) {
				return true, pos
			}
		}
	}
	return false, nil
}

type selectorModel struct {
	th    Theme
	org   string
	items []SelectorItem

	// sel is keyed by repo NAME so selections survive filter changes.
	sel      map[string]bool
	filtered []int
	hits     map[int][]int

	filter    textinput.Model
	filtering bool

	cursor, top   int
	width, height int

	// done and cancelled report how the selector ended. The selector never
	// quits the program itself: the model that hosts it reads these and
	// decides what comes next.
	done      bool
	cancelled bool
}

func newSelectorModel(org string, items []SelectorItem, preselected []string, th Theme) selectorModel {
	ti := textinput.New()
	ti.Prompt = filterPrompt
	ti.Placeholder = "type to filter"
	if !th.Plain {
		ti.SetVirtualCursor(true)
	}
	sel := make(map[string]bool, len(preselected))
	for _, n := range preselected {
		sel[n] = true
	}
	m := selectorModel{
		th:     th,
		org:    org,
		items:  items,
		sel:    sel,
		hits:   map[int][]int{},
		filter: ti,
	}
	m.applyFilter()
	return m
}

// filterPrompt labels the filter input.
const filterPrompt = "Filter: ▸ "

// filterView renders the filter input. In plain mode the bubble is bypassed
// so the line carries no escape sequences.
func (m selectorModel) filterView() string {
	if !m.th.Plain {
		return m.filter.View()
	}
	v := m.filter.Value()
	if v == "" && !m.filtering {
		v = m.filter.Placeholder
	}
	return trunc(filterPrompt+v, m.w()-4)
}

func (m *selectorModel) applyFilter() {
	pat := m.filter.Value()
	m.filtered = m.filtered[:0]
	m.hits = make(map[int][]int, len(m.items))
	for i, it := range m.items {
		if ok, pos := Match(pat, it.Name); ok {
			m.filtered = append(m.filtered, i)
			m.hits[i] = pos
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.ensureVisible()
}

func (m selectorModel) selected() []string {
	out := make([]string, 0, len(m.sel))
	for n, on := range m.sel {
		if on {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func (m selectorModel) Init() tea.Cmd { return nil }

func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	if m.filtering {
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		return m, cmd
	}
	return m, nil
}

// resize lays the selector out for a new terminal size. It is also called
// directly, because the selector may appear after the size is already known.
func (m *selectorModel) resize(w, h int) {
	m.width, m.height = w, h
	m.filter.SetWidth(max(4, m.w()-14))
	m.ensureVisible()
}

func (m selectorModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.filtering {
		switch k {
		case "esc":
			m.filter.Blur()
			m.filtering = false
			return m, nil
		case "enter":
			m.filter.Blur()
			m.filtering = false
			return m, nil
		case "ctrl+c":
			m.cancelled = true
			return m, nil
		case "up":
			m.moveCursor(-1)
			return m, nil
		case "down":
			m.moveCursor(1)
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.applyFilter()
		return m, cmd
	}

	switch k {
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "home", "g":
		m.cursor = 0
		m.ensureVisible()
	case "end", "G":
		m.cursor = max(0, len(m.filtered)-1)
		m.ensureVisible()
	case "space", " ":
		if it, ok := m.current(); ok {
			m.sel[it.Name] = !m.sel[it.Name]
		}
	case "a":
		for _, i := range m.filtered {
			m.sel[m.items[i].Name] = true
		}
	case "n":
		for n := range m.sel {
			m.sel[n] = false
		}
	case "/":
		m.filtering = true
		return m, m.filter.Focus()
	case "enter":
		m.done = true
	case "esc", "q", "ctrl+c":
		m.cancelled = true
	}
	return m, nil
}

func (m *selectorModel) moveCursor(d int) {
	m.cursor += d
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > len(m.filtered)-1 {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.ensureVisible()
}

func (m selectorModel) current() (SelectorItem, bool) {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return SelectorItem{}, false
	}
	return m.items[m.filtered[m.cursor]], true
}

// ---- layout ----

func (m selectorModel) w() int {
	if m.width < 1 {
		return 1
	}
	return m.width
}

func (m selectorModel) h() int {
	if m.height < 1 {
		return 1
	}
	return m.height
}

func (m selectorModel) viewportHeight() int {
	vh := m.h() - len(m.headerLines()) - len(m.footerLines())
	if vh < 1 {
		return 1
	}
	return vh
}

func (m *selectorModel) ensureVisible() {
	vh := m.viewportHeight()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+vh {
		m.top = m.cursor - vh + 1
	}
	if maxTop := len(m.filtered) - vh; m.top > maxTop {
		m.top = maxTop
	}
	if m.top < 0 {
		m.top = 0
	}
}

func (m selectorModel) headerLines() []string {
	w := m.w()
	th := m.th
	left := "  " + th.Title.Render(trunc("Select repos to EXCLUDE from sync", w-4))
	right := th.Muted.Render(fmt.Sprintf("%s · %d forks", m.org, len(m.items)))
	return []string{lr(left, right, w-2), "  " + m.filterView(), ""}
}

func (m selectorModel) footerLines() []string {
	w := m.w()
	th := m.th
	var out []string
	sel := m.selected()
	if len(sel) > 0 {
		out = append(out, "", "  "+th.Header.Render(fmt.Sprintf("Selected (%d)", len(sel))))
		shown := sel
		suffix := ""
		if len(shown) > 5 {
			suffix = fmt.Sprintf(" · +%d more", len(shown)-5)
			shown = shown[:5]
		}
		out = append(out, "    "+th.Selected.Render(trunc(strings.Join(shown, " · ")+suffix, w-6)))
	}
	keys := fitKeys(w-4,
		"↑↓ move · space toggle · / filter · a all · n none · enter confirm · esc cancel",
		"↑↓ move · space toggle · / filter · a all · n none · enter ok · esc cancel",
		"space toggle · / filter · enter ok · esc cancel",
		"enter ok · esc cancel",
		"esc cancel")
	out = append(out, "", "  "+th.Muted.Render(keys))
	return out
}

func (m selectorModel) rowLine(fi int) string {
	th := m.th
	w := m.w()
	idx := m.filtered[fi]
	it := m.items[idx]

	cur := "  "
	if fi == m.cursor {
		cur = th.Cursor.Render("❯ ")
	}
	box := "[ ]"
	if m.sel[it.Name] {
		box = th.Selected.Render("[x]")
	}
	right := it.Note
	if it.Behind > 0 {
		right = fmt.Sprintf("%d behind", it.Behind)
	}

	const fixed = 6 // cursor(2) + box(3) + space(1)
	avail := w - fixed
	if avail < 6 {
		return trunc(it.Name, w)
	}
	rightW := 0
	if avail >= 28 {
		rightW = 14
	}
	nameW := avail - rightW - 2
	if nameW < 4 {
		nameW = avail
		rightW = 0
	}

	var b strings.Builder
	b.WriteString(cur)
	b.WriteString(box)
	b.WriteString(" ")
	b.WriteString(m.highlight(it.Name, m.hits[idx], nameW))
	if rightW > 0 {
		b.WriteString("  ")
		b.WriteString(th.Muted.Render(padLeft(right, rightW)))
	}
	return strings.TrimRight(b.String(), " ")
}

// highlight styles the matched runes of name, then pads to exactly w cells.
func (m selectorModel) highlight(name string, pos []int, w int) string {
	plain := pad(name, w)
	if len(pos) == 0 || m.th.Plain {
		return plain
	}
	set := make(map[int]bool, len(pos))
	for _, p := range pos {
		set[p] = true
	}
	var b strings.Builder
	for i, r := range []rune(plain) {
		if set[i] {
			b.WriteString(m.th.Match.Render(string(r)))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m selectorModel) content() string {
	head := m.headerLines()
	foot := m.footerLines()
	vh := m.viewportHeight()

	top := m.top
	if maxTop := len(m.filtered) - vh; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}

	out := make([]string, 0, len(head)+vh+len(foot))
	out = append(out, head...)
	for i := 0; i < vh; i++ {
		fi := top + i
		switch {
		case fi < len(m.filtered):
			out = append(out, m.rowLine(fi))
		case i == 0 && len(m.filtered) == 0:
			out = append(out, "  "+m.th.Muted.Render(trunc("no matches", m.w()-4)))
		default:
			out = append(out, "")
		}
	}
	out = append(out, foot...)
	if len(out) > m.h() {
		out = out[:m.h()]
	}
	return strings.Join(out, "\n")
}

func (m selectorModel) View() tea.View {
	v := tea.NewView(m.content())
	v.AltScreen = true
	v.WindowTitle = "forkman"
	return v
}
