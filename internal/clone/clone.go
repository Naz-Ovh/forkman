// Package clone shells out to git to create and wire up local clones of forks.
package clone

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Options describes one clone job.
type Options struct {
	ForkURL     string
	UpstreamURL string
	Dir         string
	Full        bool
	Git         string // git binary, defaults to "git"
}

// historyOnlyLine explains the empty folder a default clone leaves behind.
const historyOnlyLine = "history only (blob:none, no checkout)"

// IsRepo reports whether dir already holds a git working tree.
func IsRepo(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	// A bare repo or a worktree file both still count as "already cloned".
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return true
	}
	return false
}

// Run clones the fork if needed, then makes sure the upstream remote exists,
// is push-disabled, and is fetched. Every line git prints is handed to onLine
// and every progress percentage to onPercent. onLine's replace argument is
// true when the line is an in-place update of the line before it, exactly as
// a terminal would redraw it (see scanProgress).
func Run(ctx context.Context, o Options, onLine func(line string, replace bool), onPercent func(float64)) error {
	onLine, onPercent = callbacks(onLine, onPercent)
	if err := EnsureClone(ctx, o, onLine, onPercent); err != nil {
		return err
	}
	if o.UpstreamURL != "" {
		if err := Stream(ctx, o, onLine, onPercent, "fetch", "--progress", "upstream", "--prune"); err != nil {
			return err
		}
	}
	onPercent(1)
	return nil
}

// EnsureClone clones o.Dir from o.ForkURL when it is missing, then makes the
// upstream remote exist, point at o.UpstreamURL and refuse pushes. It is
// idempotent and does not fetch.
func EnsureClone(ctx context.Context, o Options, onLine func(line string, replace bool), onPercent func(float64)) error {
	if o.Dir == "" {
		return fmt.Errorf("clone: empty target directory")
	}
	onLine, onPercent = callbacks(onLine, onPercent)

	if !IsRepo(o.Dir) {
		if err := os.MkdirAll(filepath.Dir(o.Dir), 0o755); err != nil {
			return fmt.Errorf("create clone parent: %w", err)
		}
		args := []string{"clone", "--progress"}
		if !o.Full {
			// History without file contents: no blobs are downloaded and
			// nothing is checked out, so the folder holds only .git.
			args = append(args, "--filter=blob:none", "--no-checkout")
		}
		args = append(args, o.ForkURL, o.Dir)
		// The clone itself runs outside the target directory.
		if err := run(ctx, gitBin(o), "", args, onLine, onPercent); err != nil {
			return err
		}
		if !o.Full {
			onLine(historyOnlyLine, false)
		}
	} else {
		onLine("already cloned; refreshing upstream", false)
	}

	if o.UpstreamURL == "" {
		return nil
	}
	if err := Stream(ctx, o, onLine, onPercent, "remote", "add", "upstream", o.UpstreamURL); err != nil {
		// Remote already exists: point it at the right place instead.
		if err2 := Stream(ctx, o, onLine, onPercent, "remote", "set-url", "upstream", o.UpstreamURL); err2 != nil {
			return err
		}
	}
	return Stream(ctx, o, onLine, onPercent, "remote", "set-url", "--push", "upstream", "no_push")
}

// Stream runs one git subcommand in o.Dir, forwarding every output line to
// onLine and every progress percentage to onPercent.
func Stream(ctx context.Context, o Options, onLine func(line string, replace bool), onPercent func(float64), args ...string) error {
	onLine, onPercent = callbacks(onLine, onPercent)
	return run(ctx, gitBin(o), o.Dir, args, onLine, onPercent)
}

// GitResult is the captured outcome of one git invocation.
type GitResult struct {
	Stdout string
	Stderr []string
	Code   int // git's exit status; -1 when git could not be run at all
}

// OK reports a zero exit status.
func (g GitResult) OK() bool { return g.Code == 0 }

// Reason returns the first stderr line worth showing a user, with git's
// "error: "/"remote: " noise stripped.
func (g GitResult) Reason() string { return Reason(g.Stderr) }

// Git runs one git subcommand in o.Dir and captures its output instead of
// streaming it. Use it for the queries whose answer is the exit status or a
// single line of stdout.
func Git(ctx context.Context, o Options, args ...string) GitResult {
	cmd := exec.CommandContext(ctx, gitBin(o), args...)
	cmd.Dir = o.Dir
	cmd.Env = gitEnv()
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	err := cmd.Run()
	var res GitResult
	res.Stdout = strings.TrimSpace(out.String())
	res.Stderr = splitLines(errBuf.String())
	switch {
	case err == nil:
		res.Code = 0
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitCode()
			break
		}
		res.Code = -1
		res.Stderr = append(res.Stderr, err.Error())
	}
	return res
}

