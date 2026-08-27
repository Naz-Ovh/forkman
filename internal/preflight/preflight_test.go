package preflight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forkman/internal/config"
	"forkman/internal/github"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveTokenPrecedence(t *testing.T) {
	ghCalled := false
	gh := func(context.Context) (string, error) {
		ghCalled = true
		return "from-gh", nil
	}
	tests := []struct {
		name       string
		vars       map[string]string
		wantToken  string
		wantSource string
		wantGH     bool
	}{
		{"forkman wins", map[string]string{
			"FORKMAN_TOKEN": "a", "GH_TOKEN": "b", "GITHUB_TOKEN": "c",
		}, "a", "FORKMAN_TOKEN", false},
		{"gh token next", map[string]string{
			"GH_TOKEN": "b", "GITHUB_TOKEN": "c",
		}, "b", "GH_TOKEN", false},
		{"github token next", map[string]string{
			"GITHUB_TOKEN": "c",
		}, "c", "GITHUB_TOKEN", false},
		{"cli last", map[string]string{}, "from-gh", "gh auth token", true},
		{"blank vars are ignored", map[string]string{
			"FORKMAN_TOKEN": "   ", "GH_TOKEN": "\t",
		}, "from-gh", "gh auth token", true},
		{"whitespace trimmed", map[string]string{
			"GH_TOKEN": "  padded  ",
		}, "padded", "GH_TOKEN", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ghCalled = false
			tok, src, err := ResolveToken(context.Background(), env(tc.vars), gh)
			if err != nil {
				t.Fatalf("ResolveToken: %v", err)
			}
			if tok != tc.wantToken || src != tc.wantSource {
				t.Errorf("got (%q, %q), want (%q, %q)", tok, src, tc.wantToken, tc.wantSource)
			}
			if ghCalled != tc.wantGH {
				t.Errorf("gh auth token called = %v, want %v", ghCalled, tc.wantGH)
			}
		})
	}
}

func TestResolveTokenFailures(t *testing.T) {
	_, _, err := ResolveToken(context.Background(), env(nil), func(context.Context) (string, error) {
		return "", errors.New("gh not installed")
	})
	if err == nil {
		t.Fatal("want an error when nothing supplies a token")
	}
	_, _, err = ResolveToken(context.Background(), env(nil), func(context.Context) (string, error) {
		return "  \n", nil
	})
	if err == nil {
		t.Fatal("want an error when gh returns only whitespace")
	}
}

func TestGHAuthTokenTimesOut(t *testing.T) {
	if _, err := exec.LookPath("gh"); err == nil {
		t.Skip("gh is installed; this test only checks the failure path")
	}
	start := time.Now()
	if _, err := GHAuthToken(context.Background()); err == nil {
		t.Fatal("want an error when gh is missing")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("GHAuthToken took %v, want well under the 2s timeout", d)
	}
}

// apiStub serves /user and /orgs/{org}.
type apiStub struct {
	scopes    string
	userCode  int
	orgCode   int
	orgCalled bool
}

