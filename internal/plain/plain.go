// Package plain renders run output for non-TTY stdout, --plain and --json.
package plain

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	sync "forkman/internal/sync"
)

// Writer emits one line per repository, either aligned text or JSON.
type Writer struct {
	W    io.Writer
	JSON bool
}

type resultLine struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	Commits    int    `json:"commits"`
	Ahead      int    `json:"ahead"`
	Behind     int    `json:"behind"`
	Message    string `json:"message"`
	DurationMS int64  `json:"duration_ms"`
}

type summaryLine struct {
	Total       int `json:"total"`
	Synced      int `json:"synced"`
	UpToDate    int `json:"up_to_date"`
	Skipped     int `json:"skipped"`
	Diverged    int `json:"diverged"`
	Failed      int `json:"failed"`
	Interrupted int `json:"interrupted"`
}

// Result writes one repository's outcome.
func (w *Writer) Result(r sync.Result) {
	if w.JSON {
		w.encode(resultLine{
			Name:       r.Name,
			Status:     r.Status.String(),
			Detail:     r.Detail,
			Commits:    r.Commits,
			Ahead:      r.Ahead,
			Behind:     r.Behind,
			Message:    r.Message,
			DurationMS: r.Duration.Milliseconds(),
		})
		return
	}
	fmt.Fprintf(w.W, "%-8s %-24s %s\n", r.Status.String(), r.Name, detail(r))
}

// Summary writes the closing tally.
func (w *Writer) Summary(s sync.Summary) {
	if w.JSON {
		w.encode(struct {
			Summary summaryLine `json:"summary"`
		}{summaryLine{
			Total:       s.Total,
			Synced:      s.Synced,
			UpToDate:    s.UpToDate,
			Skipped:     s.Skipped,
			Diverged:    s.Diverged,
			Failed:      s.Failed,
			Interrupted: s.Interrupted,
		}})
		return
	}
	parts := []string{
		fmt.Sprintf("total %d", s.Total),
		fmt.Sprintf("synced %d", s.Synced),
		fmt.Sprintf("up-to-date %d", s.UpToDate),
		fmt.Sprintf("skipped %d", s.Skipped),
		fmt.Sprintf("diverged %d", s.Diverged),
		fmt.Sprintf("failed %d", s.Failed),
	}
	if s.Interrupted > 0 {
		parts = append(parts, fmt.Sprintf("interrupted %d", s.Interrupted))
	}
	fmt.Fprintln(w.W, strings.Join(parts, "  "))
}

func (w *Writer) encode(v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintln(w.W, string(buf))
}

// detail renders the trailing column: merge type plus commit count for
// successful syncs, otherwise the status detail with the TUI's separator
// flattened to plain spacing.
func detail(r sync.Result) string {
	if r.Status == sync.Synced && r.MergeType != "" {
		if r.Commits > 0 {
			return fmt.Sprintf("%s  %d", r.MergeType, r.Commits)
		}
		return r.MergeType
	}
	return strings.ReplaceAll(r.Detail, " · ", " ")
}
