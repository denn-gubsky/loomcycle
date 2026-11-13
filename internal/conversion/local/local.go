// Package local runs conversion workers as children of the core (RFC GO §5.7,
// conversion.mode: local): copies of the same binary, started with
// `worker`, attached over a Unix socket in a private directory, each with a
// soft memory limit and, on Linux, the highest OOM score, so that in a shared
// cgroup the kernel kills a worker before the core. A child that exits is
// started again with backoff.
//
// A child gets a minimal environment: no provider key, no store connection,
// no setting of the core's. It needs none to convert bytes.
package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

const (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
	// stableAfter: a child that ran this long resets the backoff.
	stableAfter = time.Minute
	// stopWait is how long a child has to exit after it is asked to.
	stopWait = 5 * time.Second
	// maxSocketPath is under the 104-byte sun_path of macOS and the BSDs
	// (Linux allows 108).
	maxSocketPath = 100
)

// passEnv are the host variables a child keeps: what a process needs to find
// programs and a temp directory, and nothing that configures the core.
var passEnv = []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "LANG", "LC_ALL", "TZ"}

// Options configure the children.
type Options struct {
	// Binary is the loomcycle executable to start (os.Executable()).
	Binary string
	// Socket is the dispatcher's Unix socket.
	Socket string
	// Token is the per-boot worker token the children present.
	Token string
	// Workers is how many children to keep running.
	Workers int
	// MemoryLimitMB is each child's GOMEMLIMIT; 0 sets none.
	MemoryLimitMB int
	// OnRespawn is called each time a child is started again.
	OnRespawn func()
	Logger    *slog.Logger
}

// Listen makes a private directory (mode 0700) and listens on a Unix socket
// in it. cleanup closes the listener and removes the directory.
func Listen() (l net.Listener, socket string, cleanup func(), err error) {
	base := os.TempDir()
	// macOS's per-user temp directory is long; fall back to /tmp rather than
	// fail on the socket path's length.
	if len(filepath.Join(base, "lc-w-0123456789", "d.sock")) > maxSocketPath && runtime.GOOS != "windows" {
		base = "/tmp"
	}
	// MkdirTemp makes the directory 0700 (the umask can only take bits
	// away): only this user reaches the socket inside it.
	dir, err := os.MkdirTemp(base, "lc-w-")
	if err != nil {
		return nil, "", nil, fmt.Errorf("conversion: worker socket directory: %w", err)
	}
	socket = filepath.Join(dir, "d.sock")
	l, err = net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		return nil, "", nil, fmt.Errorf("conversion: worker socket: %w", err)
	}
	return l, socket, func() { l.Close(); os.RemoveAll(dir) }, nil
}

// NewToken is a fresh per-boot worker token.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Supervisor keeps the children running until Stop.
type Supervisor struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	pids map[int]int // child slot → running pid
}

// Start starts opt.Workers children and supervises them.
func Start(opt Options) *Supervisor {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{cancel: cancel, pids: map[int]int{}}
	for i := range max(opt.Workers, 1) {
		s.wg.Add(1)
		safego.Go(ctx, "conversion.supervise", func() {
			defer s.wg.Done()
			s.supervise(ctx, opt, i, opt.Logger.With("component", "conversion-supervisor", "child", i))
		})
	}
	return s
}

// Stop asks every child to exit, kills one that does not within a few
// seconds, and waits.
func (s *Supervisor) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *Supervisor) supervise(ctx context.Context, opt Options, n int, log *slog.Logger) {
	backoff := backoffMin
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if opt.OnRespawn != nil {
				opt.OnRespawn()
			}
		}
		started := time.Now()
		err := s.runChild(ctx, opt, n)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= stableAfter {
			backoff = backoffMin
		}
		log.Warn("conversion worker exited; starting another", "err", err, "in", backoff)
		if backoff == backoffMax {
			log.Error("conversion worker is crash-looping", "err", err)
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// runChild starts one child and waits for it to exit, or stops it when ctx
// ends.
func (s *Supervisor) runChild(ctx context.Context, opt Options, n int) error {
	// #nosec G204 -- Binary is this process's own executable (os.Executable at boot), and every argument is made here: the socket path of a directory this process created, its own pid and a fixed id; nothing comes from a run, a file or a caller
	cmd := exec.Command(opt.Binary, "worker",
		"--dispatcher", "unix://"+opt.Socket,
		"--parent-pid", strconv.Itoa(os.Getpid()),
		"--oom-score-adj", "1000",
		"--id", fmt.Sprintf("local-%d-%d", os.Getpid(), n))
	cmd.Env = childEnv(opt)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.pids[n] = cmd.Process.Pid
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pids, n)
		s.mu.Unlock()
	}()
	done := make(chan error, 1)
	safego.Go(ctx, "conversion.child.wait", func() { done <- cmd.Wait() })
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case err := <-done:
		return err
	case <-time.After(stopWait):
		_ = cmd.Process.Kill()
		return <-done
	}
}

func childEnv(opt Options) []string {
	var env []string
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "LOOMCYCLE_WORKER_TOKEN="+opt.Token, "LOOMCYCLE_WORKER_SLOTS=1")
	if opt.MemoryLimitMB > 0 {
		env = append(env, fmt.Sprintf("GOMEMLIMIT=%dMiB", opt.MemoryLimitMB))
	}
	return env
}