// noiseRe matches git's progress and bookkeeping chatter, which never explains
// why a command failed.
var noiseRe = regexp.MustCompile(`^(Enumerating|Counting|Delta compression|Compressing|Writing|Receiving|Resolving|Updating|Total|To |From |hint:|warning:|Cloning into|Fetching|Everything up-to-date|Already up to date|\* \[new)`)

// Reason picks the line most likely to explain a git failure: what the remote
// said, else the first error git itself reported.
func Reason(lines []string) string {
	fallback := ""
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || percentRe.MatchString(l) || noiseRe.MatchString(l) {
			continue
		}
		if rest, ok := cut(l, "remote:"); ok {
			if rest != "" {
				return rest
			}
			continue
		}
		if fallback != "" {
			continue
		}
		for _, p := range []string{"error:", "fatal:", "!"} {
			if rest, ok := cut(l, p); ok {
				l = rest
				break
			}
		}
		fallback = l
	}
	return fallback
}

func cut(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return line, false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
}

// splitLines turns captured output into the lines a terminal would have been
// left showing: an \r-separated progress phase collapses to its final state.
func splitLines(s string) []string {
	var out []string
	scanProgress(strings.NewReader(s), func(line string, replace bool) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		if replace && len(out) > 0 {
			out[len(out)-1] = line
			return
		}
		out = append(out, line)
	})
	return out
}

func gitBin(o Options) string {
	if o.Git == "" {
		return "git"
	}
	return o.Git
}

func gitEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never")
}

func callbacks(onLine func(string, bool), onPercent func(float64)) (func(string, bool), func(float64)) {
	if onLine == nil {
		onLine = func(string, bool) {}
	}
	if onPercent == nil {
		onPercent = func(float64) {}
	}
	return onLine, onPercent
}

var percentRe = regexp.MustCompile(`(?:Receiving objects|Resolving deltas|Updating files|Counting objects|Compressing objects):\s+(\d{1,3})%`)

func run(ctx context.Context, git, dir string, args []string, onLine func(string, bool), onPercent func(float64)) error {
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()

	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}

	var tail lineTail
	var mu sync.Mutex // git writes to both pipes; keep callbacks serialised
	done := make(chan struct{}, 2)
	consume := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		scanProgress(r, func(line string, replace bool) {
			mu.Lock()
			defer mu.Unlock()
			if m := percentRe.FindStringSubmatch(line); m != nil {
				if n, cerr := strconv.Atoi(m[1]); cerr == nil {
					onPercent(float64(n) / 100)
				}
			}
			tail.add(line)
			onLine(line, replace)
		})
	}
	go consume(out)
	go consume(errPipe)
	<-done
	<-done

	if err := cmd.Wait(); err != nil {
		if last := tail.last(); last != "" {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, last)
		}
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// splitLinesCR splits on \n and \r, which is how git emits progress updates,
// and keeps the terminator so the caller can tell the two apart.
func splitLinesCR(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// scanProgress reads git's output the way a terminal draws it. git separates
// progress updates with \r, which leaves the cursor on the same line: the next
// fragment overwrites what is already there. Only \n finishes a line. Each
// fragment is reported with replace=true when it overwrites the fragment
// before it, so a hundred "Receiving objects: NN%" updates collapse back into
// the single line a user would have seen.
func scanProgress(r io.Reader, emit func(line string, replace bool)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	sc.Split(splitLinesCR)
	overwrites := false
	for sc.Scan() {
		raw := sc.Bytes()
		var term byte
		if n := len(raw); n > 0 && (raw[n-1] == '\r' || raw[n-1] == '\n') {
			term, raw = raw[n-1], raw[:n-1]
		}
		line := strings.TrimRight(string(raw), " \t")
		if line == "" {
			// A bare \n still finishes the line the cursor sits on; this is
			// what keeps git's "\r…, done.\n" from adding an empty line.
			if term == '\n' {
				overwrites = false
			}
			continue
		}
		emit(line, overwrites)
		overwrites = term == '\r'
	}
}

// lineTail keeps the most recent non-progress line for error messages.
type lineTail struct{ s string }

func (t *lineTail) add(line string) {
	if percentRe.MatchString(line) {
		return
	}
	t.s = line
}

func (t *lineTail) last() string { return t.s }
