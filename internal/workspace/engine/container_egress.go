package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// A session's egress (RFC GW §4.3, §6.3; P4 plan O12).
//
// A session with egress is on a Docker network of its own: --internal, so it
// has no route out, and with no address for the host on its bridge
// (inhibit_ipv4), so it cannot reach the worker, the core or anything else on
// this host through the bridge either. The only other member of that network
// is the workspace's gateway, under the alias "egress": a small forwarder that
// passes each connection to a Unix socket the worker listens on for that
// workspace alone. So the worker knows which workspace a request came from by
// the socket it arrived on, never by anything in the request, and two
// sessions share no network at all. A name other than egress resolves
// nowhere: the network's resolver forwards to nothing (--dns 127.0.0.1).
//
// The gateway runs the forwarder in Python (the session image has python3)
// under the session's own confinement: read-only, no capability, a non-root
// user, small limits, on the internal network only. It holds no secret: the
// worker's socket is all it can reach, and the worker's handler forwards to
// the core, which decides.

// EgressHost is the name a session reaches its routes at.
const EgressHost = "egress"

// egressSocket is the socket's name in its workspace's directory, and
// gatewayMount where the gateway sees that directory.
const (
	egressSocket   = "egress.sock"
	gatewayMount   = "/run/loomgw"
	gatewayScript  = "/opt/loomgw/gateway.py"
	gatewayFile    = "gateway.py"
	maxSocketPath  = 100
	labelWorkspace = "loomcycle.workspace"
	labelRole      = "loomcycle.workspace.role"
)

// egressSessionArgs are a session's arguments when it has egress: a resolver
// that answers nothing but the network's own names, and the package managers
// pointed at the routes of the `workspace` preset. A workspace that does not
// hold one of them is refused by the core with the route's name.
func egressSessionArgs() []string {
	a := []string{"--dns", "127.0.0.1"}
	for _, kv := range egressEnv("http://"+EgressHost+"/", EgressHost) {
		a = append(a, "--env", kv)
	}
	return a
}

// gatewayPy forwards each TCP connection on port 80 to the workspace's
// socket, byte for byte. It reads nothing of what passes. At most 256
// connections at once.
const gatewayPy = `import socket, threading

SOCK = "` + gatewayMount + "/" + egressSocket + `"
slots = threading.BoundedSemaphore(256)


def pump(src, dst):
    try:
        while True:
            b = src.recv(65536)
            if not b:
                break
            dst.sendall(b)
    except OSError:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass


def serve(c):
    u = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        try:
            u.connect(SOCK)
        except OSError:
            c.sendall(b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            return
        t = threading.Thread(target=pump, args=(u, c), daemon=True)
        t.start()
        pump(c, u)
        t.join()
    except OSError:
        pass
    finally:
        u.close()
        c.close()
        slots.release()


s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 80))
s.listen(128)
while True:
    slots.acquire()
    try:
        c, _ = s.accept()
    except OSError:
        slots.release()
        continue
    threading.Thread(target=serve, args=(c,), daemon=True).start()
`

// egress is the engine's side of its sessions' egress.
type egress struct {
	opt       ContainerEgress
	image     string
	dir       string // this worker's directory, as the worker sees it
	hostDir   string // the same, as the daemon sees it
	mu        sync.Mutex
	listeners map[string]*egressListener
}

type egressListener struct {
	srv *http.Server
	l   net.Listener
}

