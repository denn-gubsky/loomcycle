// Package helper runs the operator-configured helper commands transcription
// relies on — a PDF rasteriser (`pdftoppm -r 150 -png {in} {out_prefix}`), a
// media converter (`ffmpeg -i {in} -ac 1 -ar 16000 -f wav {out}`) — on the
// host, without a shell.
//
// The command line is the operator's: Spec.Template comes from configuration,
// never from a model. What varies per call are the placeholder values (paths
// the runtime derived, a language code), and those are the untrusted part: a
// value fills exactly one argument, or part of one, and can never add an
// argument, start a shell construct or choose the program (template.go has
// the quoting rules).
//
// The child runs in an existing directory fenced under an operator root, with
// a scrubbed environment (PATH plus operator-allowlisted names, never a
// loomcycle setting or a secret), in its own process group so a timeout kills
// everything it started, and with its output captured up to a bound.
//
// Not a sandbox by itself: like the Bash tool and the Bashbox host fallback,
// the helper runs as the loomcycle user and can read any file the process
// can. The fence decides where it starts, not what it reaches. An operator
// confines it with a wrapper (Spec.Wrapper, LOOMCYCLE_HELPER_WRAPPER: bwrap,
// firejail), which runs the helper as its argv. ffmpeg is always
// run with -nostdin and its inputs pinned to the file and pipe protocols
// (ffmpegDefaults), so a hostile recording cannot open a URL or a nested
// protocol.
package helper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hostenv"
	"github.com/denn-gubsky/loomcycle/internal/procgroup"
)

// Defaults for a Spec that leaves a bound unset.
const (
	DefaultTimeout        = 5 * time.Minute
	DefaultMaxOutputBytes = 1 << 20
)

// Spec is one helper-command invocation.
type Spec struct {
	// Key names the helper in operator configuration (for example
	// "transcription.stt.media"). Every error names it, so an operator knows
	// which setting to fix.
	Key string
	// Template is the operator's command line. See template.go.
	Template string
	// Vars are the placeholder values. Every placeholder in Template must have
	// one; a value may be empty, never hold NUL, and never begin an argument
	// with '-'. Paths among them are the caller's to fence.
	Vars map[string]string
	// Root is the operator-configured directory the working directory is
	// fenced to (an inbox's dynamic root). It is the only path resolved
	// through symlinks.
	Root string
	// Dir is the working directory: an absolute path named lexically under
	// Root (Root itself is allowed), reached without following a link below
	// Root, and an existing directory.
	Dir string
	// Timeout bounds the run; 0 is DefaultTimeout. On expiry the helper's
	// whole process group is killed.
	Timeout time.Duration
	// MaxOutputBytes bounds what is kept of each of stdout and stderr; 0 is
	// DefaultMaxOutputBytes. Output past it is read and dropped, so a chatty
	// helper is not blocked or killed for it.
	MaxOutputBytes int
	// ExtraEnv names host environment variables the child receives besides
	// PATH (operator-allowlisted, e.g. TESSDATA_PREFIX). A loomcycle setting
	// (LOOMCYCLE_*) or a secret-shaped name (*_KEY, *_TOKEN, *_SECRET, *_AUTH,
	// *_PASSWORD, *_CREDENTIAL(S), *_DSN) is refused: a helper converts files
	// and needs none.
	ExtraEnv []string
	// Wrapper is the operator's confinement command (see ParseWrapper). When
	// set, it runs with the helper's resolved program and arguments appended
	// as further argv elements. Empty runs the helper directly.
	Wrapper string
}

// Result is what a started helper produced.
type Result struct {
	// ExitCode is the helper's exit status; -1 when it was killed.
	ExitCode        int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	Duration        time.Duration
}

var (
	// ErrNotFound: the template's program is not installed on this host.
	ErrNotFound = errors.New("helper program not found")
	// ErrTimeout: the helper ran past Spec.Timeout and was killed.
	ErrTimeout = errors.New("helper command timed out")
)

// ExitError is a helper that ran and exited non-zero. Its output is in the
// Result returned beside it.
type ExitError struct {
	Key  string
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s: the helper command exited with code %d", e.Key, e.Code)
}

