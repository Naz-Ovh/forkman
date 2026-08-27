package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"forkman/internal/sync"
)

// Options configures the run-time TUI. Workers never touch the model; they
// only publish on Events.
type Options struct {
	Org           string
	Kind          sync.Kind
	Tasks         []sync.Task
	Events        <-chan sync.Event
	Cancel        context.CancelFunc
	RateRemaining func() int
	Plain         bool
	Version       string
	ScopesKnown   bool
}

// Run drives the Work → Summary program and returns the collected results.
func Run(ctx context.Context, o Options) ([]sync.Result, bool, error) {
	p := tea.NewProgram(newAppModel(o),
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
		tea.WithFPS(30),
	)
	final, err := p.Run()
	interrupted := false
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			interrupted = true
		} else {
			return nil, false, err
		}
	}
	m, ok := final.(appModel)
	if !ok {
		return nil, interrupted, nil
	}
	return m.collect(), interrupted || m.interrupted, nil
}

type appState int

const (
	stateWork appState = iota
	stateCancelling
	stateSummary
)

type eventMsg struct{ ev sync.Event }
type eventsClosedMsg struct{}

// waitForEvent reads exactly one event so the model stays single-threaded.
func waitForEvent(ch <-chan sync.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg{ev}
	}
}

// maxRowLog matches the runner's Result.Log cap.
const maxRowLog = 200

type row struct {
	name     string
	status   sync.Status
	detail   string
	dur      time.Duration
	log      []string
	expanded bool
	res      *sync.Result
}

func (r row) failed() bool {
	return r.status == sync.Failed || r.status == sync.Diverged
}

type appModel struct {
	o     Options
	th    Theme
	state appState

	width, height int

	rows   []row
	index  map[string]int
	cursor int
	top    int

	done        int
	sp          spinner.Model
	bar         bar
	rate        int
	interrupted bool
}

func newAppModel(o Options) appModel {
	th := NewTheme(o.Plain)
	rows := make([]row, 0, len(o.Tasks))
	index := make(map[string]int, len(o.Tasks))
	for _, t := range o.Tasks {
		index[t.Fork.Name] = len(rows)
		rows = append(rows, row{name: t.Fork.Name, status: sync.Pending})
	}
	return appModel{
		o:     o,
		th:    th,
		rows:  rows,
		index: index,
		sp:    spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(th.Info)),
		bar:   newBar(th),
		rate:  -1,
	}
}

func (m appModel) Init() tea.Cmd {
	return tea.Batch(m.sp.Tick, waitForEvent(m.o.Events))
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.bar.setWidth(m.barWidth())
		m.ensureVisible()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		if m.state == stateSummary {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case eventMsg:
		m = m.applyEvent(msg.ev)
		if m.o.RateRemaining != nil {
			m.rate = m.o.RateRemaining()
		}
		cmds := []tea.Cmd{waitForEvent(m.o.Events)}
		if c := m.bar.setPercent(m.fraction()); c != nil {
			cmds = append(cmds, c)
		}
		m.ensureVisible()
		return m, tea.Batch(cmds...)

	case eventsClosedMsg:
		m.state = stateSummary
		m.autoExpandFirstFailure()
		m.ensureVisible()
		return m, nil
	}

	if cmd := m.bar.update(msg); cmd != nil {
		return m, cmd
	}
	return m, nil
}

func (m appModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		m.ensureVisible()
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		m.ensureVisible()
	case "home", "g":
		m.cursor = 0
		m.ensureVisible()
	case "end", "G":
		m.cursor = max(0, len(m.rows)-1)
		m.ensureVisible()
	case "enter", "space", " ":
		if m.state == stateSummary {
			return m, tea.Quit
		}
		m.toggle(m.cursor)
		m.ensureVisible()
	case "e":
		m.expandFailures()
		m.ensureVisible()
	case "esc":
		if m.state == stateSummary {
			return m, tea.Quit
		}
	case "q", "ctrl+c":
		switch m.state {
		case stateWork:
			m.interrupted = true
			m.state = stateCancelling
			if m.o.Cancel != nil {
				m.o.Cancel()
			}
		case stateCancelling:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
		default:
			return m, tea.Quit
		}
	}
	return m, nil
}

// toggle expands or collapses any row. Rows that have produced no output yet
// still expand; they show a single "(no output)" line, which is the honest
// answer and keeps enter from feeling broken.
func (m *appModel) toggle(i int) {
	if i < 0 || i >= len(m.rows) {
		return
	}
	m.rows[i].expanded = !m.rows[i].expanded
}

