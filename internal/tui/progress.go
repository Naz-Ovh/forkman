package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"forkman/internal/sync"
)

// bar wraps bubbles/progress. In plain mode the bubble is bypassed at render
// time so the output carries no escape sequences.
type bar struct {
	m     progress.Model
	plain bool
	width int
}

func newBar(th Theme) bar {
	opts := []progress.Option{progress.WithoutPercentage(), progress.WithWidth(defaultBarWidth)}
	if !th.Plain {
		opts = append(opts, progress.WithDefaultBlend())
	}
	return bar{m: progress.New(opts...), plain: th.Plain, width: defaultBarWidth}
}

const defaultBarWidth = 40

func (b *bar) setWidth(w int) {
	if w < 1 {
		w = 1
	}
	b.width = w
	b.m.SetWidth(w)
}

func (b *bar) setPercent(p float64) tea.Cmd {
	if b.plain {
		return nil
	}
	return b.m.SetPercent(clamp01(p))
}

func (b *bar) update(msg tea.Msg) tea.Cmd {
	if b.plain {
		return nil
	}
	var cmd tea.Cmd
	b.m, cmd = b.m.Update(msg)
	return cmd
}

// view renders the bar. pct is used directly in plain mode; otherwise the
// animated value held by the bubble is used.
func (b bar) view(pct float64) string {
	if b.plain {
		return plainBar(pct, b.width)
	}
	return b.m.View()
}

func plainBar(pct float64, w int) string {
	if w < 1 {
		return ""
	}
	full := int(math.Round(float64(w) * clamp01(pct)))
	if full > w {
		full = w
	}
	return strings.Repeat("█", full) + strings.Repeat("░", w-full)
}

func clamp01(p float64) float64 {
	if math.IsNaN(p) || p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

func percentText(pct float64) string {
	return fmt.Sprintf("%3d%%", int(math.Round(clamp01(pct)*100)))
}

// glyph is the one-cell status marker. Running rows use the spinner instead
// and pending rows are blank.
func glyph(s sync.Status) string {
	switch s {
	case sync.Synced:
		return "✔"
	case sync.UpToDate:
		return "○"
	case sync.Failed, sync.Diverged:
		return "✖"
	case sync.Skipped:
		return "–"
	default:
		return " "
	}
}

func statusStyle(th Theme, s sync.Status) lipgloss.Style {
	switch s {
	case sync.Synced:
		return th.OK
	case sync.Failed, sync.Diverged:
		return th.Fail
	case sync.Skipped:
		return th.Warn
	case sync.Running:
		return th.Info
	default:
		return th.Muted
	}
}

func formatDur(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// trunc shortens s to at most w display cells, appending an ellipsis when it
// had to cut. It is meant for unstyled text.
func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	b.WriteString("…")
	return b.String()
}

// pad truncates then right-pads unstyled text to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = trunc(s, w)
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// padLeft truncates then left-pads unstyled text to exactly w cells.
func padLeft(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = trunc(s, w)
	if n := w - lipgloss.Width(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

// fitKeys returns the first key-hint string that fits in w cells, falling
// back to a truncated version of the last (shortest) candidate.
func fitKeys(w int, candidates ...string) string {
	for _, c := range candidates {
		if lipgloss.Width(c) <= w {
			return c
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return trunc(candidates[len(candidates)-1], w)
}

// lr places right at the right edge of a w-cell line, keeping left as-is.
// Both sides may already be styled; widths are measured ANSI-aware. If there
// is no room the right side is dropped rather than mangled.
func lr(left, right string, w int) string {
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if right == "" || lw+rw+1 > w {
		return left
	}
	return left + strings.Repeat(" ", w-lw-rw) + right
}
