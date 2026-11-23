package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/fsops"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// startWatch is how long start waits to see whether a server ended at once
// (a typo, a port in use), so the agent reads why in start's answer rather
// than in a later logs.
const startWatch = 500 * time.Millisecond

// stopWait bounds how long stop waits for a killed server to be reaped.
const stopWait = 10 * time.Second

// servers are the workspaces' servers (RFC GW §5.3): commands that outlive
// the op that started them. A server ends with stop, with its workspace's
// removal (Drop) and with the worker (Close).
type servers struct {
	mu sync.Mutex
	// by is each workspace's servers by id.
	by map[string]map[string]*server
	// next numbers each workspace's servers; it never goes back, so an id
	// names one server for the workspace's life.
	next map[string]int
}

type server struct {
	info workspace.ServerInfo
	log  *ring
	// mu guards proc and stopped: a stop (or the workspace's removal) can
	// come while start is still starting the process.
	mu      sync.Mutex
	proc    engine.Process
	stopped bool
	// ended is closed once the output is read to its end and the process
	// reaped; endedAt is then set.
	ended   chan struct{}
	endedAt time.Time
}

func (s *servers) init() {
	if s.by == nil {
		s.by = map[string]map[string]*server{}
		s.next = map[string]int{}
	}
}

// add registers a server for id unless the workspace runs MaxServers.
func (s *servers) add(id string, srv *server) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	running := 0
	for _, o := range s.by[id] {
		if !o.isEnded() {
			running++
		}
	}
	if running >= workspace.MaxServers {
		return errServerLimit
	}
	if s.by[id] == nil {
		s.by[id] = map[string]*server{}
	}
	// Ended servers are kept for logs, but not past the bound twice over.
	if len(s.by[id]) >= 2*workspace.MaxServers {
		oldest := ""
		for sid, o := range s.by[id] {
			if o.isEnded() && (oldest == "" || o.info.StartedAt.Before(s.by[id][oldest].info.StartedAt)) {
				oldest = sid
			}
		}
		delete(s.by[id], oldest)
	}
	s.next[id]++
	srv.info.Server = "srv-" + strconv.Itoa(s.next[id])
	s.by[id][srv.info.Server] = srv
	return nil
}

var errServerLimit = errors.New("server limit")

func (s *servers) get(id, sid string) *server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.by[id][sid]
}

func (s *servers) list(id string) []*server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*server, 0, len(s.by[id]))
	for _, o := range s.by[id] {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].info.StartedAt.Before(out[j].info.StartedAt) })
	return out
}

// Drop kills every server of workspace id and forgets them: its workspace
// is being removed.
func (s *servers) Drop(id string) {
	s.mu.Lock()
	gone := s.by[id]
	delete(s.by, id)
	delete(s.next, id)
	s.mu.Unlock()
	for _, o := range gone {
		_ = o
	}
}

// Close kills every server: the worker is stopping.
func (s *servers) Close() {
	s.mu.Lock()
	all := s.by
	s.by, s.next = nil, nil
	s.mu.Unlock()
	for _, m := range all {
		for _, o := range m {
			o.stop()
		}
	}
}

func (o *server) isEnded() bool {
	select {
	case <-o.ended:
		return true
	default:
		return false
	}
}

// stop kills the server's process group and waits, bounded, for it to be
// reaped. It reports whether the server was still running.
func (o *server) stop() bool {
	o.mu.Lock()
	o.stopped = true
	p := o.proc
	o.mu.Unlock()
	if p == nil || o.isEnded() {
		// Not started yet: start sees stopped and kills what it starts.
		return false
	}
	_ = p.Kill()
	select {
	case <-o.ended:
	case <-time.After(stopWait):
	}
	return true
}

// snapshot is the server's info; with tail > 0, its last tail bytes of
// output too.
func (o *server) snapshot(tail int) workspace.ServerInfo {
	info := o.info
	info.Running = !o.isEnded()
	if !info.Running {
		info.EndedAt = o.endedAt
	}
	if tail > 0 {
		b, dropped := o.log.tail(tail)
		text := strings.ToValidUTF8(string(b), string(utf8.RuneError))
		info.Output = &text
		info.Truncated = dropped
	}
	return info
}

// ring keeps the last max bytes written to it.
type ring struct {
	mu      sync.Mutex
	max     int
	buf     []byte
	dropped bool
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = append(r.buf[:0:0], r.buf[over:]...)
		r.dropped = true
	}
	return len(p), nil
}

// tail is the last n bytes kept, and whether anything before them was
// written.
func (r *ring) tail(n int) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.buf) {
		return append([]byte(nil), r.buf...), r.dropped
	}
	return append([]byte(nil), r.buf[len(r.buf)-n:]...), true
}

