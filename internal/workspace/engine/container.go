package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// WorkspaceMount is where every workspace is mounted in its own session
// (RFC GW §4.3). One fixed path keeps what tools write into their own files
// (a Python environment's absolute paths) correct after a fork, a restore
// or a template (spike F).
const WorkspaceMount = "/workspace"

// Defaults for a session's limits.
const (
	DefaultContainerMemoryMB = 2048
	DefaultContainerCPUs     = 2.0
	DefaultContainerPids     = 512
	DefaultContainerTmpMB    = 256
)

// Bounds on the CLI calls the engine makes itself, never on a command.
const (
	// probeTimeout covers a first pull of the image at start.
	probeTimeout = 10 * time.Minute
	// sessionTimeout bounds starting, inspecting or removing a session.
	sessionTimeout = 2 * time.Minute
	// killGrace is how long a command's client may take to return once its
	// process group was killed, before the session is removed instead.
	killGrace = 10 * time.Second
)

// ContainerOptions configure the container engine. They are the operator's
// (LOOMCYCLE_WORKSPACE_CONTAINER_*) and the worker's; nothing in them comes
// from a model or a run.
type ContainerOptions struct {
	// CLI is the container command, run without a shell: docker (the
	// default) or podman.
	CLI string
	// Image is the session image. Required.
	Image string
	// Runtime is the OCI runtime the daemon starts sessions with (runc,
	// runsc for gVisor, a kata runtime); empty is the daemon's default.
	Runtime string
	// MemoryMB, CPUs, Pids and TmpMB are a session's limits: its memory
	// (no swap beyond it), CPUs, processes, and the size of its /tmp.
	MemoryMB int64
	CPUs     float64
	Pids     int64
	TmpMB    int64
	// User is the uid:gid a session runs as: the worker's own, so files a
	// command writes and files the worker writes have one owner. Never
	// root.
	User string
	// HostRoot is the worker's root as the container daemon sees it, when
	// the two differ (a worker that runs in a container itself and drives
	// the host's daemon); empty is the root as the worker sees it.
	HostRoot string
	// WorkerKey names this worker's sessions among others on one daemon.
	WorkerKey string
	// MaxOutput bounds the combined output kept; zero is DefaultMaxOutput.
	MaxOutput int
	// Commands are the programs a command may start with, as for Host.
	Commands []string
	// Egress, when set, gives each session a way out through the worker
	// (RFC GW §6.3); nil keeps a session with no network at all.
	Egress *ContainerEgress
}

// ContainerEgress configures a session's egress: a network of its own with
// no route out, shared only with a gateway that answers as "egress" and
// forwards to the worker over a Unix socket of that workspace alone.
type ContainerEgress struct {
	// Dir is where the worker keeps one socket directory per workspace, as
	// the worker sees it. Short: a socket's path is limited to about 100
	// bytes.
	Dir string
	// HostDir is Dir as the container daemon sees it, when the two differ;
	// empty is Dir.
	HostDir string
	// Image is the gateway's image, which needs python3; empty is the
	// session image.
	Image string
	// Handler answers a workspace's requests; nil answers every one with
	// 503.
	Handler func(workspace string) http.Handler
}

// Container runs each workspace's commands in a container of its own, its
// session (RFC GW §4.3): read-only root, every capability dropped, no new
// privileges, a non-root user, process, memory and CPU limits, no network,
// and the workspace's directory mounted at /workspace and nothing else of
// the worker's disk. Its environment is the engine's own list, so nothing of
// the worker's environment (its token) reaches a command.
//
// A session is started at a workspace's first command and started again when
// it is gone. A command that outlives its timeout, or whose op the core gave
// up on, is killed inside the session with everything in its process group;
// when that cannot be done the session is removed. What a session holds
// outside /workspace (/tmp, a running server) ends with it.
type Container struct {
	opt ContainerOptions
	mu  sync.Mutex
	// locks serialises starting and removing one workspace's session.
	locks map[string]*sync.Mutex
}

