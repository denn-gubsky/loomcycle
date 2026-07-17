package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// workDir is the fixed in-container workspace. It is a size-capped tmpfs, so the
// whole workspace is in RAM and vanishes when the container is removed ("in-memory
// for safety"). All toolchain caches are redirected here (see sessionEnv) so a
// read-only rootfs still supports real compiles.
const workDir = "/work"

// Runner executes a podman argv and returns its combined (stdout+stderr) output,
// bounded to maxOut bytes, plus the process exit code. Abstracted so the arg
// construction is unit-testable without a podman host.
type Runner interface {
	Run(ctx context.Context, stdin []byte, maxOut int64, argv ...string) (out []byte, exitCode int, err error)
}

// execRunner is the production Runner backed by os/exec.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, stdin []byte, maxOut int64, argv ...string) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, -1, errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	bb := &boundedBuf{cap: maxOut}
	cmd.Stdout = bb
	cmd.Stderr = bb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			err = nil // a non-zero exit is data, not a runner failure
		} else {
			code = -1 // failed to start / killed by ctx / not found
		}
	}
	return bb.Bytes(), code, err
}

// Engine builds + runs podman commands for the session lifecycle.
type Engine struct {
	cfg *Config
	run Runner
}

func NewEngine(cfg *Config, run Runner) *Engine { return &Engine{cfg: cfg, run: run} }

// openOpts are the clamped, validated parameters for a new session.
type openOpts struct {
	Network string // "none" | "egress"
	TmpfsMB int64
	CPUs    float64
	MemMB   int64
	Pids    int64
	Image   string
	// WorkspaceHostDir, when set, is a persistent host directory bind-mounted at
	// /work (durable workspace, RFC BI P2a) instead of the in-memory tmpfs. It is
	// a resolved + fenced path (see Dispatcher.resolveWorkspaceDir) — never a raw
	// caller value.
	WorkspaceHostDir string
}

// runArgs builds the `podman run` argv for a session container. Pure + exported
// to the test via engine_test.go — this is where the hardening posture lives, so
// it is the thing worth asserting.
func (e *Engine) runArgs(name string, o openOpts) []string {
	a := []string{"run", "-d", "--name", name,
		"--label", "loomcycle.managed=1",
		"--label", "sandbox.session=" + name,
	}
	// Isolation runtime (gVisor/kata) when the operator set one.
	if e.cfg.Runtime != "" {
		a = append(a, "--runtime", e.cfg.Runtime)
	}
	// Network: off by default; a filtered bridge only when the operator opted
	// in AND the caller asked for it.
	if o.Network == "egress" && e.cfg.AllowEgress {
		a = append(a, "--network", "bridge")
	} else {
		a = append(a, "--network", "none")
	}
	// Hardening: read-only rootfs; the only writable surfaces are /work and /tmp;
	// drop every capability; no privilege escalation; non-root user; resource caps.
	a = append(a, "--read-only")
	// /work is either an in-memory, size-capped tmpfs (default — vanishes with the
	// container) or a persistent bind-mounted workspace (RFC BI P2a — durable across
	// container churn) when the caller requested one and the operator enabled
	// SANDBOX_WORKSPACE_ROOT.
	if o.WorkspaceHostDir != "" {
		a = append(a, "-v", o.WorkspaceHostDir+":"+workDir+":rw")
	} else {
		a = append(a, "--tmpfs", fmt.Sprintf("%s:rw,size=%dm,mode=0700,exec", workDir, o.TmpfsMB))
	}
	a = append(a,
		"--tmpfs", "/tmp:rw,size=64m,exec",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", e.cfg.CtrUser,
		"--pids-limit", strconv.FormatInt(o.Pids, 10),
		"--memory", fmt.Sprintf("%dm", o.MemMB),
		"--cpus", strconv.FormatFloat(o.CPUs, 'f', -1, 64),
		"--workdir", workDir,
	)
	for _, kv := range sessionEnv() {
		a = append(a, "--env", kv)
	}
	a = append(a, o.Image, "sleep", "infinity")
	return a
}

// sessionEnv redirects every toolchain's writable state into the tmpfs workspace
// so a --read-only rootfs still supports go build / cargo / npm / pip.
func sessionEnv() []string {
	return []string{
		"HOME=" + workDir,
		"TMPDIR=/tmp",
		"XDG_CACHE_HOME=" + workDir + "/.cache",
		"GOCACHE=" + workDir + "/.cache/go-build",
		"GOPATH=" + workDir + "/go",
		"GOMODCACHE=" + workDir + "/go/pkg/mod",
		"CARGO_HOME=" + workDir + "/.cargo",
		"npm_config_cache=" + workDir + "/.npm",
		"PIP_CACHE_DIR=" + workDir + "/.cache/pip",
	}
}

