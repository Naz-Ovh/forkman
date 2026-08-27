# forkman

A single-binary terminal app that keeps an organization's forks in sync with
their upstream parents, with an animated fullscreen TUI and a scriptable
plain-text mode.

Dependencies are a hard budget: bubbletea, bubbles and lipgloss. Everything
else is the Go standard library — the GitHub client is hand-rolled on
`net/http`, git is shelled out to, and config is JSON.

## Install

With the Go toolchain (recommended — no extra tooling, and it builds from the
tagged source you can read):

```sh
go install github.com/naz-ovh/forkman/cmd/forkman@latest
go install github.com/naz-ovh/forkman/cmd/forkman@v0.1.1   # or pin a release
```

The binary lands in `$(go env GOPATH)/bin`, so make sure that is on your `PATH`.
Any Go 1.21 or newer toolchain works: the `go 1.27.0` line in `go.mod` makes it
fetch Go 1.27 on its own. Installed this way `forkman --version` reports the
module version, which the toolchain records in the build info.

`go install` needs v0.1.1 or newer. v0.1.0 was published before the module path
was corrected, and the Go module proxy's copy of a version is immutable, so that
one tag can only be installed from a release archive or from source.

From a release archive: download `forkman_<version>_<os>_<arch>.tar.gz` (or `.zip` on Windows) from the
[releases page](https://github.com/naz-ovh/forkman/releases), verify against `checksums.txt`, and put `forkman` on your `PATH`.

With [bin](https://github.com/marcosnils/bin), if you already use it to track GitHub releases and update in place:

```sh
bin install github.com/naz-ovh/forkman
bin update forkman        # later
```

From source:

```sh
make build      # -> bin/forkman
```

Releases are built by [GoReleaser](https://goreleaser.com) (`build/goreleaser.yaml`) for `linux/{amd64,arm64}`,
`darwin/{amd64,arm64}` and `windows/amd64`, from both GitHub Actions (`.github/workflows/release.yml`) and
GitLab CI (`build/ci/gitlab-ci.yml`, set as the custom CI config path in the GitLab project settings) on any `v*` tag.

## Usage

```
forkman sync       [--plain] [--json] [--dry-run] [--concurrency N] [--mode api|git] [--dir PATH]
forkman clone      [--plain] [--json] [--dry-run] [--concurrency N] [--full] [--dir PATH]
forkman configure  [--org NAME] [--exclude a,b] [--exclude-add a,b] [--exclude-remove a,b]
                   [--concurrency N] [--clone-dir PATH] [--mode api|git] [--protocol ssh|https]
forkman doctor
forkman --version
```

First run:

```sh
forkman configure --org=my-org
forkman sync
```

`sync` calls the GitHub `merge-upstream` endpoint for every fork whose default
branch is behind its parent. Forks already at the parent's head are detected
during discovery and cost no further API calls. Diverged branches are reported
with the commands to resolve them locally; forkman never writes to upstream and
never attempts a conflict resolution.

`clone` gives you every fork's **history**, not its files: it clones with
`--filter=blob:none --no-checkout`, adds a push-disabled `upstream` remote and
fetches it. Each `<cloneDir>/<repoName>` therefore holds a complete commit and
tree history with zero file contents downloaded, and the folder looks empty —
the row logs `history only (blob:none, no checkout)` to say so. That is the
point: keeping fifty forks' commit graphs current costs a fraction of keeping
fifty working trees.

When you do want the files in one of them, ask git for them:

```sh
cd ~/src/forks/tempo && git checkout main   # blobs are fetched on demand
```

Or clone with working trees from the start with `forkman clone --full`, which is
a plain `git clone` — every blob, files checked out.

`doctor` runs every preflight check and prints a pass/fail table without doing
any work, including which sync mode is configured.

## Git mode (avoiding the `workflow` scope)

`merge-upstream` is an authenticated API call, so GitHub applies the OAuth
workflow-file rule to it: whenever the upstream commits being merged touch
`.github/workflows/`, a token without the `workflow` scope is rejected. The same
rule applies to an https push authenticated with that token. A push over **ssh**
carries no OAuth scopes at all, so it is not subject to the rule.

Git mode uses that: keep every fork cloned locally and sync with plain git.

```sh
forkman configure --mode git --protocol ssh --clone-dir ~/src/forks
forkman sync                       # or: forkman sync --mode git --dir /other/path
```

`cloneDir` is the folder that holds all the forks, one directory per repository
(`<cloneDir>/<repoName>`); it defaults to `~/src/forks` in git mode. It is
always stored and reported as an absolute path — `configure --clone-dir` expands
`~` and resolves a relative path against the working directory — and it is the
single source of truth for both `clone` and git-mode `sync`, with `--dir` as a
per-run override.

Discovery and planning still go through the API (read-only), so a fork already
at its parent's head is skipped with no git work at all. For every other fork
forkman runs, in `<cloneDir>/<repoName>`:

```sh
git clone --progress [--filter=blob:none --no-checkout] <fork-url> <dir>  # if missing
git remote add upstream <parent-url>                         # or remote set-url
git remote set-url --push upstream no_push
git remote set-url origin <fork-url>                         # keep the protocol
git fetch --progress upstream --prune
git fetch --progress origin --prune
git merge-base --is-ancestor refs/remotes/origin/<b> refs/remotes/upstream/<b>
git rev-list --count refs/remotes/origin/<b>..refs/remotes/upstream/<b>
git push origin refs/remotes/upstream/<b>:refs/heads/<b>
git fetch origin
git update-ref refs/heads/<b> <upstream-sha> <old-sha>        # empty worktree
git merge --ff-only refs/remotes/upstream/<b>                 # populated, clean
```

Every invocation runs with `GIT_TERMINAL_PROMPT=0` and is cancelled with the
run. The push comes straight from the fetched upstream ref, so no working-tree
checkout is needed to sync — which is what makes the history-only clone above
enough for git mode. The last step then brings the local clone along:

- nothing checked out (the default clone): the branch ref is moved with
  `update-ref`, which is given the old sha so a concurrent change makes it fail
  rather than clobber, and only after `merge-base --is-ancestor` proves it is a
  fast-forward. The row logs `local branch fast-forwarded (no checkout; run git
  checkout <b> for files)`.
- files checked out (a `--full` clone, or you ran `git checkout`): a real
  `merge --ff-only`, but only when HEAD is on `<b>` with nothing uncommitted.
  Otherwise the row logs `local checkout not updated (branch/dirty)` and leaves
  your work alone — the push has already succeeded either way.

If `merge-base --is-ancestor` says the fork has commits of its own, the row is
reported as `diverged` with the ahead/behind counts and the same resolution
commands as api mode. **forkman never force-pushes.** A rejected push (protected
branch, pre-receive hook, missing ssh key) is a `failed` row whose expanded log
holds git's stderr verbatim.

With `--protocol https` the push is token-authenticated again, so the
`workflow` scope is required and preflight still insists on it.

## Configuration

`$XDG_CONFIG_HOME/forkman/config.json`, else `~/.config/forkman/config.json`
(directory `0700`, file `0600`):

```json
{
  "version": 1,
  "org": "0x-fork",
  "excluded": ["some-repo", "test-*"],
  "concurrency": 4,
  "defaultBranchOnly": true,
  "cloneDir": "/home/you/src/forks",
  "syncMode": "api",
  "protocol": "ssh"
}
```

`excluded` matches the bare repository name, case-insensitively, with an
optional trailing `*`. Unknown keys and unknown versions are errors.
`concurrency` is clamped to 1–16 (default 4). `syncMode` is `api` (default) or
`git`; `protocol` is `ssh` (default) or `https` and shapes the remote URLs in
git mode. Any other value is a config error (exit 2).

Running `forkman configure` with no flags prints the current settings and opens
the interactive exclusion selector.

## Authentication

The token is taken from the first of `FORKMAN_TOKEN`, `GH_TOKEN`,
`GITHUB_TOKEN`, or `gh auth token`. A classic PAT needs the `repo` and
`workflow` scopes; forkman refuses to start without them and prints the exact
`gh auth refresh` command. In git mode over ssh the `workflow` scope is not
required and preflight says so; `repo` is still needed to discover the forks. Fine-grained PATs do not report scopes, so the
workflow-file capability cannot be verified in advance and the summary says so.
The token is never printed, logged or included in any error.

## Output and environment

Output falls back to one line per repository when stdout is not a TTY, or with
`--plain`; `--json` emits one JSON object per line.

| Variable | Effect |
| --- | --- |
| `NO_COLOR` | non-empty disables colour |
| `FORCE_COLOR` | non-empty re-enables colour |
| `FORKMAN_DEBUG=1` | debug logging to stderr (token redacted) |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success, or everything already current |
| 1 | internal error |
| 2 | missing or invalid config |
| 3 | git not on PATH |
| 4 | authentication or scope problem |
| 5 | organization not accessible |
| 6 | run completed but at least one repository failed |
| 130 | interrupted |

## Development

```sh
make vet test deps-check build size
```

`deps-check` fails if go.mod gains a direct dependency outside the allowlist.
