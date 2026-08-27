package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"forkman/internal/config"
	fsync "forkman/internal/sync"
)

// capture runs fn with stdout and stderr redirected, returning its exit code
// and everything it printed.
func capture(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	text := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		text <- string(b)
	}()
	code := fn()
	w.Close()
	out := <-text
	r.Close()
	return code, out
}

// configureFixture points the config at a temporary directory and creates one.
func configureFixture(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix home layout")
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HOME", "/home/tester")
	if code, out := capture(t, func() int { return cmdConfigure([]string{"--org", "acme"}) }); code != exitOK {
		t.Fatalf("configure --org = %d\n%s", code, out)
	}
	return filepath.Join(root, "forkman", "config.json")
}

func TestConfigureStoresAbsoluteCloneDir(t *testing.T) {
	path := configureFixture(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ arg, want string }{
		{"forks", filepath.Join(cwd, "forks")},
		{"./nested/../forks/", filepath.Join(cwd, "forks")},
		{"~/src/forks", "/home/tester/src/forks"},
		{"/srv/forks", "/srv/forks"},
	}
	for _, tc := range tests {
		code, out := capture(t, func() int { return cmdConfigure([]string{"--clone-dir", tc.arg}) })
		if code != exitOK {
			t.Fatalf("configure --clone-dir=%q = %d\n%s", tc.arg, code, out)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.CloneDir != tc.want {
			t.Errorf("--clone-dir=%q stored %q, want %q", tc.arg, cfg.CloneDir, tc.want)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("settings dump does not show the resolved path %q:\n%s", tc.want, out)
		}
	}
}

func TestConfigureRejectsEmptyCloneDir(t *testing.T) {
	path := configureFixture(t)
	code, out := capture(t, func() int { return cmdConfigure([]string{"--clone-dir="}) })
	if code != exitConfig {
		t.Fatalf("exit = %d, want %d", code, exitConfig)
	}
	if !strings.Contains(out, "clone directory must not be empty") {
		t.Errorf("output = %q", out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CloneDir != "" {
		t.Errorf("CloneDir = %q, want the config left untouched", cfg.CloneDir)
	}
}

func TestConfigureModeAndProtocol(t *testing.T) {
	path := configureFixture(t)

	code, out := capture(t, func() int { return cmdConfigure([]string{"--mode", "git", "--protocol", "https"}) })
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SyncMode != config.ModeGit || cfg.Protocol != config.ProtoHTTPS {
		t.Errorf("stored %q/%q, want git/https", cfg.SyncMode, cfg.Protocol)
	}
	// git mode without an explicit clone dir falls back to ~/src/forks.
	if cfg.CloneDir != "/home/tester/src/forks" {
		t.Errorf("CloneDir = %q", cfg.CloneDir)
	}
	if !strings.Contains(out, "mode        git") || !strings.Contains(out, "protocol    https") {
		t.Errorf("settings dump missing mode/protocol:\n%s", out)
	}

	code, out = capture(t, func() int { return cmdConfigure([]string{"--mode", "local"}) })
	if code != exitConfig {
		t.Fatalf("exit = %d, want %d for an unknown mode\n%s", code, exitConfig, out)
	}
	if !strings.Contains(out, "invalid syncMode") {
		t.Errorf("output = %q", out)
	}
}

func TestConfigureNoFlagsPrintsResolvedSettings(t *testing.T) {
	configureFixture(t)
	if code, _ := capture(t, func() int { return cmdConfigure([]string{"--clone-dir", "~/forks"}) }); code != exitOK {
		t.Fatal("setup failed")
	}
	// stdout is a pipe, so the interactive selector is skipped.
	code, out := capture(t, func() int { return cmdConfigure(nil) })
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	for _, want := range []string{"mode        api", "protocol    ssh", "clone dir   /home/tester/forks"} {
		if !strings.Contains(out, want) {
			t.Errorf("settings dump lacks %q:\n%s", want, out)
		}
	}
}

func TestModeCheck(t *testing.T) {
	api := modeCheck(&config.Config{SyncMode: config.ModeAPI, Protocol: config.ProtoSSH})
	if !api.OK || !strings.Contains(api.Detail, "api") || !strings.Contains(api.Detail, "(unset)") {
		t.Errorf("api mode row = %+v", api)
	}
	git := modeCheck(&config.Config{SyncMode: config.ModeGit, Protocol: config.ProtoSSH, CloneDir: "/srv/forks"})
	if !git.OK || git.Detail != "git over ssh" {
		t.Errorf("git mode row = %+v", git)
	}
}

func TestKindVerb(t *testing.T) {
	if got := kindVerb(fsync.KindClone, false); got != "clone" {
		t.Errorf("clone verb = %q", got)
	}
	if got := kindVerb(fsync.KindSync, false); !strings.Contains(got, "merge-upstream") {
		t.Errorf("api verb = %q", got)
	}
	if got := kindVerb(fsync.KindSync, true); !strings.Contains(got, "push to origin") {
		t.Errorf("git verb = %q", got)
	}
}
