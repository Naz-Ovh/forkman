package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPathPrefersXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/xdg", "forkman", "config.json"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/tester")
	got, err = Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/tester", ".config", "forkman", "config.json"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	want := &Config{
		Org:               "0x-fork",
		Excluded:          []string{"some-repo", "test-*"},
		Concurrency:       4,
		DefaultBranchOnly: true,
		CloneDir:          "~/src/forks",
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if want.Version != CurrentVersion {
		t.Errorf("Save did not stamp Version: %d", want.Version)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Org != want.Org || got.Concurrency != want.Concurrency ||
		got.CloneDir != want.CloneDir || got.DefaultBranchOnly != want.DefaultBranchOnly {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Excluded) != 2 || got.Excluded[0] != "some-repo" || got.Excluded[1] != "test-*" {
		t.Errorf("Excluded = %v", got.Excluded)
	}
}

func TestSavePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "forkman", "config.json")
	if err := Save(path, &Config{Org: "acme"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
	// No temp files left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("leftover files in config dir: %d", len(entries))
	}
}

func TestSaveTightensLooseDirPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "forkman")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(dir, "config.json"), &Config{Org: "acme"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
}

func writeRaw(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadUnknownKey(t *testing.T) {
	path := writeRaw(t, `{"version":1,"org":"acme","excludes":["x"]}`)
	_, err := Load(path)
	var uke *UnknownKeyError
	if !errors.As(err, &uke) {
		t.Fatalf("error = %v, want *UnknownKeyError", err)
	}
	if uke.Key != "excludes" {
		t.Errorf("Key = %q, want %q", uke.Key, "excludes")
	}
	if got := uke.Error(); got != `unknown config key "excludes"` {
		t.Errorf("Error() = %q", got)
	}
}

func TestLoadVersionMismatch(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"version":2,"org":"acme"}`, "unsupported config version 2 (this build supports 1)"},
		{`{"org":"acme"}`, "unsupported config version 0 (this build supports 1)"},
	} {
		_, err := Load(writeRaw(t, tc.body))
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("error = %v, want *VersionError", err)
		}
		if ve.Error() != tc.want {
			t.Errorf("Error() = %q, want %q", ve.Error(), tc.want)
		}
	}
}

func TestLoadNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestLoadPreservesNilVersusEmptyExcluded(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantNil bool
	}{
		{"key absent", `{"version":1,"org":"acme"}`, true},
		{"empty array", `{"version":1,"org":"acme","excluded":[]}`, false},
		{"null", `{"version":1,"org":"acme","excluded":null}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeRaw(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if (c.Excluded == nil) != tc.wantNil {
				t.Errorf("Excluded nil = %v, want %v", c.Excluded == nil, tc.wantNil)
			}
			c.Normalize()
			if (c.Excluded == nil) != tc.wantNil {
				t.Errorf("Normalize changed nil-ness: nil = %v, want %v", c.Excluded == nil, tc.wantNil)
			}
		})
	}
}

func TestSaveEmptyExcludedSurvivesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, &Config{Org: "acme", Excluded: []string{}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if string(probe["excluded"]) != "[]" {
		t.Errorf("excluded serialised as %s, want []", probe["excluded"])
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Excluded == nil {
		t.Error("empty Excluded came back nil; the first-run selector would re-prompt")
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		in, want int
	}{{0, 4}, {-3, 4}, {1, 1}, {8, 8}, {16, 16}, {17, 16}, {1000, 16}}
	for _, tc := range tests {
		c := &Config{Concurrency: tc.in}
		c.Normalize()
		if c.Concurrency != tc.want {
			t.Errorf("Normalize(%d) = %d, want %d", tc.in, c.Concurrency, tc.want)
		}
		if !c.DefaultBranchOnly {
			t.Error("DefaultBranchOnly should be forced on")
		}
	}
}

func TestIsExcluded(t *testing.T) {
	c := &Config{Excluded: []string{"some-repo", "test-*", "  ", "MixedCase"}}
	tests := []struct {
		name string
		want bool
	}{
		{"some-repo", true},
		{"SOME-REPO", true},
		{"some-repo-2", false},
		{"test-", true},
		{"test-anything", true},
		{"TEST-ANYTHING", true},
		{"tests", false},
		{"mixedcase", true},
		{"MIXEDCASE", true},
		{"other", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := c.IsExcluded(tc.name); got != tc.want {
			t.Errorf("IsExcluded(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	empty := &Config{}
	if empty.IsExcluded("anything") {
		t.Error("empty exclusion list matched")
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	tests := []struct{ in, want string }{
		{"", ""},
		{"~", "/home/tester"},
		{"~/src/forks", "/home/tester/src/forks"},
		{"~other/src", "~other/src"},
		{"/abs/path", "/abs/path"},
		{"rel/path", "rel/path"},
	}
	for _, tc := range tests {
		if got := ExpandHome(tc.in); got != tc.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	path := writeRaw(t, `{"version":1,"org":"acme"}{"version":1}`)
	if _, err := Load(path); err == nil {
		t.Fatal("want error for trailing data")
	}
}

func TestModeAndProtocolRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := &Config{Org: "acme", SyncMode: ModeGit, Protocol: ProtoHTTPS, CloneDir: "/srv/forks"}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SyncMode != ModeGit || got.Protocol != ProtoHTTPS || got.CloneDir != "/srv/forks" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if !got.GitMode() {
		t.Error("GitMode() = false for syncMode=git")
	}
}

func TestLoadRejectsBadModeAndProtocol(t *testing.T) {
	tests := []struct {
		name, body, wantKey string
	}{
		{"mode", `{"version":1,"syncMode":"local"}`, "syncMode"},
		{"protocol", `{"version":1,"protocol":"rsync"}`, "protocol"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			var ve *ValueError
			if !errors.As(err, &ve) {
				t.Fatalf("Load error = %v, want *ValueError", err)
			}
			if ve.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", ve.Key, tc.wantKey)
			}
			if !strings.Contains(ve.Error(), tc.wantKey) {
				t.Errorf("Error() = %q", ve.Error())
			}
		})
	}
}

func TestValidateAllowsEmptyAndKnownValues(t *testing.T) {
	for _, c := range []*Config{
		{},
		{SyncMode: ModeAPI, Protocol: ProtoSSH},
		{SyncMode: ModeGit, Protocol: ProtoHTTPS},
	} {
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", c, err)
		}
	}
}

func TestNormalizeModeDefaults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix home layout")
	}
	t.Setenv("HOME", "/home/tester")

	api := &Config{}
	api.Normalize()
	if api.SyncMode != ModeAPI || api.Protocol != ProtoSSH {
		t.Errorf("api defaults = %q/%q, want %q/%q", api.SyncMode, api.Protocol, ModeAPI, ProtoSSH)
	}
	if api.CloneDir != "" {
		t.Errorf("CloneDir = %q, want it left unset in api mode", api.CloneDir)
	}

	git := &Config{SyncMode: ModeGit}
	git.Normalize()
	if want := filepath.Join("/home/tester", "src", "forks"); git.CloneDir != want {
		t.Errorf("CloneDir = %q, want %q", git.CloneDir, want)
	}

	// An existing tilde path is resolved in place, so cloneDir is always
	// reported and used as an absolute path.
	old := &Config{CloneDir: "~/elsewhere"}
	old.Normalize()
	if want := filepath.Join("/home/tester", "elsewhere"); old.CloneDir != want {
		t.Errorf("CloneDir = %q, want %q", old.CloneDir, want)
	}
}

func TestResolveDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix home layout")
	}
	t.Setenv("HOME", "/home/tester")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ in, want string }{
		{"~/src/forks", filepath.Join("/home/tester", "src", "forks")},
		{"~", "/home/tester"},
		{"forks", filepath.Join(cwd, "forks")},
		{"./a/../forks/", filepath.Join(cwd, "forks")},
		{"  /srv/forks/  ", "/srv/forks"},
	}
	for _, tc := range tests {
		got, err := ResolveDir(tc.in)
		if err != nil {
			t.Errorf("ResolveDir(%q) = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("ResolveDir(%q) = %q, want an absolute path", tc.in, got)
		}
	}
	for _, in := range []string{"", "   "} {
		if _, err := ResolveDir(in); !errors.Is(err, ErrEmptyDir) {
			t.Errorf("ResolveDir(%q) error = %v, want ErrEmptyDir", in, err)
		}
	}
	if ErrEmptyDir.Error() != "clone directory must not be empty" {
		t.Errorf("ErrEmptyDir = %q", ErrEmptyDir)
	}
}

func TestMatchesNamesThePattern(t *testing.T) {
	c := &Config{Excluded: []string{" Legacy ", "Test-*"}}
	cases := []struct {
		name, pattern string
		ok            bool
	}{
		{"legacy", "Legacy", true},
		{"test-thing", "Test-*", true},
		{"keeper", "", false},
	}
	for _, tc := range cases {
		pat, ok := c.Matches(tc.name)
		if ok != tc.ok || pat != tc.pattern {
			t.Errorf("Matches(%q) = %q, %v; want %q, %v", tc.name, pat, ok, tc.pattern, tc.ok)
		}
		if got := c.IsExcluded(tc.name); got != tc.ok {
			t.Errorf("IsExcluded(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
