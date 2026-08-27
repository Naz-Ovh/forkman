package tui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// StepState is where one startup step stands.
type StepState int

const (
	StepRunning StepState = iota
	StepOK
	StepFail
)

// Step is one line of the startup checklist. A report whose Key is already on
// screen updates that line in place; a new Key appends one. Label defaults to
// Key when empty.
type Step struct {
	Key    string
	Label  string
	Detail string
	State  StepState
}

func (s Step) label() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Key
}

// stepLine is a Step plus the wall-clock it has taken.
type stepLine struct {
	Step
	begun time.Time
	took  time.Duration
	fixed bool // took is final: the step is no longer running
}

// startup is the checklist shown while preflight and discovery run, so the
// seconds before the first repository row are accounted for instead of silent.
type startup struct {
	th    Theme
	lines []stepLine
	index map[string]int
	begun time.Time
	done  bool
	took  time.Duration
}

func newStartup(th Theme, now time.Time) startup {
	return startup{th: th, index: map[string]int{}, begun: now}
}

// apply records a report, timing the step from the moment it first appeared.
func (s *startup) apply(st Step, now time.Time) {
	i, ok := s.index[st.Key]
	if !ok {
		s.index[st.Key] = len(s.lines)
		s.lines = append(s.lines, stepLine{Step: st, begun: now})
		i = len(s.lines) - 1
	}
	l := &s.lines[i]
	// A later report may omit the label; the first one wins.
	if st.Label == "" {
		st.Label = l.Label
	}
	l.Step = st
	if st.State != StepRunning && !l.fixed {
		l.took, l.fixed = now.Sub(l.begun), true
	}
}

// finish freezes the total, so the header keeps reading as the time startup
// actually took once the work view takes over.
func (s *startup) finish(now time.Time) {
	if !s.done {
		s.took, s.done = now.Sub(s.begun), true
	}
}

// elapsed is how long startup has been running, or took if it has finished.
func (s startup) elapsed(now time.Time) time.Duration {
	if s.done {
		return s.took
	}
	return now.Sub(s.begun)
}

// labelWidth is the column the details start in: wide enough for every label
// on screen, but never so wide that the detail has nowhere to go.
func (s startup) labelWidth(w int) int {
	n := 0
	for _, l := range s.lines {
		if c := lipgloss.Width(l.label()); c > n {
			n = c
		}
	}
	if maxLabel := w / 3; n > maxLabel {
		n = maxLabel
	}
	if n < 4 {
		n = 4
	}
	return n
}

// view renders one line per step: marker, label, detail, and the time that
// step has taken. spin is the current spinner frame, used for running steps.
func (s startup) view(w int, now time.Time, spin string) []string {
	labelW := s.labelWidth(w)
	out := make([]string, 0, len(s.lines))
	for _, l := range s.lines {
		mark, style := spin, s.th.Info
		switch l.State {
		case StepOK:
			mark, style = "✔", s.th.OK
		case StepFail:
			mark, style = "✖", s.th.Fail
		}
		took := l.took
		if !l.fixed {
			took = now.Sub(l.begun)
		}
		// A step that has just started has no meaningful duration yet.
		dur := ""
		if l.fixed || took >= 100*time.Millisecond {
			dur = formatDur(took)
		}

		const fixed = 4 // indent(2) + marker(1) + space(1)
		if w < fixed+labelW {
			out = append(out, trunc(l.label(), w))
			continue
		}
		durW := 0
		if avail := w - fixed - labelW; avail >= 20 {
			durW = 7
		}
		detailW := w - fixed - labelW - durW - 2
		line := "  " + style.Render(mark) + " " + style.Render(pad(l.label(), labelW))
		if detailW > 0 {
			line += "  " + s.th.Muted.Render(pad(l.Detail, detailW))
		}
		if durW > 0 {
			line += s.th.Muted.Render(padLeft(dur, durW))
		}
		out = append(out, strings.TrimRight(line, " "))
	}
	return out
}
