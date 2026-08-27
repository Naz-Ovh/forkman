package clone

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSplitLinesCR(t *testing.T) {
	// git writes progress with \r and normal output with \n.
	in := "Cloning into 'tempo'...\nReceiving objects:  12% (1/8)\rReceiving objects: 100% (8/8)\rdone.\n"
	sc := bufio.NewScanner(strings.NewReader(in))
	sc.Split(splitLinesCR)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	want := []string{
		"Cloning into 'tempo'...",
		"Receiving objects:  12% (1/8)",
		"Receiving objects: 100% (8/8)",
		"done.",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
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
	bin := filepath.Join(t.TempDir(), "fakegit")
	script := "#!/bin/sh\necho 'fatal: could not read Username' >&2\nexit 128\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	err := Run(context.Background(), Options{
		ForkURL: "https://example.invalid/a.git",
		Dir:     filepath.Join(t.TempDir(), "target"),
		Git:     bin,
	}, func(l string) { lines = append(lines, l) }, nil)
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
	bin := filepath.Join(t.TempDir(), "fakegit")
	script := "#!/bin/sh\necho stdout-value\necho 'fatal: nope' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
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