// Open launches a session container and returns its name. The caller wraps it in
// a Session and stores it.
func (e *Engine) Open(ctx context.Context, name string, o openOpts) error {
	out, code, err := e.run.Run(ctx, nil, e.cfg.MaxOutBytes, prepend(e.cfg.PodmanBin, e.runArgs(name, o))...)
	if err != nil {
		return fmt.Errorf("podman run: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("podman run exited %d: %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// execArgs builds `podman exec` for running a command inside a session's shell.
func (e *Engine) execArgs(name, command string) []string {
	// The user command runs inside the CONTAINER's login shell — that's the
	// sandbox boundary. Host-side there is no shell: podman + its args are
	// exec'd directly (no host shell interpolation), so `command` cannot inject
	// into the host.
	return []string{"exec", name, "bash", "-lc", command}
}

// Exec runs one command in the session, bounded by timeout + maxOut.
func (e *Engine) Exec(ctx context.Context, name, command string, timeout time.Duration, maxOut int64) (out []byte, code int, timedOut bool, err error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, code, err = e.run.Run(rctx, nil, maxOut, prepend(e.cfg.PodmanBin, e.execArgs(name, command))...)
	if rctx.Err() == context.DeadlineExceeded {
		return out, code, true, nil
	}
	return out, code, false, err
}

// Write copies content to <workDir>/<rel> inside the session. The path is passed
// via an env var (not interpolated into the shell string) and content via stdin,
// so neither can inject a host or in-container command.
func (e *Engine) Write(ctx context.Context, name, rel string, content []byte) error {
	abs := workDir + "/" + rel
	argv := prepend(e.cfg.PodmanBin, []string{
		"exec", "-i", "--env", "SBX_DEST=" + abs, name,
		"bash", "-lc", `mkdir -p "$(dirname "$SBX_DEST")" && cat > "$SBX_DEST"`,
	})
	out, code, err := e.run.Run(ctx, content, e.cfg.MaxOutBytes, argv...)
	if err != nil {
		return fmt.Errorf("podman exec write: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("write failed (exit %d): %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// Read returns up to maxBytes of <workDir>/<rel> from the session.
func (e *Engine) Read(ctx context.Context, name, rel string, maxBytes int64) ([]byte, error) {
	abs := workDir + "/" + rel
	argv := prepend(e.cfg.PodmanBin, []string{
		"exec", "--env", "SBX_SRC=" + abs, name,
		"bash", "-lc", `cat "$SBX_SRC"`,
	})
	out, code, err := e.run.Run(ctx, nil, maxBytes, argv...)
	if err != nil {
		return nil, fmt.Errorf("podman exec read: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("read failed (exit %d): %s", code, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Close force-removes a session container. Idempotent: removing an
// already-gone container is not an error.
func (e *Engine) Close(ctx context.Context, name string) error {
	out, code, err := e.run.Run(ctx, nil, e.cfg.MaxOutBytes, prepend(e.cfg.PodmanBin, []string{"rm", "-f", name})...)
	if err != nil {
		return fmt.Errorf("podman rm: %w", err)
	}
	if code != 0 && !strings.Contains(string(out), "no such container") {
		return fmt.Errorf("podman rm exited %d: %s", code, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReconcileBoot force-removes every container we previously managed. Run once at
// startup so a crash that skipped Close doesn't leak orphaned sandboxes.
func (e *Engine) ReconcileBoot(ctx context.Context) error {
	argv := prepend(e.cfg.PodmanBin, []string{"ps", "-aq", "--filter", "label=loomcycle.managed=1"})
	out, code, err := e.run.Run(ctx, nil, e.cfg.MaxOutBytes, argv...)
	if err != nil || code != 0 {
		return fmt.Errorf("podman ps: code=%d err=%v", code, err)
	}
	for _, id := range strings.Fields(string(out)) {
		_, _, _ = e.run.Run(ctx, nil, e.cfg.MaxOutBytes, prepend(e.cfg.PodmanBin, []string{"rm", "-f", id})...)
	}
	return nil
}

func prepend(bin string, args []string) []string {
	return append([]string{bin}, args...)
}

// boundedBuf is a bytes.Buffer that stops accepting after cap bytes and records
// that it truncated — mirrors the Bash tool's output bounding.
type boundedBuf struct {
	cap       int64
	n         int64
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuf) Write(p []byte) (int, error) {
	if b.n >= b.cap {
		b.truncated = true
		return len(p), nil // discard silently; caller appends a marker
	}
	remain := b.cap - b.n
	if int64(len(p)) > remain {
		b.buf.Write(p[:remain])
		b.n = b.cap
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	b.n += int64(len(p))
	return len(p), nil
}

func (b *boundedBuf) Bytes() []byte { return b.buf.Bytes() }