// serverOp runs a server op in workspace id, whose real directory is real.
func (w *worker) serverOp(ctx context.Context, id, real, op string, raw []byte) answer {
	switch op {
	case workspace.OpServerStart:
		var in workspace.ServerStartArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		// A server is a command (RFC GW P2 ruling Q2): refused while the
		// workspace is marked full.
		return w.commandInQuota(ctx, id, real, func() answer { return w.startServer(ctx, id, real, in) })
	case workspace.OpServerStop:
		var in workspace.ServerStopArgs
		if a := decodeArgs(raw, &in); a.class != "" {
			return a
		}
		o := w.servers.get(id, in.Server)
		if o == nil {
			return refuse(workspace.ClassServerNotFound, "this workspace has no server %q; logs lists them", in.Server)
		}
		out, _ := json.Marshal(workspace.ServerStopAnswer{Server: in.Server, Stopped: o.stop()})
		return answer{output: out}
	case workspace.OpServerLogs:
		in := workspace.ServerLogsArgs{}
		if len(raw) > 0 {
			if a := decodeArgs(raw, &in); a.class != "" {
				return a
			}
		}
		if in.TailBytes < 0 || in.TailBytes > workspace.ServerLogBytes {
			return refuse(workspace.ClassInvalid, "tail_bytes must be from 1 to %d", workspace.ServerLogBytes)
		}
		var ans workspace.ServerLogsAnswer
		if in.Server == "" {
			for _, o := range w.servers.list(id) {
				ans.Servers = append(ans.Servers, o.snapshot(0))
			}
		} else {
			o := w.servers.get(id, in.Server)
			if o == nil {
				return refuse(workspace.ClassServerNotFound, "this workspace has no server %q; logs lists them", in.Server)
			}
			tail := in.TailBytes
			if tail == 0 {
				tail = defaultLogTail
			}
			ans.Servers = []workspace.ServerInfo{o.snapshot(tail)}
		}
		if ans.Servers == nil {
			ans.Servers = []workspace.ServerInfo{}
		}
		out, _ := json.Marshal(ans)
		return answer{output: out}
	}
	return refuse(workspace.ClassUnsupportedOp, "this worker does not run %q", op)
}

// defaultLogTail is the output logs returns for a server when the call
// names no tail_bytes.
const defaultLogTail = 64 << 10

func (w *worker) startServer(ctx context.Context, id, real string, in workspace.ServerStartArgs) answer {
	if strings.TrimSpace(in.Command) == "" {
		return refuse(workspace.ClassInvalid, "start needs command")
	}
	if len(in.Command) > workspace.MaxServerCommand {
		return refuse(workspace.ClassInvalid, "the command is over %d bytes", workspace.MaxServerCommand)
	}
	if in.Port < 0 || in.Port > 65535 {
		return refuse(workspace.ClassInvalid, "port must be from 1 to 65535")
	}
	dir := real
	if in.Cwd != "" {
		d, err := fsops.ResolveInsideRoot(real, in.Cwd)
		if err != nil {
			return hidePaths(refuse(workspace.ClassInvalid, "cwd: %v", err), false, real)
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return refuse(workspace.ClassInvalid, "cwd: %q is not a directory in the workspace", in.Cwd)
		}
		dir = d
	}
	srv := &server{
		info:  workspace.ServerInfo{Command: in.Command, Port: in.Port, StartedAt: time.Now().UTC()},
		log:   &ring{max: workspace.ServerLogBytes},
		ended: make(chan struct{}),
	}
	// Registered before it starts, so two starts racing cannot both pass
	// the bound; a start that fails takes its entry back.
	if err := w.servers.add(id, srv); err != nil {
		return refuse(workspace.ClassServerLimit, "this workspace runs %d servers, the most it may; stop one first", workspace.MaxServers)
	}
	p, err := w.engine.Start(ctx, engine.StartRequest{Workspace: id, Root: real, Dir: dir, Command: in.Command})
	if err != nil {
		w.servers.remove(id, srv.info.Server)
		if errors.Is(err, engine.ErrRefused) {
			return refuse(workspace.ClassRefused, "%v", err)
		}
		return hidePaths(refuse(workspace.ClassFailed, "starting the server: %v", err), false, real)
	}
	srv.mu.Lock()
	stopped := srv.stopped
	srv.proc = p
	srv.mu.Unlock()
	if stopped {
		// Stopped, or its workspace removed, while it started.
		_ = p.Kill()
		return refuse(workspace.ClassFailed, "the server was stopped while it started")
	}
	safego.Go(context.WithoutCancel(ctx), "workspace.worker.server-output", func() {
		_, _ = io.Copy(srv.log, p.Stdout())
		<-p.Done()
		srv.endedAt = time.Now().UTC()
		close(srv.ended)
	})
	select {
	case <-srv.ended:
	case <-time.After(startWatch):
	case <-ctx.Done():
	}
	info := srv.snapshot(0)
	if !info.Running {
		// It ended at once: say why.
		info = srv.snapshot(defaultLogTail)
	}
	out, err := json.Marshal(info)
	if err != nil {
		return refuse(workspace.ClassFailed, "encoding the answer: %v", err)
	}
	return answer{output: out}
}

// remove forgets a server that never started.
func (s *servers) remove(id, sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by[id], sid)
}