// Run renders spec's template into argv, checks the working directory and the
// environment, and runs the program. The error is nil only for an exit status
// of 0. When the program started, Result holds what it wrote, whatever the
// error.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if spec.Key == "" {
		return Result{}, errors.New("helper: a spec needs the Key that names it in configuration")
	}
	words, err := parseTemplate(spec.Template)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", spec.Key, err)
	}
	argv, err := expand(words, spec.Vars)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", spec.Key, err)
	}
	for _, name := range spec.ExtraEnv {
		if err := checkEnvName(name); err != nil {
			return Result{}, fmt.Errorf("%s: %w", spec.Key, err)
		}
	}
	dir, err := workDir(spec.Root, spec.Dir)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", spec.Key, err)
	}
	argv = ffmpegDefaults(argv)
	// Resolved against this process's PATH, which is the PATH the child gets.
	prog, err := exec.LookPath(argv[0])
	if err != nil {
		return Result{}, fmt.Errorf("%s: %q is not installed on this host — use the loomcycle-media image or set %s to a command that exists here: %w",
			spec.Key, argv[0], spec.Key, ErrNotFound)
	}
	args := argv[1:]
	if spec.Wrapper != "" {
		wrap, err := ParseWrapper(spec.Wrapper)
		if err != nil {
			return Result{}, fmt.Errorf("%s: LOOMCYCLE_HELPER_WRAPPER: %w", spec.Key, err)
		}
		wprog, err := exec.LookPath(wrap[0])
		if err != nil {
			return Result{}, fmt.Errorf("%s: the helper wrapper %q is not installed on this host: %w", spec.Key, wrap[0], ErrNotFound)
		}
		// The helper by its resolved path, so the wrapper runs exactly the
		// program found here whatever PATH it gives its child.
		args = append(append(wrap[1:len(wrap):len(wrap)], prog), args...)
		prog = wprog
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := spec.MaxOutputBytes
	if limit <= 0 {
		limit = DefaultMaxOutputBytes
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr := &cappedBuffer{max: limit}, &cappedBuffer{max: limit}
	// #nosec G204 -- prog is the operator-configured helper (ffmpeg, pdftoppm), or the operator's wrapper with the helper's resolved path in args, run without a shell, scrubbed env, fenced cwd, capped output and a timeout
	cmd := exec.CommandContext(runCtx, prog, args...)
	cmd.Dir = dir
	cmd.Env = hostenv.Scrubbed(spec.ExtraEnv)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Own process group; a timeout or cancel SIGKILLs the whole group. A
	// descendant that calls setsid() leaves it — the documented residual of
	// every host-exec path.
	procgroup.Bind(cmd)

	start := time.Now()
	runErr := cmd.Run()
	res := Result{
		ExitCode: cmd.ProcessState.ExitCode(), Duration: time.Since(start),
		Stdout: stdout.buf, Stderr: stderr.buf,
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
	}
	switch {
	case ctx.Err() != nil:
		return res, fmt.Errorf("%s: %w", spec.Key, ctx.Err())
	case runCtx.Err() != nil:
		return res, fmt.Errorf("%s: after %v: %w", spec.Key, timeout, ErrTimeout)
	case runErr == nil, errors.Is(runErr, exec.ErrWaitDelay):
		// ErrWaitDelay: the program exited 0, and a background process it
		// started still held the output when the wait bound closed it.
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return res, &ExitError{Key: spec.Key, Code: exitErr.ExitCode()}
	}
	return res, fmt.Errorf("%s: %w", spec.Key, runErr)
}

// ParseWrapper splits an operator's wrapper command into argv by the
// template rules (template.go): no shell, quoting as in a helper template. It
// may hold no placeholder: what follows it is the helper's own argv, appended
// whole.
func ParseWrapper(wrapper string) ([]string, error) {
	words, err := parseTemplate(wrapper)
	if err != nil {
		return nil, err
	}
	argv := make([]string, 0, len(words))
	for _, w := range words {
		var b strings.Builder
		for _, seg := range w {
			if seg.placeholder {
				return nil, fmt.Errorf("the wrapper may not hold a placeholder ({%s}); the helper's command is appended to it", seg.text)
			}
			b.WriteString(seg.text)
		}
		argv = append(argv, b.String())
	}
	return argv, nil
}

// ffmpegDefaults adds, to an ffmpeg command line, -nostdin and, before each
// input, -protocol_whitelist file,pipe. The inputs the runtime passes are
// files it wrote; the whitelist is inherited by every nested open, so a
// recording that is really a playlist or a concat list cannot fetch a URL or
// reach the protocols that re-read other files (concat:, subfile:, data:,
// http:). An option the operator already wrote is left as it is. Only the
// template writes "-i": a value may never begin an argument with '-'.
func ffmpegDefaults(argv []string) []string {
	if filepath.Base(argv[0]) != "ffmpeg" {
		return argv
	}
	rest := argv[1:]
	out := []string{argv[0]}
	if !slices.Contains(rest, "-nostdin") {
		out = append(out, "-nostdin")
	}
	pin := !slices.Contains(rest, "-protocol_whitelist")
	for _, a := range rest {
		if pin && a == "-i" {
			out = append(out, "-protocol_whitelist", "file,pipe")
		}
		out = append(out, a)
	}
	return out
}

// FFmpegDefaultsSkipped reports why ffmpegDefaults would leave template's
// ffmpeg as written, or "" when they apply (or the template runs no ffmpeg,
// or does not parse, which validation reports on its own). The defaults key
// on the program being ffmpeg and on the template leaving the whitelist to
// them, so a template that runs ffmpeg behind another program (nice,
// timeout) or sets -protocol_whitelist itself gets neither.
func FFmpegDefaultsSkipped(template string) string {
	words, err := parseTemplate(template)
	if err != nil {
		return ""
	}
	argv := make([]string, len(words))
	for i, w := range words {
		for _, seg := range w {
			if !seg.placeholder {
				argv[i] += seg.text
			}
		}
	}
	if filepath.Base(argv[0]) == "ffmpeg" {
		if slices.Contains(argv[1:], "-protocol_whitelist") {
			return "the template sets -protocol_whitelist itself"
		}
		return ""
	}
	for _, a := range argv[1:] {
		if filepath.Base(a) == "ffmpeg" {
			return fmt.Sprintf("the template runs ffmpeg behind %s", filepath.Base(argv[0]))
		}
	}
	return ""
}

// workDir fences dir under root. Only root — operator configuration — is
// resolved through symlinks. The part of dir below it is derived lexically and
// walked one component at a time with Lstat, refusing a link, so a link
// planted below the root (in a run-writable inbox) cannot move the helper
// somewhere else.
//
// Residual: the check precedes exec, so a component swapped for a link between
// the two is not caught. The writers below an inbox root are the runtime and
// the run's own tools; the helper still only starts there.
func workDir(root, dir string) (string, error) {
	if root == "" {
		return "", errors.New("no root to fence the working directory to")
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("the root %q and the working directory %q must be absolute", root, dir)
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the working directory %q is not under %q", dir, root)
	}
	cur, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("the root %q: %w", root, err)
	}
	if fi, err := os.Stat(cur); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("the root %q is not a directory", root)
	}
	if rel == "." {
		return cur, nil
	}
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		switch {
		case err != nil:
			return "", fmt.Errorf("the working directory %q: %w", dir, err)
		case fi.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("the working directory %q passes through a link (%s)", dir, c)
		case !fi.IsDir():
			return "", fmt.Errorf("the working directory %q is not a directory", dir)
		}
	}
	return cur, nil
}

// secretSuffixes classify a name as a secret. They mirror
// config.IsSecretEnvName (a drift guard in the tests holds them to it); the
// copy keeps this package an import leaf.
var secretSuffixes = []string{"_KEY", "_TOKEN", "_SECRET", "_AUTH", "_PASSWORD", "_CREDENTIAL", "_CREDENTIALS", "_DSN"}

// checkEnvName refuses a name a helper must not receive.
func checkEnvName(name string) error {
	if !validName(name) {
		return fmt.Errorf("%q is not an environment variable name", name)
	}
	up := strings.ToUpper(name)
	switch {
	case up == "PATH":
		return errors.New("PATH is always passed; do not list it")
	case strings.HasPrefix(up, "LOOMCYCLE_"):
		return fmt.Errorf("%s is a loomcycle setting and is never passed to a helper", name)
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(up, s) {
			return fmt.Errorf("%s looks like a secret and is never passed to a helper", name)
		}
	}
	return nil
}

// cappedBuffer keeps the first max bytes written to it and drops the rest,
// reporting every write as complete so the child is never blocked on it.
type cappedBuffer struct {
	buf       []byte
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
			c.truncated = true
		} else {
			c.buf = append(c.buf, p...)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}