func newEgress(opt ContainerEgress, sessionImage, workerKey string) (*egress, error) {
	if !filepath.IsAbs(opt.Dir) {
		return nil, fmt.Errorf("container engine: the egress directory %q is not an absolute path", opt.Dir)
	}
	hostDir := opt.HostDir
	if hostDir == "" {
		hostDir = opt.Dir
	}
	if !filepath.IsAbs(hostDir) || strings.ContainsAny(opt.Dir+hostDir, ":,") {
		return nil, fmt.Errorf("container engine: the egress directory %q (on the daemon %q) is not an absolute path free of ':' and ','", opt.Dir, hostDir)
	}
	image := opt.Image
	if image == "" {
		image = sessionImage
	}
	if strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\n") {
		return nil, fmt.Errorf("container engine: the gateway image %q is not an image name", image)
	}
	e := &egress{
		opt: opt, image: image,
		dir: filepath.Join(opt.Dir, workerKey), hostDir: filepath.Join(hostDir, workerKey),
		listeners: map[string]*egressListener{},
	}
	if len(e.socketPath("x")) > maxSocketPath {
		return nil, fmt.Errorf("container engine: the egress directory %q is too long for a Unix socket's path (at most %d bytes in all)", opt.Dir, maxSocketPath)
	}
	return e, nil
}

// wsDir names a workspace's socket directory by a hash: a workspace id may be
// long, and a socket's path may not.
func wsDir(workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	return hex.EncodeToString(sum[:8])
}

func (e *egress) socketPath(workspace string) string {
	return filepath.Join(e.dir, wsDir(workspace), egressSocket)
}

// sessionNetwork is the network a workspace's session joins.
func (c *Container) sessionNetwork(workspace string) string {
	if c.egress == nil {
		return "none"
	}
	return c.session(workspace) + "-net"
}

func (c *Container) gateway(workspace string) string { return c.session(workspace) + "-gw" }

// egressUp makes sure a workspace's way out is there before its session
// starts: the worker's socket, the network, and the gateway on it. The
// caller holds the workspace's lock.
func (c *Container) egressUp(ctx context.Context, workspace string) error {
	if c.egress == nil {
		return nil
	}
	if err := c.egress.listen(ctx, workspace); err != nil {
		return fmt.Errorf("egress: %w", err)
	}
	netName := c.sessionNetwork(workspace)
	if _, err := c.cli(ctx, "network", "inspect", "--format", "{{.Internal}}", netName); err != nil {
		if _, err := c.cli(ctx, "network", "create", "--internal",
			"--opt", "com.docker.network.bridge.inhibit_ipv4=true",
			"--label", labelWorker+"="+c.opt.WorkerKey, "--label", labelWorkspace+"="+workspace,
			netName); err != nil {
			return fmt.Errorf("egress: creating the session's network: %w", err)
		}
	}
	gw := c.gateway(workspace)
	out, err := c.cli(ctx, "inspect", "--format", "{{.State.Running}}", gw)
	if err == nil && strings.TrimSpace(string(out)) == "true" {
		return nil
	}
	if err == nil {
		if _, err := c.cli(ctx, "rm", "-f", gw); err != nil {
			return fmt.Errorf("egress: removing the stopped gateway: %w", err)
		}
	}
	if _, err := c.cli(ctx, c.gatewayArgs(workspace)...); err != nil {
		return fmt.Errorf("egress: starting the gateway: %w", err)
	}
	return nil
}

// gatewayArgs start a workspace's gateway: confined as a session is, smaller,
// on the session's network only, seeing nothing of the worker's disk but its
// own workspace's socket directory and the forwarder, both read-only.
func (c *Container) gatewayArgs(workspace string) []string {
	e := c.egress
	a := []string{"run", "-d", "--name", c.gateway(workspace),
		"--label", labelWorker + "=" + c.opt.WorkerKey,
		"--label", labelWorkspace + "=" + workspace,
		"--label", labelRole + "=gateway",
		"--init", "--network", c.sessionNetwork(workspace), "--network-alias", EgressHost,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", c.opt.User,
		// Port 80 without a capability, inside its own network namespace.
		"--sysctl", "net.ipv4.ip_unprivileged_port_start=0",
		"--pids-limit", "300", "--memory", "96m", "--memory-swap", "96m", "--cpus", "0.5",
	}
	if c.opt.Runtime != "" {
		a = append(a, "--runtime", c.opt.Runtime)
	}
	a = append(a,
		"-v", filepath.Join(e.hostDir, wsDir(workspace))+":"+gatewayMount+":ro",
		"-v", filepath.Join(e.hostDir, gatewayFile)+":"+gatewayScript+":ro",
		e.image, "python3", "-I", gatewayScript)
	return a
}