// expandFailures expands every failing row, or collapses them all when they
// are already expanded.
func (m *appModel) expandFailures() {
	anyCollapsed := false
	for _, r := range m.rows {
		if r.failed() && !r.expanded {
			anyCollapsed = true
			break
		}
	}
	for i := range m.rows {
		if m.rows[i].failed() {
			m.rows[i].expanded = anyCollapsed
		}
	}
}

func (m *appModel) autoExpandFirstFailure() {
	for i := range m.rows {
		if m.rows[i].failed() {
			m.rows[i].expanded = true
			m.cursor = i
			return
		}
	}
}

func (m appModel) applyEvent(ev sync.Event) appModel {
	i, ok := m.index[ev.Name]
	if !ok {
		return m
	}
	r := &m.rows[i]
	switch ev.Kind {
	case sync.EvStarted:
		r.status = sync.Running
	case sync.EvProgress:
		// Per-repo percentage is reflected in the global bar only.
	case sync.EvLog:
		// Same cap the runner puts on Result.Log, so the two stay comparable
		// at EvDone and a chatty clone cannot grow the model without bound.
		if ev.Line != "" && len(r.log) < maxRowLog {
			r.log = append(r.log, ev.Line)
		}
	case sync.EvDone:
		m.done++
		if ev.Result != nil {
			res := *ev.Result
			r.res = &res
			r.status = res.Status
			r.detail = res.Detail
			r.dur = res.Duration
			// The runner streams every line as EvLog and repeats them in
			// Result.Log, so the final log replaces what was streamed rather
			// than being appended to it. If Result.Log is the shorter of the
			// two (its 200-line cap was hit) the streamed lines are kept.
			if len(res.Log) >= len(r.log) {
				r.log = res.Log
			}
		}
	}
	return m
}

func (m appModel) collect() []sync.Result {
	out := make([]sync.Result, 0, len(m.rows))
	for _, r := range m.rows {
		if r.res != nil {
			out = append(out, *r.res)
		}
	}
	return out
}

func (m appModel) fraction() float64 {
	if len(m.rows) == 0 {
		return 1
	}
	return float64(m.done) / float64(len(m.rows))
}

func (m appModel) failedCount() int {
	n := 0
	for _, r := range m.rows {
		if r.failed() {
			n++
		}
	}
	return n
}

// ---- layout ----

func (m appModel) w() int {
	if m.width < 1 {
		return 1
	}
	return m.width
}

func (m appModel) h() int {
	if m.height < 1 {
		return 1
	}
	return m.height
}

func (m appModel) barWidth() int {
	w := m.w() - 4 - 6 // margins + " NNN%"
	if w < 1 {
		w = 1
	}
	if w > 60 {
		w = 60
	}
	return w
}

func (m appModel) viewportHeight() int {
	vh := m.h() - len(m.headerLines()) - len(m.footerLines())
	if vh < 1 {
		return 1
	}
	return vh
}

type dline struct {
	row  int
	text string
}

func (m appModel) displayLines() []dline {
	w := m.w()
	out := make([]dline, 0, len(m.rows)+8)
	for i := range m.rows {
		out = append(out, dline{row: i, text: m.rowLine(i)})
		if m.rows[i].expanded {
			log := m.rows[i].log
			if len(log) == 0 {
				log = []string{"(no output)"}
			}
			for _, l := range log {
				out = append(out, dline{row: -1, text: "        " + m.th.Muted.Render(trunc(l, w-8))})
			}
		}
	}
	return out
}

func (m appModel) cursorLine(lines []dline) int {
	for i, l := range lines {
		if l.row == m.cursor {
			return i
		}
	}
	return 0
}

func (m *appModel) ensureVisible() {
	lines := m.displayLines()
	vh := m.viewportHeight()
	ci := m.cursorLine(lines)
	if ci < m.top {
		m.top = ci
	}
	if ci >= m.top+vh {
		m.top = ci - vh + 1
	}
	if maxTop := len(lines) - vh; m.top > maxTop {
		m.top = maxTop
	}
	if m.top < 0 {
		m.top = 0
	}
}

