package clone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// progressStream is a phase of git progress exactly as git writes it: updates
// separated by \r, the final state of each phase terminated by \n.
const progressStream = "Receiving objects:   1% (1/3)\rReceiving objects:  66% (2/3)\r" +
	"Receiving objects: 100% (3/3), done.\nResolving deltas:  50% (1/2)\r" +
	"Resolving deltas: 100% (2/2), done.\n"

// collector is the consumer side of onLine: it applies replace the way the
// runner and the TUI do.
type collector struct{ lines []string }

func (c *collector) onLine(line string, replace bool) {
	if replace && len(c.lines) > 0 {
		c.lines[len(c.lines)-1] = line
		return
	}
	c.lines = append(c.lines, line)
}

func TestScanProgressCollapsesInPlaceUpdates(t *testing.T) {
	var c collector
	scanProgress(strings.NewReader(progressStream), c.onLine)
	want := []string{
		"Receiving objects: 100% (3/3), done.",
		"Resolving deltas: 100% (2/2), done.",
	}
	if strings.Join(c.lines, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", c.lines, want)
	}
}

func TestScanProgressKeepsFinishedLinesAndMarksUpdates(t *testing.T) {
	// A normal line, then a phase git redraws, then a plain line again.
	in := "Cloning into 'tempo'...\nReceiving objects:  12% (1/8)\rReceiving objects: 100% (8/8)\rdone.\nnext\n"
	type frag struct {
		line    string
		replace bool
	}
	var got []frag
	scanProgress(strings.NewReader(in), func(line string, replace bool) {
		got = append(got, frag{line, replace})
	})
	want := []frag{
		{"Cloning into 'tempo'...", false},
		{"Receiving objects:  12% (1/8)", false},
		{"Receiving objects: 100% (8/8)", true},
		{"done.", true},
		{"next", false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fragment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A phase ending in "\r…, done.\n" must not leave a blank line behind, and a
// trailing \r must not make the next real line overwrite a finished one.
func TestScanProgressNoEmptyLines(t *testing.T) {
	var c collector
	scanProgress(strings.NewReader("first\r\nsecond\n\r\nthird"), c.onLine)
	if want := "first|second|third"; strings.Join(c.lines, "|") != want {
		t.Errorf("got %q, want %q", c.lines, want)
	}
}

func TestSplitLinesCollapsesProgress(t *testing.T) {
	got := splitLines(progressStream + "fatal: nope\n")
	want := []string{
		"Receiving objects: 100% (3/3), done.",
		"Resolving deltas: 100% (2/2), done.",
		"fatal: nope",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// fakeGit writes a shell script standing in for git and returns its path.
func fakeGit(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakegit")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The whole reader path, from git's pipes to the caller: hundreds of progress
// updates must arrive as one line per phase.
func TestRunCollapsesProgressLinesAndReportsPercent(t *testing.T) {
	bin := fakeGit(t, "printf 'Receiving objects:   1%% (1/3)\\rReceiving objects:  66%% (2/3)\\r"+
		"Receiving objects: 100%% (3/3), done.\\nResolving deltas:  50%% (1/2)\\r"+
		"Resolving deltas: 100%% (2/2), done.\\n' >&2\n")

	var c collector
	var pct []float64
	err := Run(context.Background(), Options{
		ForkURL: "https://example.invalid/a.git",
		Dir:     filepath.Join(t.TempDir(), "target"),
		Git:     bin,
	}, c.onLine, func(p float64) { pct = append(pct, p) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"Receiving objects: 100% (3/3), done.",
		"Resolving deltas: 100% (2/2), done.",
		historyOnlyLine,
	}
	if strings.Join(c.lines, "|") != strings.Join(want, "|") {
		t.Errorf("streamed lines =\n%q\nwant\n%q", c.lines, want)
	}
	// Every update is still reported as progress, in order, per phase.
	wantPct := []float64{0.01, 0.66, 1, 0.5, 1, 1} // the last 1 is Run finishing
	if fmt.Sprint(pct) != fmt.Sprint(wantPct) {
		t.Errorf("percentages = %v, want %v", pct, wantPct)
	}
}

// The default clone takes history and nothing else: no blobs, no working tree.
func TestCloneArgsHistoryOnlyByDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		full    bool
		want    []string
		notWant []string
	}{
		{
			name: "default",
			want: []string{"clone", "--progress", "--filter=blob:none", "--no-checkout"},
		},
		{
			name:    "full",
			full:    true,
			want:    []string{"clone", "--progress"},
			notWant: []string{"--filter=blob:none", "--no-checkout"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := filepath.Join(t.TempDir(), "argv")
			bin := fakeGit(t, "echo \"$@\" >> "+argv+"\n")
			var c collector
			if err := EnsureClone(context.Background(), Options{
				ForkURL: "https://example.invalid/a.git",
				Dir:     filepath.Join(t.TempDir(), "target"),
				Git:     bin,
				Full:    tc.full,
			}, c.onLine, nil); err != nil {
				t.Fatalf("EnsureClone: %v", err)
			}
			got, err := os.ReadFile(argv)
			if err != nil {
				t.Fatal(err)
			}
			line := strings.TrimSpace(string(got))
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("git args %q lack %q", line, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(line, w) {
					t.Errorf("git args %q contain %q", line, w)
				}
			}
			hasNote := slices.Contains(c.lines, historyOnlyLine)
			if hasNote == tc.full {
				t.Errorf("history-only note present = %v, want %v (log %q)", hasNote, !tc.full, c.lines)
			}
		})
	}
}

func TestPercentParsing(t *testing.T) {
	tests := map[string]int{
		"Receiving objects:  26% (12/47)":   26,
		"Resolving deltas: 100% (3/3)":      100,
		"Counting objects: 7% (1/14), done": 7,
		"remote: Enumerating objects: 100":  -1,
		"Cloning into 'x'...":               -1,
	}
	for line, want := range tests {
		m := percentRe.FindStringSubmatch(line)
		if want < 0 {
			if m != nil {
				t.Errorf("%q unexpectedly matched %v", line, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("%q did not match", line)
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n != want {
			t.Errorf("%q -> %v (%v), want %d", line, m[1], err, want)
		}
	}
}

func TestIsRepo(t *testing.T) {
	dir := t.TempDir()
	if IsRepo(dir) {
		t.Error("empty dir reported as a repo")
	}
	if IsRepo("") {
		t.Error("empty path reported as a repo")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(dir) {
		t.Error("dir with .git not reported as a repo")
	}
}

func TestRunRejectsEmptyDir(t *testing.T) {
	if err := Run(context.Background(), Options{ForkURL: "x"}, nil, nil); err == nil {
		t.Fatal("want an error for an empty target directory")
	}
}

func TestRunUsesInjectedGitAndReportsFailure(t *testing.T) {
	// A "git" that always fails, so no network or real git is involved.
	bin := fakeGit(t, "echo 'fatal: could not read Username' >&2\nexit 128\n")
	var lines []string
	err := Run(context.Background(), Options{
		ForkURL: "https://example.invalid/a.git",
		Dir:     filepath.Join(t.TempDir(), "target"),
		Git:     bin,
	}, func(l string, _ bool) { lines = append(lines, l) }, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "could not read Username") {
		t.Errorf("error = %v, want the last git line included", err)
	}
	if len(lines) == 0 {
		t.Error("git output was not streamed")
	}
}

func TestReason(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name: "hook rejection over the noise",
			lines: []string{
				"Enumerating objects: 5, done.",
				"Writing objects:  50% (1/2)",
				"remote: pre-receive: pushes are blocked by policy",
				"To /tmp/origin.git",
				"! [remote rejected] main -> main (pre-receive hook declined)",
			},
			want: "pre-receive: pushes are blocked by policy",
		},
		{
			name: "workflow scope",
			lines: []string{
				"remote: error: refusing to allow an OAuth App to create or update workflow without `workflow` scope",
			},
			want: "error: refusing to allow an OAuth App to create or update workflow without `workflow` scope",
		},
		{
			name: "ssh key",
			lines: []string{
				"git@github.com: Permission denied (publickey).",
				"fatal: Could not read from remote repository.",
			},
			want: "git@github.com: Permission denied (publickey).",
		},
		{
			name:  "only noise",
			lines: []string{"Counting objects: 100% (3/3)", "To github.com:a/b.git"},
			want:  "",
		},
		{
			name:  "error prefix stripped",
			lines: []string{"error: failed to push some refs"},
			want:  "failed to push some refs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Reason(tc.lines); got != tc.want {
				t.Errorf("Reason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGitCapturesExitStatus(t *testing.T) {
	// A stand-in for git that exits 1 with a message, so no real git is used.
	bin := fakeGit(t, "echo stdout-value\necho 'fatal: nope' >&2\nexit 1\n")
	g := Git(context.Background(), Options{Git: bin}, "status")
	if g.OK() || g.Code != 1 {
		t.Errorf("Code = %d, OK = %v, want 1/false", g.Code, g.OK())
	}
	if g.Stdout != "stdout-value" {
		t.Errorf("Stdout = %q", g.Stdout)
	}
	if g.Reason() != "nope" {
		t.Errorf("Reason() = %q, want %q", g.Reason(), "nope")
	}
}

func TestGitMissingBinary(t *testing.T) {
	g := Git(context.Background(), Options{Git: filepath.Join(t.TempDir(), "absent")}, "status")
	if g.Code != -1 {
		t.Errorf("Code = %d, want -1 for a git that cannot run", g.Code)
	}
	if g.Reason() == "" {
		t.Error("Reason() empty; the start failure should be reported")
	}
}