// listen starts the worker's socket for a workspace, once.
func (e *egress) listen(_ context.Context, workspace string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.listeners[workspace] != nil {
		return nil
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return err
	}
	// Written each time: Reconcile empties the directory.
	if err := os.WriteFile(filepath.Join(e.dir, gatewayFile), []byte(gatewayPy), 0o400); err != nil && !errors.Is(err, os.ErrPermission) {
		return err
	}
	dir := filepath.Join(e.dir, wsDir(workspace))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, egressSocket)
	_ = os.Remove(path) // a socket a crashed worker left
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return err
	}
	var h http.Handler = http.HandlerFunc(notLinked)
	if e.opt.Handler != nil {
		h = e.opt.Handler(workspace)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	e.listeners[workspace] = &egressListener{srv: srv, l: l}
	safego.Go(context.Background(), "workspace.engine.egress.serve", func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("workspace egress socket stopped", "workspace", workspace, "err", err)
		}
	})
	return nil
}

// notLinked answers when the worker has no way to the core for egress.
func notLinked(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, "egress is not linked to the core on this worker\n")
}

// stop closes a workspace's socket and removes its directory.
func (e *egress) stop(workspace string) error {
	e.mu.Lock()
	ls := e.listeners[workspace]
	delete(e.listeners, workspace)
	e.mu.Unlock()
	if ls != nil {
		_ = ls.srv.Close()
	}
	return os.RemoveAll(filepath.Join(e.dir, wsDir(workspace)))
}

// egressDown removes a workspace's gateway, network and socket. The caller
// holds the workspace's lock and has removed the session (a network with a
// member cannot be removed).
func (c *Container) egressDown(ctx context.Context, workspace string) error {
	if c.egress == nil {
		return nil
	}
	err := c.remove(ctx, c.gateway(workspace))
	if nerr := c.removeNetwork(ctx, c.sessionNetwork(workspace)); err == nil {
		err = nerr
	}
	if serr := c.egress.stop(workspace); err == nil && serr != nil {
		err = fmt.Errorf("container engine: removing the egress socket: %w", serr)
	}
	return err
}

func (c *Container) removeNetwork(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionTimeout)
	defer cancel()
	if _, err := c.cli(ctx, "network", "rm", name); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not found") || strings.Contains(msg, "no such network") {
			return nil
		}
		return fmt.Errorf("container engine: removing the session's network: %w", err)
	}
	return nil
}

// egressReconcile removes this worker's networks and sockets, after its
// containers are gone: at start what a crash left, at stop what is still
// there.
func (c *Container) egressReconcile(ctx context.Context) error {
	if c.egress == nil {
		return nil
	}
	out, err := c.cli(ctx, "network", "ls", "-q", "--filter", "label="+labelWorker+"="+c.opt.WorkerKey)
	if err != nil {
		return fmt.Errorf("container engine: listing this worker's networks: %w", err)
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		if _, err := c.cli(ctx, append([]string{"network", "rm"}, ids...)...); err != nil {
			return fmt.Errorf("container engine: removing this worker's networks: %w", err)
		}
	}
	c.egress.mu.Lock()
	ws := make([]string, 0, len(c.egress.listeners))
	for w := range c.egress.listeners {
		ws = append(ws, w)
	}
	c.egress.mu.Unlock()
	for _, w := range ws {
		_ = c.egress.stop(w)
	}
	if err := os.RemoveAll(c.egress.dir); err != nil {
		return fmt.Errorf("container engine: removing this worker's egress sockets: %w", err)
	}
	return nil
}
