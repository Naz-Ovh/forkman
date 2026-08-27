// Package preflight runs the environment and credential checks that must pass
// before forkman touches any repository.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/naz-ovh/forkman/internal/config"
	"github.com/naz-ovh/forkman/internal/github"
)

// Exit codes owned by preflight.
const (
	CodeConfig     = 2
	CodeGitMissing = 3
	CodeAuth       = 4
	CodeOrg        = 5
)

const ghTokenTimeout = 2 * time.Second

// The check names, which double as the labels a front-end shows while each
// probe runs.
const (
	checkGit      = "git on PATH"
	checkCloneDir = "clone dir"
	checkToken    = "token"
	checkAuth     = "authentication"
	checkScopes   = "scopes"
	checkOrg      = "organization"
)

// Check is one preflight probe, reported by `forkman doctor`.
type Check struct {
	Name   string
	OK     bool
	Detail string
	Fix    string
	Code   int // exit code to use when this check fails
}

// Failure is a fatal preflight result carrying the process exit code.
type Failure struct {
	Code int
	Msg  string
	Fix  string
}

func (f *Failure) Error() string {
	if f.Fix == "" {
		return f.Msg
	}
	return f.Msg + "\nFix: " + f.Fix
}

// Options configures Run. The function fields exist so tests can inject the
// environment without touching the real one.
type Options struct {
	NeedGit bool
	Org     string
	Version string

	// GitMode means syncs run through local clones and plain git, which needs
	// git on PATH and a writable clone directory but no 'workflow' scope.
	GitMode  bool
	Protocol string
	CloneDir string

	Client   *github.Client
	LookPath func(string) (string, error)
	Getenv   func(string) string
	GHToken  func(context.Context) (string, error)

	// Begin and Done, when set, report progress as the checks run: Begin with
	// the name of a check that is about to start, Done with the finished
	// check. They let a front-end show which probe is taking the time instead
	// of leaving the user in front of a silent terminal.
	Begin func(name string)
	Done  func(Check)
}

// Result is what a successful (or partially successful) preflight produced.
type Result struct {
	Checks      []Check
	Token       string
	Client      *github.Client
	User        *github.User
	ScopesKnown bool
}

// ResolveToken finds a GitHub token, preferring FORKMAN_TOKEN, then GH_TOKEN,
// then GITHUB_TOKEN, then `gh auth token`. source names where it came from.
func ResolveToken(ctx context.Context, getenv func(string) string, ghToken func(context.Context) (string, error)) (string, string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, name := range []string{"FORKMAN_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v, name, nil
		}
	}
	if ghToken == nil {
		ghToken = GHAuthToken
	}
	tok, err := ghToken(ctx)
	if err != nil {
		return "", "", err
	}
	if tok = strings.TrimSpace(tok); tok == "" {
		return "", "", errors.New("gh auth token returned nothing")
	}
	return tok, "gh auth token", nil
}