// NewContainer checks opt and fills its defaults. It runs nothing: Probe
// checks that the daemon, the runtime and the image work.
func NewContainer(opt ContainerOptions) (*Container, error) {
	if opt.CLI == "" {
		opt.CLI = "docker"
	}
	if opt.Image == "" || strings.HasPrefix(opt.Image, "-") || strings.ContainsAny(opt.Image, " \t\n") {
		return nil, fmt.Errorf("container engine: the session image %q is not an image name", opt.Image)
	}
	if opt.Runtime != "" && !plainName(opt.Runtime) {
		return nil, fmt.Errorf("container engine: the runtime %q is not a runtime name", opt.Runtime)
	}
	if !plainName(opt.WorkerKey) {
		return nil, fmt.Errorf("container engine: the worker key %q is not a plain name", opt.WorkerKey)
	}
	uid, gid, ok := strings.Cut(opt.User, ":")
	u, uerr := strconv.Atoi(uid)
	_, gerr := strconv.Atoi(gid)
	if !ok || uerr != nil || gerr != nil || u < 0 {
		return nil, fmt.Errorf("container engine: the user %q is not uid:gid", opt.User)
	}
	if u == 0 {
		return nil, errors.New("container engine: sessions would run as root; run the worker as a user that is not root")
	}
	if strings.ContainsAny(opt.HostRoot, ":,") {
		return nil, fmt.Errorf("container engine: the host root %q holds a ':' or ','", opt.HostRoot)
	}
	if opt.MemoryMB <= 0 {
		opt.MemoryMB = DefaultContainerMemoryMB
	}
	if opt.CPUs <= 0 {
		opt.CPUs = DefaultContainerCPUs
	}
	if opt.Pids <= 0 {
		opt.Pids = DefaultContainerPids
	}
	if opt.TmpMB <= 0 {
		opt.TmpMB = DefaultContainerTmpMB
	}
	if opt.MaxOutput <= 0 {
		opt.MaxOutput = DefaultMaxOutput
	}
	return &Container{opt: opt, locks: map[string]*sync.Mutex{}}, nil
}