func (a *apiStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			if a.scopes != "" {
				w.Header().Set("X-OAuth-Scopes", a.scopes)
			}
			if a.userCode != 0 {
				w.WriteHeader(a.userCode)
				fmt.Fprint(w, `{"message":"Bad credentials"}`)
				return
			}
			fmt.Fprint(w, `{"login":"octo"}`)
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			a.orgCalled = true
			if a.orgCode != 0 {
				w.WriteHeader(a.orgCode)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprint(w, `{"login":"acme"}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func newStub(t *testing.T, a *apiStub) *github.Client {
	t.Helper()
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)
	return github.New("tok", "test",
		github.WithBaseURL(srv.URL),
		github.WithHTTPClient(srv.Client()),
		github.WithClock(time.Now, func(context.Context, time.Duration) error { return nil }),
	)
}

func lookPathOK(name string) (string, error) { return "/usr/bin/" + name, nil }

func lookPathMissing(string) (string, error) { return "", exec.ErrNotFound }

func TestRunGitMissing(t *testing.T) {
	stub := &apiStub{}
	res, fail := Run(context.Background(), Options{
		NeedGit:  true,
		Org:      "acme",
		Client:   newStub(t, stub),
		LookPath: lookPathMissing,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail == nil {
		t.Fatal("want a failure")
	}
	if fail.Code != CodeGitMissing {
		t.Errorf("Code = %d, want %d", fail.Code, CodeGitMissing)
	}
	if !strings.Contains(fail.Msg, "git not found on PATH") {
		t.Errorf("Msg = %q", fail.Msg)
	}
	if !strings.Contains(fail.Fix, "install git") {
		t.Errorf("Fix = %q", fail.Fix)
	}
	if len(res.Checks) != 1 || res.Checks[0].OK {
		t.Errorf("Checks = %+v, want one failed check", res.Checks)
	}
	if stub.orgCalled {
		t.Error("preflight continued past the git check")
	}
}

func TestRunGitNotNeeded(t *testing.T) {
	res, fail := Run(context.Background(), Options{
		Org:      "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, workflow"}),
		LookPath: lookPathMissing,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail != nil {
		t.Fatalf("unexpected failure: %+v", fail)
	}
	for _, c := range res.Checks {
		if c.Name == "git on PATH" {
			t.Error("git checked even though it is not needed")
		}
	}
}

func TestRunNoToken(t *testing.T) {
	_, fail := Run(context.Background(), Options{
		Org:      "acme",
		Client:   newStub(t, &apiStub{}),
		LookPath: lookPathOK,
		Getenv:   env(nil),
		GHToken:  func(context.Context) (string, error) { return "", errors.New("no gh") },
	})
	if fail == nil {
		t.Fatal("want a failure")
	}
	if fail.Code != CodeAuth {
		t.Errorf("Code = %d, want %d", fail.Code, CodeAuth)
	}
	// Both remedies must be offered.
	if !strings.Contains(fail.Fix, "GH_TOKEN") || !strings.Contains(fail.Fix, "gh auth login") {
		t.Errorf("Fix = %q, want both remedies", fail.Fix)
	}
}

func TestRunUnauthorized(t *testing.T) {
	_, fail := Run(context.Background(), Options{
		Org:      "acme",
		Client:   newStub(t, &apiStub{userCode: 401}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "bad-token"}),
	})
	if fail == nil {
		t.Fatal("want a failure")
	}
	if fail.Code != CodeAuth {
		t.Errorf("Code = %d, want %d", fail.Code, CodeAuth)
	}
	if !strings.Contains(fail.Msg, "401") || !strings.Contains(fail.Msg, "Bad credentials") {
		t.Errorf("Msg = %q", fail.Msg)
	}
	if strings.Contains(fail.Msg+fail.Fix, "bad-token") {
		t.Error("failure leaked the token")
	}
}

func TestRunScopeChecks(t *testing.T) {
	tests := []struct {
		name      string
		scopes    string
		wantFail  bool
		wantMsg   string
		wantFix   string
		wantKnown bool
	}{
		{
			name: "complete", scopes: "repo, workflow, read:org", wantKnown: true,
		},
		{
			name: "missing workflow", scopes: "repo, read:org", wantFail: true, wantKnown: true,
			wantMsg: "token lacks 'workflow' scope; syncs touching workflow files will fail",
			wantFix: "gh auth refresh -s workflow",
		},
		{
			name: "missing repo", scopes: "workflow", wantFail: true, wantKnown: true,
			wantMsg: "token lacks 'repo' scope",
			wantFix: "gh auth refresh -s repo",
		},
		{
			name: "fine grained pat", scopes: "", wantKnown: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &apiStub{scopes: tc.scopes}
			res, fail := Run(context.Background(), Options{
				Org:      "acme",
				Client:   newStub(t, stub),
				LookPath: lookPathOK,
				Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
			})
			if tc.wantFail {
				if fail == nil {
					t.Fatal("want a failure")
				}
				if fail.Code != CodeAuth {
					t.Errorf("Code = %d, want %d", fail.Code, CodeAuth)
				}
				if !strings.Contains(fail.Msg, tc.wantMsg) {
					t.Errorf("Msg = %q, want it to contain %q", fail.Msg, tc.wantMsg)
				}
				if fail.Fix != tc.wantFix {
					t.Errorf("Fix = %q, want %q", fail.Fix, tc.wantFix)
				}
				if stub.orgCalled {
					t.Error("preflight continued past the scope check")
				}
				return
			}
			if fail != nil {
				t.Fatalf("unexpected failure: %+v", fail)
			}
			if res.ScopesKnown != tc.wantKnown {
				t.Errorf("ScopesKnown = %v, want %v", res.ScopesKnown, tc.wantKnown)
			}
			if !tc.wantKnown {
				var detail string
				for _, c := range res.Checks {
					if c.Name == "scopes" {
						detail = c.Detail
					}
				}
				if !strings.Contains(detail, "could not be verified in advance") {
					t.Errorf("scopes detail = %q, want the fine-grained note", detail)
				}
			}
		})
	}
}

func TestRunOrgNotVisible(t *testing.T) {
	for _, code := range []int{404, 403} {
		_, fail := Run(context.Background(), Options{
			Org:      "ghost",
			Client:   newStub(t, &apiStub{scopes: "repo, workflow", orgCode: code}),
			LookPath: lookPathOK,
			Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
		})
		if fail == nil {
			t.Fatalf("%d: want a failure", code)
		}
		if fail.Code != CodeOrg {
			t.Errorf("%d: Code = %d, want %d", code, fail.Code, CodeOrg)
		}
		// The message must say it could be either cause.
		if !strings.Contains(fail.Msg, "not found") || !strings.Contains(fail.Msg, "cannot see it") {
			t.Errorf("%d: Msg = %q, want both explanations", code, fail.Msg)
		}
		if !strings.Contains(fail.Msg, "read:org") {
			t.Errorf("%d: Msg = %q, want the read:org hint", code, fail.Msg)
		}
	}
}

func TestRunNoOrgConfigured(t *testing.T) {
	_, fail := Run(context.Background(), Options{
		Client:   newStub(t, &apiStub{scopes: "repo, workflow"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail == nil {
		t.Fatal("want a failure")
	}
	if fail.Code != 2 {
		t.Errorf("Code = %d, want 2", fail.Code)
	}
	if fail.Fix != "forkman configure --org=<name>" {
		t.Errorf("Fix = %q", fail.Fix)
	}
}

func TestRunAllChecksPass(t *testing.T) {
	res, fail := Run(context.Background(), Options{
		NeedGit:  true,
		Org:      "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, workflow, read:org"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"FORKMAN_TOKEN": "t"}),
	})
	if fail != nil {
		t.Fatalf("unexpected failure: %+v", fail)
	}
	var names []string
	for _, c := range res.Checks {
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
		names = append(names, c.Name)
	}
	want := []string{"git on PATH", "token", "authentication", "scopes", "organization"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("checks = %v, want %v", names, want)
	}
	if res.User == nil || res.User.Login != "octo" {
		t.Errorf("User = %+v", res.User)
	}
	if res.Token != "t" {
		t.Errorf("Token not carried through")
	}
	if res.Client == nil {
		t.Error("Client not returned")
	}
	// The token source is reported, not the token itself.
	for _, c := range res.Checks {
		if c.Name == "token" && c.Detail != "from FORKMAN_TOKEN" {
			t.Errorf("token detail = %q", c.Detail)
		}
	}
}

func TestFailureError(t *testing.T) {
	f := &Failure{Code: 4, Msg: "token lacks 'workflow' scope", Fix: "gh auth refresh -s workflow"}
	want := "token lacks 'workflow' scope\nFix: gh auth refresh -s workflow"
	if f.Error() != want {
		t.Errorf("Error() = %q, want %q", f.Error(), want)
	}
	if got := (&Failure{Msg: "plain"}).Error(); got != "plain" {
		t.Errorf("Error() = %q", got)
	}
}

func TestRunGitModeSkipsWorkflowScopeOverSSH(t *testing.T) {
	res, fail := Run(context.Background(), Options{
		GitMode:  true,
		Protocol: config.ProtoSSH,
		CloneDir: filepath.Join(t.TempDir(), "forks"),
		Org:      "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, read:org"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail != nil {
		t.Fatalf("unexpected failure: %+v", fail)
	}
	var detail string
	for _, c := range res.Checks {
		if c.Name == "scopes" {
			if !c.OK {
				t.Error("scopes check failed even though git mode does not need 'workflow'")
			}
			detail = c.Detail
		}
	}
	if detail != "not required in git mode (pushes go over git remote)" {
		t.Errorf("scopes detail = %q", detail)
	}
}

func TestRunGitModeStillNeedsWorkflowScopeOverHTTPS(t *testing.T) {
	_, fail := Run(context.Background(), Options{
		GitMode:  true,
		Protocol: config.ProtoHTTPS,
		CloneDir: filepath.Join(t.TempDir(), "forks"),
		Org:      "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, read:org"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail == nil {
		t.Fatal("want a failure: an https push of workflow files needs the scope")
	}
	if fail.Code != CodeAuth {
		t.Errorf("Code = %d, want %d", fail.Code, CodeAuth)
	}
	if !strings.Contains(fail.Fix, "--protocol ssh") {
		t.Errorf("Fix = %q, want it to offer ssh", fail.Fix)
	}
}

func TestRunGitModeRequiresGit(t *testing.T) {
	res, fail := Run(context.Background(), Options{
		GitMode:  true,
		Protocol: config.ProtoSSH,
		CloneDir: t.TempDir(),
		Org:      "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, workflow"}),
		LookPath: lookPathMissing,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail == nil || fail.Code != CodeGitMissing {
		t.Fatalf("fail = %+v, want exit %d", fail, CodeGitMissing)
	}
	if len(res.Checks) != 1 || res.Checks[0].Name != "git on PATH" {
		t.Errorf("Checks = %+v", res.Checks)
	}
}

func TestRunGitModeCloneDirChecks(t *testing.T) {
	// Creatable: preflight makes the directory itself.
	dir := filepath.Join(t.TempDir(), "a", "b", "forks")
	res, fail := Run(context.Background(), Options{
		GitMode:  true,
		Protocol: config.ProtoSSH,
		CloneDir: dir,
		Org:      "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, workflow"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail != nil {
		t.Fatalf("unexpected failure: %+v", fail)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("clone dir was not created: %v", err)
	}
	found := false
	for _, c := range res.Checks {
		if c.Name == "clone dir" {
			found = true
			if !c.OK || !strings.Contains(c.Detail, dir) {
				t.Errorf("clone dir check = %+v", c)
			}
		}
	}
	if !found {
		t.Error("no clone dir check in git mode")
	}

	// Unset: a configuration problem, exit 2.
	_, fail = Run(context.Background(), Options{
		GitMode: true, Protocol: config.ProtoSSH, Org: "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, workflow"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail == nil || fail.Code != CodeConfig {
		t.Fatalf("fail = %+v, want exit %d for an unset clone dir", fail, CodeConfig)
	}

	// Not creatable: a file sits where the directory should go.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fail = Run(context.Background(), Options{
		GitMode: true, Protocol: config.ProtoSSH, CloneDir: filepath.Join(blocked, "forks"), Org: "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, workflow"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail == nil || fail.Code != CodeConfig {
		t.Fatalf("fail = %+v, want exit %d for an unusable clone dir", fail, CodeConfig)
	}
}

func TestRunAPIModeIgnoresProtocolAndCloneDir(t *testing.T) {
	// Without GitMode nothing about git is checked, so the api path is
	// unchanged even when a protocol is configured.
	res, fail := Run(context.Background(), Options{
		Protocol: config.ProtoSSH,
		CloneDir: "/nonexistent/definitely/not/writable",
		Org:      "acme",
		Client:   newStub(t, &apiStub{scopes: "repo, read:org"}),
		LookPath: lookPathOK,
		Getenv:   env(map[string]string{"GH_TOKEN": "t"}),
	})
	if fail == nil {
		t.Fatal("want the usual workflow-scope failure in api mode")
	}
	for _, c := range res.Checks {
		if c.Name == "clone dir" || c.Name == "git on PATH" {
			t.Errorf("api mode ran the %q check", c.Name)
		}
	}
}