// GHAuthToken shells out to `gh auth token` with a short timeout.
func GHAuthToken(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTokenTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

const noTokenFix = "set a token: export GH_TOKEN=<token>   —or—   install gh and run: gh auth login"

// Run executes the checks in order, stopping at the first fatal one. The
// returned Result always holds every check attempted so `doctor` can print a
// full table.
func Run(ctx context.Context, opts Options) (*Result, *Failure) {
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	res := &Result{}
	begin := func(name string) {
		if opts.Begin != nil {
			opts.Begin(name)
		}
	}
	add := func(c Check) {
		res.Checks = append(res.Checks, c)
		if opts.Done != nil {
			opts.Done(c)
		}
	}

	if opts.NeedGit || opts.GitMode {
		begin(checkGit)
		path, err := lookPath("git")
		if err != nil {
			add(Check{Name: checkGit, Code: CodeGitMissing,
				Detail: "git not found on PATH",
				Fix:    "install git (e.g. `apt install git` / `brew install git`) and ensure it is on PATH"})
			return res, res.failure()
		}
		add(Check{Name: checkGit, OK: true, Detail: path})
	}

	if opts.GitMode {
		begin(checkCloneDir)
		if opts.CloneDir == "" {
			add(Check{Name: checkCloneDir, Code: CodeConfig,
				Detail: "git mode needs a clone directory to hold the forks",
				Fix:    "forkman configure --clone-dir=PATH"})
			return res, res.failure()
		}
		if err := ensureWritable(opts.CloneDir); err != nil {
			add(Check{Name: checkCloneDir, Code: CodeConfig,
				Detail: fmt.Sprintf("%s is not usable: %v", opts.CloneDir, err),
				Fix:    "pick a writable location: forkman configure --clone-dir=PATH"})
			return res, res.failure()
		}
		add(Check{Name: checkCloneDir, OK: true, Detail: opts.CloneDir + " (writable)"})
	}

	begin(checkToken)
	token, source, err := ResolveToken(ctx, opts.Getenv, opts.GHToken)
	if err != nil || token == "" {
		add(Check{Name: checkToken, Code: CodeAuth,
			Detail: "no GitHub token in FORKMAN_TOKEN, GH_TOKEN or GITHUB_TOKEN, and `gh auth token` failed",
			Fix:    noTokenFix})
		return res, res.failure()
	}
	res.Token = token
	add(Check{Name: checkToken, OK: true, Detail: "from " + source})

	client := opts.Client
	if client == nil {
		client = github.New(token, opts.Version)
	}
	res.Client = client

	begin(checkAuth)
	user, err := client.GetUser(ctx)
	if err != nil {
		var ae *github.APIError
		detail := err.Error()
		fix := ""
		if errors.As(err, &ae) && ae.Status == 401 {
			detail = fmt.Sprintf("GitHub rejected the token (401: %s)", ae.Message)
			fix = noTokenFix
		}
		add(Check{Name: checkAuth, Code: CodeAuth, Detail: detail, Fix: fix})
		return res, res.failure()
	}
	res.User = user
	add(Check{Name: checkAuth, OK: true, Detail: "authenticated as " + user.Login})

	begin(checkScopes)
	res.ScopesKnown = user.ScopesKnown
	if !user.ScopesKnown {
		add(Check{Name: checkScopes, OK: true,
			Detail: "token does not report scopes (fine-grained PAT); workflow-file capability could not be verified in advance"})
	} else {
		have := make(map[string]bool, len(user.Scopes))
		for _, s := range user.Scopes {
			have[s] = true
		}
		switch {
		case !have["repo"]:
			add(Check{Name: checkScopes, Code: CodeAuth,
				Detail: "token lacks 'repo' scope; forkman cannot read or sync private forks",
				Fix:    "gh auth refresh -s repo"})
			return res, res.failure()
		case !have["workflow"]:
			// A git-mode push over ssh carries no OAuth scopes at all, so the
			// workflow-file restriction simply does not apply.
			if opts.GitMode && opts.Protocol != config.ProtoHTTPS {
				add(Check{Name: checkScopes, OK: true,
					Detail: "not required in git mode (pushes go over git remote)"})
				break
			}
			c := Check{Name: checkScopes, Code: CodeAuth,
				Detail: "token lacks 'workflow' scope; syncs touching workflow files will fail",
				Fix:    "gh auth refresh -s workflow"}
			if opts.GitMode {
				c.Detail = "token lacks 'workflow' scope; an https git push of workflow files will be rejected"
				c.Fix = "gh auth refresh -s workflow   —or—   push over ssh: forkman configure --mode git --protocol ssh"
			}
			add(c)
			return res, res.failure()
		default:
			add(Check{Name: checkScopes, OK: true, Detail: strings.Join(user.Scopes, ", ")})
		}
	}

	begin(checkOrg)
	if opts.Org == "" {
		add(Check{Name: checkOrg, Code: CodeConfig,
			Detail: "no organization configured",
			Fix:    "forkman configure --org=<name>"})
		return res, res.failure()
	}
	if err := client.GetOrg(ctx, opts.Org); err != nil {
		var ae *github.APIError
		detail := err.Error()
		fix := ""
		if errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 403) {
			detail = fmt.Sprintf("organization %q not found, or this token cannot see it (a private org also returns 404 when the token lacks 'read:org')", opts.Org)
			fix = "check the name with `forkman configure --org=<name>`; if the org is private, run: gh auth refresh -s read:org"
		}
		add(Check{Name: checkOrg, Code: CodeOrg, Detail: detail, Fix: fix})
		return res, res.failure()
	}
	add(Check{Name: checkOrg, OK: true, Detail: opts.Org + " visible"})

	return res, nil
}

// ensureWritable creates dir if needed and proves forkman can write in it.
func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".forkman-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// failure converts the last (failed) check into a Failure.
func (r *Result) failure() *Failure {
	if len(r.Checks) == 0 {
		return nil
	}
	c := r.Checks[len(r.Checks)-1]
	if c.OK {
		return nil
	}
	return &Failure{Code: c.Code, Msg: c.Detail, Fix: c.Fix}
}