// plainName is a name the CLI can only read as a value: letters, digits and
// . _ -, not starting with -.
func plainName(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// WorkerKey derives a worker's session key from its id and root, so two
// workers on one daemon never touch each other's sessions.
func WorkerKey(workerID, root string) string {
	sum := sha256.Sum256([]byte(workerID + "\x00" + root))
	return hex.EncodeToString(sum[:6])
}

// labelWorker marks a session with the worker it belongs to.
const labelWorker = "loomcycle.workspace.worker"

func (c *Container) session(workspace string) string {
	return "loomws-" + c.opt.WorkerKey + "-" + workspace
}

// hardening is the part of a session's run arguments that confines it. The
// probe runs under it too, so what the worker reports is what a session has.
func (c *Container) hardening() []string {
	a := []string{"--init", "--network", "none", "--read-only",
		"--tmpfs", fmt.Sprintf("/tmp:rw,size=%dm,mode=1777,nosuid,nodev,exec", c.opt.TmpMB),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", c.opt.User,
		"--pids-limit", strconv.FormatInt(c.opt.Pids, 10),
		"--memory", fmt.Sprintf("%dm", c.opt.MemoryMB),
		"--memory-swap", fmt.Sprintf("%dm", c.opt.MemoryMB),
		"--cpus", strconv.FormatFloat(c.opt.CPUs, 'f', -1, 64),
	}
	if c.opt.Runtime != "" {
		a = append(a, "--runtime", c.opt.Runtime)
	}
	// The whole environment of a session: written here, never copied from
	// the worker's. Caches go into the workspace, so installs survive a
	// recreated session and count against its quota.
	for _, kv := range []string{
		"HOME=" + WorkspaceMount,
		"TMPDIR=/tmp",
		"XDG_CACHE_HOME=" + WorkspaceMount + "/.cache",
		"PIP_CACHE_DIR=" + WorkspaceMount + "/.cache/pip",
		"npm_config_cache=" + WorkspaceMount + "/.cache/npm",
	} {
		a = append(a, "--env", kv)
	}
	return a
}

// runArgs are the arguments that start workspace's session over hostDir.
func (c *Container) runArgs(workspace, hostDir string) []string {
	a := []string{"run", "-d", "--name", c.session(workspace),
		"--label", labelWorker + "=" + c.opt.WorkerKey,
		"--label", "loomcycle.workspace=" + workspace,
	}
	a = append(a, c.hardening()...)
	a = append(a, "-v", hostDir+":"+WorkspaceMount+":rw", "--workdir", WorkspaceMount, c.opt.Image, "sleep", "infinity")
	return a
}

// execWrapper runs the command ($1) in a shell of its own and records that
// shell's process group (a command started by exec leads its own group) in
// /tmp under the call's token ($2), so a kill can name the group. The record
// goes when the command ends; the exit status is the command's.
const execWrapper = `f=/tmp/.loom-exec-$2; echo $$ > "$f" || exit 125; /bin/sh -c "$1"; s=$?; rm -f "$f"; exit $s`

// killScript kills the process group recorded under the token ($1).
const killScript = `p=$(cat /tmp/.loom-exec-$1) && kill -9 -$p`

// Probe checks that the daemon runs a session with these options (the
// runtime is known, the image is there or can be pulled) and reports the
// toolchains the image has (workspace.ToolchainPython, ToolchainNode).
func (c *Container) Probe(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	args := append([]string{"run", "--rm"}, c.hardening()...)
	args = append(args, c.opt.Image, "/bin/sh", "-c",
		`command -v python3 >/dev/null 2>&1 && echo python; command -v node >/dev/null 2>&1 && echo node; true`)
	out, err := c.cli(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("container engine: starting a session of %s: %w", c.opt.Image, err)
	}
	// Only the two names the probe prints are taken: the image's own output
	// (a login banner, an entrypoint's chatter) never becomes a label.
	tc := []string{}
	for _, f := range strings.Fields(string(out)) {
		if (f == "python" || f == "node") && !slices.Contains(tc, f) {
			tc = append(tc, f)
		}
	}
	return tc, nil
}

// Reconcile removes every session of this worker: at start, those a crash
// left; at stop, those still running. A session holds nothing that must
// survive: the workspace's files are on the worker's disk.
func (c *Container) Reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()
	out, err := c.cli(ctx, "ps", "-aq", "--filter", "label="+labelWorker+"="+c.opt.WorkerKey)
	if err != nil {
		return fmt.Errorf("container engine: listing this worker's sessions: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	if _, err := c.cli(ctx, append([]string{"rm", "-f"}, ids...)...); err != nil {
		return fmt.Errorf("container engine: removing this worker's sessions: %w", err)
	}
	return nil
}

func (c *Container) lock(workspace string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.locks[workspace]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[workspace] = l
	}
	return l
}

// ensure starts workspace's session unless it is running; one that stopped
// is removed and started again over the same directory.
func (c *Container) ensure(ctx context.Context, workspace, root string) error {
	l := c.lock(workspace)
	l.Lock()
	defer l.Unlock()
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()
	name := c.session(workspace)
	out, err := c.cli(ctx, "inspect", "--format", "{{.State.Running}}", name)
	if err == nil && strings.TrimSpace(string(out)) == "true" {
		return nil
	}
	if err == nil {
		if _, err := c.cli(ctx, "rm", "-f", name); err != nil {
			return fmt.Errorf("removing the stopped session: %w", err)
		}
	}
	hostDir := root
	if c.opt.HostRoot != "" {
		hostDir = filepath.Join(c.opt.HostRoot, workspace)
	}
	if strings.ContainsAny(hostDir, ":,") {
		return fmt.Errorf("the workspace's path %q holds a ':' or ','", hostDir)
	}
	if _, err := c.cli(ctx, c.runArgs(workspace, hostDir)...); err != nil {
		return fmt.Errorf("starting the session: %w", err)
	}
	return nil
}

// Forget removes workspace's session and everything running in it.
func (c *Container) Forget(ctx context.Context, workspace string) error {
	l := c.lock(workspace)
	l.Lock()
	defer l.Unlock()
	err := c.remove(ctx, c.session(workspace))
	c.mu.Lock()
	delete(c.locks, workspace)
	c.mu.Unlock()
	return err
}

