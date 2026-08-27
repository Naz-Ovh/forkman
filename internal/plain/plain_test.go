package plain

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sync "forkman/internal/sync"
)

func TestResultText(t *testing.T) {
	tests := []struct {
		name string
		in   sync.Result
		want string
	}{
		{
			"fast forward",
			sync.Result{Name: "tempo", Status: sync.Synced, MergeType: "fast-forward", Commits: 1181,
				Detail: "fast-forward · 1181 commits"},
			"synced   tempo                    fast-forward  1181",
		},
		{
			"merge without commit count",
			sync.Result{Name: "vault", Status: sync.Synced, MergeType: "merge", Detail: "merge"},
			"synced   vault                    merge",
		},
		{
			"failure",
			sync.Result{Name: "tempo-alloy", Status: sync.Failed, Detail: "422 · branch protection",
				Message: "Protected branch update failed"},
			"failed   tempo-alloy              422 branch protection",
		},
		{
			"skipped",
			sync.Result{Name: "old-tools", Status: sync.Skipped, Detail: "excluded by config"},
			"skipped  old-tools                excluded by config",
		},
		{
			"up to date",
			sync.Result{Name: "quiet", Status: sync.UpToDate, Detail: "already up to date"},
			"up to date quiet                    already up to date",
		},
		{
			"diverged",
			sync.Result{Name: "vault-core", Status: sync.Diverged, Detail: "409 · diverged"},
			"diverged vault-core               409 diverged",
		},
		{
			"long name is not truncated",
			sync.Result{Name: strings.Repeat("x", 30), Status: sync.Skipped, Detail: "archived"},
			"skipped  " + strings.Repeat("x", 30) + " archived",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &Writer{W: &buf}
			w.Result(tc.in)
			got := strings.TrimSuffix(buf.String(), "\n")
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
			if strings.Count(buf.String(), "\n") != 1 {
				t.Errorf("want exactly one line, got %q", buf.String())
			}
		})
	}
}

func TestResultTextGrepFailed(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{W: &buf}
	w.Result(sync.Result{Name: "a", Status: sync.Synced, MergeType: "fast-forward"})
	w.Result(sync.Result{Name: "b", Status: sync.Failed, Detail: "422 · branch protection"})
	var failed []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.HasPrefix(line, "failed") {
			failed = append(failed, line)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "b") {
		t.Errorf("grep failed found %v", failed)
	}
}

func TestResultJSON(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{W: &buf, JSON: true}
	w.Result(sync.Result{
		Name: "tempo", Status: sync.Failed, Detail: "422 · branch protection",
		Commits: 0, Ahead: 3, Behind: 12,
		Message:  "Protected branch update failed for refs/heads/main",
		Duration: 2400 * time.Millisecond,
	})
	line := buf.String()
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("want one JSON line, got %q", line)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	want := map[string]any{
		"name": "tempo", "status": "failed", "detail": "422 · branch protection",
		"commits": float64(0), "ahead": float64(3), "behind": float64(12),
		"message":     "Protected branch update failed for refs/heads/main",
		"duration_ms": float64(2400),
	}
	if len(got) != len(want) {
		t.Errorf("keys = %v, want %v", keys(got), keys(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
}

func TestSummaryText(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{W: &buf}
	w.Summary(sync.Summary{Total: 47, Synced: 12, UpToDate: 30, Skipped: 3, Diverged: 1, Failed: 1})
	got := strings.TrimSuffix(buf.String(), "\n")
	want := "total 47  synced 12  up-to-date 30  skipped 3  diverged 1  failed 1"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	buf.Reset()
	w.Summary(sync.Summary{Total: 2, Interrupted: 2})
	if !strings.Contains(buf.String(), "interrupted 2") {
		t.Errorf("summary omitted interrupted count: %q", buf.String())
	}
}

func TestSummaryJSON(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{W: &buf, JSON: true}
	w.Summary(sync.Summary{Total: 47, Synced: 12, UpToDate: 30, Skipped: 3, Diverged: 1, Failed: 1, Interrupted: 0})
	var got struct {
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	want := map[string]int{"total": 47, "synced": 12, "up_to_date": 30, "skipped": 3, "diverged": 1, "failed": 1, "interrupted": 0}
	if len(got.Summary) != len(want) {
		t.Fatalf("summary = %v, want %v", got.Summary, want)
	}
	for k, v := range want {
		if got.Summary[k] != v {
			t.Errorf("summary.%s = %d, want %d", k, got.Summary[k], v)
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