func (m appModel) rowLine(i int) string {
	r := m.rows[i]
	th := m.th
	w := m.w()

	cur := "  "
	if i == m.cursor {
		cur = th.Cursor.Render("❯ ")
	}
	// Every row is expandable, so every row carries a marker.
	marker := "▸"
	if r.expanded {
		marker = "▾"
	}
	g := glyph(r.status)
	if r.status == sync.Running {
		g = m.sp.View()
	}
	st := statusStyle(th, r.status)

	const fixed = 6 // cursor(2) marker+space(2) glyph+space(2)
	avail := w - fixed
	if avail < 6 {
		return trunc(r.name, w)
	}
	durW := 0
	if avail >= 34 {
		durW = 7
	}
	nameW := (avail - durW) * 2 / 5
	if nameW > 26 {
		nameW = 26
	}
	if nameW < 6 {
		nameW = 6
	}
	detailW := avail - durW - nameW - 2
	if detailW < 0 {
		detailW = 0
	}

	var b strings.Builder
	b.WriteString(cur)
	b.WriteString(th.Muted.Render(marker))
	b.WriteString(" ")
	b.WriteString(st.Render(g))
	b.WriteString(" ")
	b.WriteString(st.Render(pad(r.name, nameW)))
	if detailW > 0 {
		b.WriteString("  ")
		b.WriteString(th.Muted.Render(pad(r.detail, detailW)))
	}
	if durW > 0 {
		b.WriteString(th.Muted.Render(padLeft(formatDur(r.dur), durW)))
	}
	return strings.TrimRight(b.String(), " ")
}

func (m appModel) headerLines() []string {
	w := m.w()
	th := m.th
	if m.state == stateSummary {
		return m.summaryHeader()
	}
	verb := "Syncing"
	if m.o.Kind == sync.KindClone {
		verb = "Cloning"
	}
	title := fmt.Sprintf("%s %s", verb, m.o.Org)
	if m.state == stateCancelling {
		title = "cancelling… " + m.o.Org
	}
	stats := fmt.Sprintf("%d/%d", m.done, len(m.rows))
	if n := m.failedCount(); n > 0 {
		stats += fmt.Sprintf(" · %d failed", n)
	}
	head := lr("  "+th.Title.Render(trunc(title, w-4)), th.Muted.Render(stats), w-2)
	barLine := "  " + m.bar.view(m.fraction()) + " " + th.Muted.Render(percentText(m.fraction()))
	return []string{head, trunc(barLine, w), ""}
}

func (m appModel) summaryHeader() []string {
	w := m.w()
	th := m.th
	s := sync.Summarize(m.collect())
	title := "Summary " + m.o.Org
	if m.interrupted {
		title = "Summary (interrupted) " + m.o.Org
	}
	head := lr("  "+th.Title.Render(trunc(title, w-4)),
		th.Muted.Render(fmt.Sprintf("%d repos", len(m.rows))), w-2)
	counts := fmt.Sprintf("✔ %d synced · ○ %d up to date · – %d skipped · ✖ %d failed · ✖ %d diverged",
		s.Synced, s.UpToDate, s.Skipped, s.Failed, s.Diverged)
	lines := []string{head, "  " + th.Muted.Render(trunc(counts, w-4))}
	if !m.o.ScopesKnown {
		lines = append(lines, "  "+th.Warn.Render(trunc(
			"note: workflow-file capability could not be verified in advance", w-4)))
	}
	return append(lines, "")
}

func (m appModel) footerLines() []string {
	w := m.w()
	th := m.th
	var keys string
	switch m.state {
	case stateSummary:
		keys = fitKeys(w-4,
			"↑↓ move · enter expand · e failures · q quit",
			"enter expand · e failures · q quit",
			"e failures · q quit",
			"q quit")
	case stateCancelling:
		keys = fitKeys(w-4, "cancelling… · ctrl+c force quit", "ctrl+c force quit", "ctrl+c")
	default:
		keys = fitKeys(w-4,
			"↑↓ move · enter expand · e failures · q cancel",
			"enter expand · e failures · q cancel",
			"e failures · q cancel",
			"q cancel")
	}
	right := ""
	if m.rate >= 0 {
		right = fmt.Sprintf("api: %d left", m.rate)
	}
	line := lr("  "+th.Muted.Render(keys), th.Muted.Render(right), w-2)
	return []string{"", line}
}

func (m appModel) content() string {
	head := m.headerLines()
	foot := m.footerLines()
	vh := m.viewportHeight()
	lines := m.displayLines()

	top := m.top
	if maxTop := len(lines) - vh; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}

	out := make([]string, 0, len(head)+vh+len(foot))
	out = append(out, head...)
	for i := 0; i < vh; i++ {
		if top+i < len(lines) {
			out = append(out, lines[top+i].text)
		} else {
			out = append(out, "")
		}
	}
	out = append(out, foot...)
	if len(out) > m.h() {
		out = out[:m.h()]
	}
	return strings.Join(out, "\n")
}

func (m appModel) View() tea.View {
	v := tea.NewView(m.content())
	v.AltScreen = true
	v.WindowTitle = "forkman"
	return v
}
