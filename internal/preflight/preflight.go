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

	"forkman/internal/config"
	"forkman/internal/github"
)

// Exit codes owned by preflight.
const (
	CodeConfig     = 2
	CodeGitMissing = 3
	CodeAuth       = 4
	CodeOrg        = 5
)

const ghTokenTimeout = 2 * time.Second

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

	if opts.NeedGit || opts.GitMode {
		path, err := lookPath("git")
		if err != nil {
			res.add(Check{Name: "git on PATH", Code: CodeGitMissing,
				Detail: "git not found on PATH",
				Fix:    "install git (e.g. `apt install git` / `brew install git`) and ensure it is on PATH"})
			return res, res.failure()
		}
		res.add(Check{Name: "git on PATH", OK: true, Detail: path})
	}

	if opts.GitMode {
		if opts.CloneDir == "" {
			res.add(Check{Name: "clone dir", Code: CodeConfig,
				Detail: "git mode needs a clone directory to hold the forks",
				Fix:    "forkman configure --clone-dir=PATH"})
			return res, res.failure()
		}
		if err := ensureWritable(opts.CloneDir); err != nil {
			res.add(Check{Name: "clone dir", Code: CodeConfig,
				Detail: fmt.Sprintf("%s is not usable: %v", opts.CloneDir, err),
				Fix:    "pick a writable location: forkman configure --clone-dir=PATH"})
			return res, res.failure()
		}
		res.add(Check{Name: "clone dir", OK: true, Detail: opts.CloneDir + " (writable)"})
	}

	token, source, err := ResolveToken(ctx, opts.Getenv, opts.GHToken)
	if err != nil || token == "" {
		res.add(Check{Name: "token", Code: CodeAuth,
			Detail: "no GitHub token in FORKMAN_TOKEN, GH_TOKEN or GITHUB_TOKEN, and `gh auth token` failed",
			Fix:    noTokenFix})
		return res, res.failure()
	}
	res.Token = token
	res.add(Check{Name: "token", OK: true, Detail: "from " + source})

	client := opts.Client
	if client == nil {
		client = github.New(token, opts.Version)
	}
	res.Client = client

	user, err := client.GetUser(ctx)
	if err != nil {
		var ae *github.APIError
		detail := err.Error()
		fix := ""
		if errors.As(err, &ae) && ae.Status == 401 {
			detail = fmt.Sprintf("GitHub rejected the token (401: %s)", ae.Message)
			fix = noTokenFix
		}
		res.add(Check{Name: "authentication", Code: CodeAuth, Detail: detail, Fix: fix})
		return res, res.failure()
	}
	res.User = user
	res.add(Check{Name: "authentication", OK: true, Detail: "authenticated as " + user.Login})

	res.ScopesKnown = user.ScopesKnown
	if !user.ScopesKnown {
		res.add(Check{Name: "scopes", OK: true,
			Detail: "token does not report scopes (fine-grained PAT); workflow-file capability could not be verified in advance"})
	} else {
		have := make(map[string]bool, len(user.Scopes))
		for _, s := range user.Scopes {
			have[s] = true
		}
		switch {
		case !have["repo"]:
			res.add(Check{Name: "scopes", Code: CodeAuth,
				Detail: "token lacks 'repo' scope; forkman cannot read or sync private forks",
				Fix:    "gh auth refresh -s repo"})
			return res, res.failure()
		case !have["workflow"]:
			// A git-mode push over ssh carries no OAuth scopes at all, so the
			// workflow-file restriction simply does not apply.
			if opts.GitMode && opts.Protocol != config.ProtoHTTPS {
				res.add(Check{Name: "scopes", OK: true,
					Detail: "not required in git mode (pushes go over git remote)"})
				break
			}
			c := Check{Name: "scopes", Code: CodeAuth,
				Detail: "token lacks 'workflow' scope; syncs touching workflow files will fail",
				Fix:    "gh auth refresh -s workflow"}
			if opts.GitMode {
				c.Detail = "token lacks 'workflow' scope; an https git push of workflow files will be rejected"
				c.Fix = "gh auth refresh -s workflow   —or—   push over ssh: forkman configure --mode git --protocol ssh"
			}
			res.add(c)
			return res, res.failure()
		default:
			res.add(Check{Name: "scopes", OK: true, Detail: strings.Join(user.Scopes, ", ")})
		}
	}

	if opts.Org == "" {
		res.add(Check{Name: "organization", Code: CodeConfig,
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
		res.add(Check{Name: "organization", Code: CodeOrg, Detail: detail, Fix: fix})
		return res, res.failure()
	}
	res.add(Check{Name: "organization", OK: true, Detail: opts.Org + " visible"})

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

func (r *Result) add(c Check) { r.Checks = append(r.Checks, c) }

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