// remove force-removes a session; one already gone is not an error.
func (c *Container) remove(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionTimeout)
	defer cancel()
	if _, err := c.cli(ctx, "rm", "-f", name); err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return fmt.Errorf("container engine: removing the session: %w", err)
	}
	return nil
}

// Close removes this worker's sessions: a stopped worker leaves nothing
// running.
func (c *Container) Close() error {
	return c.Reconcile(context.Background())
}

// Exec runs req.Command in its workspace's session, in /workspace or the
// folder of it req.Dir names.
func (c *Container) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if strings.TrimSpace(req.Command) == "" {
		return ExecResult{}, fmt.Errorf("%w: an empty command", ErrRefused)
	}
	if err := allowed(c.opt.Commands, req.Command); err != nil {
		return ExecResult{}, err
	}
	if req.Timeout <= 0 {
		return ExecResult{}, errors.New("exec: no timeout")
	}
	rel, err := filepath.Rel(req.Root, req.Dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ExecResult{}, fmt.Errorf("exec: the directory is not in the workspace")
	}
	cwd := path.Join(WorkspaceMount, filepath.ToSlash(rel))
	if err := c.ensure(ctx, req.Workspace, req.Root); err != nil {
		return ExecResult{}, fmt.Errorf("exec: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return ExecResult{}, err
	}
	name := c.session(req.Workspace)
	// Not CommandContext: killing the CLI client would leave the command
	// running in the session. The kill below goes through the engine.
	// #nosec G204 -- the CLI is the operator's setting, run without a shell; the agent's command is an argument of a shell inside the session, which is what this engine is for
	cmd := exec.Command(c.opt.CLI, "exec", "-w", cwd, name, "/bin/sh", "-c", execWrapper, "sh", req.Command, token)
	out := &capped{max: c.opt.MaxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return ExecResult{}, fmt.Errorf("exec: starting the command: %w", err)
	}
	waited := make(chan error, 1)
	safego.Go(ctx, "workspace.engine.container.wait", func() {
		// Sent from a defer, so a recovered panic still answers: the kill
		// path below waits on it.
		err := errors.New("exec: waiting for the command failed")
		defer func() { waited <- err }()
		err = cmd.Wait()
	})

	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	var waitErr error
	killed := false
	select {
	case waitErr = <-waited:
	case <-runCtx.Done():
		killed = true
		waitErr = c.kill(name, token, cmd, waited)
	}
	res := ExecResult{ExitCode: -1}
	res.Output, res.Truncated = out.result()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if killed {
		res.ExitCode = -1
		res.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	if res.Truncated {
		res.Output = append(res.Output, fmt.Sprintf("\n[output truncated at %d bytes]", c.opt.MaxOutput)...)
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && !killed {
		return res, fmt.Errorf("exec: %w", waitErr)
	}
	return res, nil
}

// kill ends a command whose time is up: its process group in the session,
// then the client. When the group cannot be killed (the session is gone, or
// the command removed its record) or the client does not return, the session
// is removed, which ends everything in it. It returns the client's Wait.
func (c *Container) kill(name, token string, cmd *exec.Cmd, waited <-chan error) error {
	ctx, cancel := context.WithTimeout(context.Background(), killGrace)
	defer cancel()
	if _, err := c.cli(ctx, "exec", name, "/bin/sh", "-c", killScript, "sh", token); err == nil {
		select {
		case err := <-waited:
			return err
		case <-time.After(killGrace):
		}
	}
	_ = c.remove(context.Background(), name)
	select {
	case err := <-waited:
		return err
	case <-time.After(killGrace):
		_ = cmd.Process.Kill()
		return <-waited
	}
}

// cli runs the container CLI and returns its standard output; a failure
// carries its standard error.
func (c *Container) cli(ctx context.Context, args ...string) ([]byte, error) {
	// #nosec G204 -- the CLI is the operator's setting and every argument is built here from checked names, never a shell
	cmd := exec.CommandContext(ctx, c.opt.CLI, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{buf: &stdout, max: 1 << 20}, &limited{buf: &stderr, max: 64 << 10}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("%s %s: %w", c.opt.CLI, args[0], err)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", c.opt.CLI, args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

// limited keeps the first max bytes written to it.
type limited struct {
	buf *bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func randomToken() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
