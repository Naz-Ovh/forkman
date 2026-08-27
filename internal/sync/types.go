// Package sync plans and executes the per-repository work: the shared
// vocabulary used by the TUI, the plain writer and main.
package sync

import (
	"time"

	"forkman/internal/github"
)

// Status is the terminal (or in-flight) state of one repository.
type Status int

const (
	Pending Status = iota
	Running
	Synced
	UpToDate
	Skipped
	Diverged
	Failed
)

func (s Status) String() string {
	switch s {
	case Pending:
		return "pending"
	case Running:
		return "running"
	case Synced:
		return "synced"
	case UpToDate:
		return "up to date"
	case Skipped:
		return "skipped"
	case Diverged:
		return "diverged"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Kind selects what the runner does with each task.
type Kind int

const (
	KindSync Kind = iota
	KindClone
)

// Task is one planned unit of work. Plan pre-classifies skips and
// already-in-sync repositories so no worker has to.
type Task struct {
	Fork       github.Fork
	Skip       bool
	SkipReason string
	UpToDate   bool

	// Log explains a pre-classified outcome (why it was skipped, why it is
	// already current) so the row is expandable even though no worker ever
	// runs for it. The runner copies it into the immediate Result.
	Log []string
}

// Result is the outcome for one repository.
type Result struct {
	Name   string
	Status Status
	Detail string

	Commits   int
	Ahead     int
	Behind    int
	MergeType string

	Err     error
	Message string // verbatim GitHub API message on failure

	Log      []string
	Duration time.Duration
}

// EventKind distinguishes the runner's progress messages.
type EventKind int

const (
	EvStarted EventKind = iota
	EvProgress
	EvLog
	EvDone
)

// Event is emitted by the runner on its channel. Workers never touch UI
// state; they only send these.
type Event struct {
	Name    string
	Kind    EventKind
	Percent float64
	Line    string
	// Replace marks an EvLog line as an in-place update of the previous line
	// for this repository: git redraws a progress phase with \r, and the
	// consumer overwrites instead of appending so the row keeps one line per
	// phase rather than one per percent.
	Replace bool
	Result  *Result
}

// Summary aggregates results for the final report and the exit code.
type Summary struct {
	Total       int
	Synced      int
	UpToDate    int
	Skipped     int
	Diverged    int
	Failed      int
	Interrupted int
}

// interruptedDetail marks a result the runner abandoned on cancellation.
const interruptedDetail = "interrupted"

// Summarize counts results by status. Results abandoned on cancellation are
// counted as interrupted rather than failed.
func Summarize(results []Result) Summary {
	var s Summary
	s.Total = len(results)
	for _, r := range results {
		switch r.Status {
		case Synced:
			s.Synced++
		case UpToDate:
			s.UpToDate++
		case Skipped:
			s.Skipped++
		case Diverged:
			s.Diverged++
		case Failed:
			if r.Detail == interruptedDetail {
				s.Interrupted++
			} else {
				s.Failed++
			}
		}
	}
	return s
}

// ExitCode maps a summary to forkman's process exit code.
func ExitCode(s Summary, interrupted bool) int {
	if interrupted || s.Interrupted > 0 {
		return 130
	}
	if s.Failed+s.Diverged > 0 {
		return 6
	}
	return 0
}
