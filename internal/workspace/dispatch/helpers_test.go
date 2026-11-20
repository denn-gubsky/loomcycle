package dispatch_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workerlink"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// The bearers the test resolver knows. None is a real token.
const (
	tokAcme = "tok-acme-worker"
	// tokAcme2 is a second token of acme's grant: a rotation of tokAcme.
	tokAcme2   = "tok-acme-worker-rotated"
	tokOther   = "tok-other-worker"
	tokShared  = "tok-shared-worker"
	tokAdmin   = "tok-admin"
	tokNoScope = "tok-runs-only"
	tokMixed   = "tok-attach-and-runs"
	// tokConversion stands for a LOOMCYCLE_WORKER_TOKEN: a shared secret the
	// conversion service compares, never an operator token.
	tokConversion = "0123456789abcdef0123456789abcdef-conversion"
)

var principals = map[string]auth.Principal{
	tokAcme:    {TenantID: "acme", Subject: "w-acme", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-acme"},
	tokAcme2:   {TenantID: "acme", Subject: "w-acme", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-acme-2"},
	tokOther:   {TenantID: "other", Subject: "w-other", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-other"},
	tokShared:  {TenantID: "ops", Subject: "w-shared", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-shared", Restrictions: auth.Restrictions{WorkerShared: true}},
	tokAdmin:   {TenantID: "ops", Subject: "admin", Scopes: []string{auth.ScopeAdmin}, TokenDefID: "def-admin"},
	tokNoScope: {TenantID: "acme", Subject: "u", Scopes: []string{auth.ScopeRunsCreate}, TokenDefID: "def-runs"},
	tokMixed:   {TenantID: "acme", Subject: "u", Scopes: []string{auth.ScopeWorkersAttach, auth.ScopeRunsCreate}, TokenDefID: "def-mixed"},
}

// principalsMu guards principals: a test revokes or re-binds a token while
// the dispatcher resolves it.
var principalsMu sync.Mutex

func resolve(_ context.Context, bearer string) (auth.Principal, bool) {
	principalsMu.Lock()
	defer principalsMu.Unlock()
	p, ok := principals[bearer]
	return p, ok
}

// setToken makes bearer resolve to p (or to nothing, revoked, when p is
// nil) until the test ends.
func setToken(t *testing.T, bearer string, p *auth.Principal) {
	t.Helper()
	principalsMu.Lock()
	old, had := principals[bearer]
	if p == nil {
		delete(principals, bearer)
	} else {
		principals[bearer] = *p
	}
	principalsMu.Unlock()
	t.Cleanup(func() {
		principalsMu.Lock()
		defer principalsMu.Unlock()
		if had {
			principals[bearer] = old
		} else {
			delete(principals, bearer)
		}
	})
}

// The core-side ids of the workers the tests attach.
func acmeID(declared string) string   { return "tenant:acme/" + declared }
func otherID(declared string) string  { return "tenant:other/" + declared }
func sharedID(declared string) string { return "shared/" + declared }

// memStore is the workspaces table, and the runs a lost row's owner is read
// from, in memory.
type memStore struct {
	mu   sync.Mutex
	rows map[string]store.WorkspaceRow
	runs map[string]store.RunStatus
	// touches counts WorkspaceTouchByWorker calls.
	touches int
}

func newMemStore(rows ...store.WorkspaceRow) *memStore {
	m := &memStore{rows: map[string]store.WorkspaceRow{}, runs: map[string]store.RunStatus{}}
	for _, r := range rows {
		m.rows[r.ID] = r
	}
	return m
}

func (m *memStore) WorkspaceListByWorker(_ context.Context, workerID string, states ...store.WorkspaceState) ([]store.WorkspaceRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.WorkspaceRow
	for _, r := range m.rows {
		if r.WorkerID != workerID {
			continue
		}
		if len(states) > 0 {
			ok := false
			for _, s := range states {
				ok = ok || r.State == s
			}
			if !ok {
				continue
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) WorkspaceDelete(_ context.Context, tenantID, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.TenantID != tenantID {
		return false, nil
	}
	delete(m.rows, id)
	return true, nil
}

func (m *memStore) WorkspaceCountActiveByWorker(_ context.Context, workerID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.WorkerID == workerID && (r.State == store.WorkspaceOpening || r.State == store.WorkspaceReady) {
			n++
		}
	}
	return n, nil
}

func (m *memStore) WorkspaceSetState(_ context.Context, tenantID, id string, from, to store.WorkspaceState) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.TenantID != tenantID || r.State != from {
		return false, nil
	}
	r.State = to
	m.rows[id] = r
	return true, nil
}

func (m *memStore) GetRun(_ context.Context, runID string) (store.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.runs[runID]
	if !ok {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: runID}
	}
	return store.Run{ID: runID, Status: st}, nil
}

func (m *memStore) WorkspaceTouchByWorker(_ context.Context, workerID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if r.WorkerID == workerID && (r.State == store.WorkspaceOpening || r.State == store.WorkspaceReady) {
			r.UpdatedAt = time.Now()
			m.rows[id] = r
			n++
		}
	}
	m.touches++
	return n, nil
}

func (m *memStore) touched() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.touches
}

func (m *memStore) state(id string) store.WorkspaceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[id].State
}

// put writes a row, under the lock: an attach's reconciliation reads the
// table concurrently.
func (m *memStore) put(r store.WorkspaceRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[r.ID] = r
}

func (m *memStore) has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rows[id]
	return ok
}

// core serves a dispatcher on a loopback port, activated with st and the
// test resolver.
func core(t *testing.T, st dispatch.Store, opt dispatch.Options) (*dispatch.Dispatcher, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(opt)
	d.Activate(st, resolve)
	go func() { _ = d.Serve(l) }()
	t.Cleanup(d.Stop)
	return d, l.Addr().String()
}

// realWorker runs `loomcycle worker --role workspace` in this process and
// returns its root and a channel that receives Run's return.
func realWorker(t *testing.T, target, token, id string, opt worker.Options) (string, <-chan error) {
	t.Helper()
	root, done, _ := stoppableWorker(t, target, token, id, opt)
	return root, done
}

// stoppableWorker is realWorker with a stop that returns once Run has.
func stoppableWorker(t *testing.T, target, token, id string, opt worker.Options) (string, <-chan error, func()) {
	t.Helper()
	if opt.Root == "" {
		opt.Root = t.TempDir()
	}
	opt.Target, opt.Token, opt.ID = target, token, id
	if opt.Toolchains == nil {
		opt.Toolchains = []string{"python", "node"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	// done reports Run's return to the test; finished tells the cleanup it
	// returned, whether or not the test read done.
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- worker.Run(ctx, opt)
	}()
	stop := func() { cancel(); <-finished }
	t.Cleanup(stop)
	return opt.Root, done, stop
}

// waitFor polls cond for up to 10s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitAttached(t *testing.T, d *dispatch.Dispatcher, id string) {
	t.Helper()
	waitFor(t, "worker "+id+" to attach", func() bool {
		for _, w := range d.Workers() {
			if w.ID == id {
				return true
			}
		}
		return false
	})
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mkdirs(t *testing.T, root string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := os.MkdirAll(filepath.Join(root, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeWorker speaks the protocol frame by frame, for what a real worker
// never does: stall, die mid-op, lie about its version.
type fakeWorker struct {
	t      *testing.T
	conn   *grpc.ClientConn
	client workspacepb.WorkspaceServiceClient
	ctx    context.Context
	cancel context.CancelFunc
	att    workspacepb.WorkspaceService_AttachClient
}

func dialFake(t *testing.T, target, token string) *fakeWorker {
	t.Helper()
	conn, err := grpc.NewClient(target, append(workerlink.DialOptions(), grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(context.Background(), workspace.MDAuthorization, "Bearer "+token))
	f := &fakeWorker{t: t, conn: conn, client: workspacepb.NewWorkspaceServiceClient(conn), ctx: ctx, cancel: cancel}
	t.Cleanup(func() { cancel(); _ = conn.Close() })
	return f
}

// hello attaches with h and returns the core's first answer (or its error).
func (f *fakeWorker) hello(h *workspacepb.Hello) (*workspacepb.CoreControl, error) {
	f.t.Helper()
	att, err := f.client.Attach(f.ctx)
	if err != nil {
		return nil, err
	}
	f.att = att
	// io.EOF from Send is the core having ended the stream already (a
	// refusal before it read the Hello); Recv returns the status.
	if err := att.Send(&workspacepb.WorkerMessage{Msg: &workspacepb.WorkerMessage_Hello{Hello: h}}); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return att.Recv()
}

// attach attaches as a well-formed worker named id, holding nothing.
func (f *fakeWorker) attach(id string) {
	f.t.Helper()
	if _, err := f.hello(&workspacepb.Hello{WorkerId: id, ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost, Capacity: 4}); err != nil {
		f.t.Fatalf("attach: %v", err)
	}
}

// heartbeat keeps the attachment alive until the fake is closed.
func (f *fakeWorker) heartbeat(every time.Duration) {
	go func() {
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-f.ctx.Done():
				return
			case <-tk.C:
				if f.att.Send(&workspacepb.WorkerMessage{Msg: &workspacepb.WorkerMessage_Heartbeat{Heartbeat: &workspacepb.Heartbeat{}}}) != nil {
					return
				}
			}
		}
	}()
}

// stream opens one Op stream declaring the worker id.
func (f *fakeWorker) stream(id string) workspacepb.WorkspaceService_OpClient {
	f.t.Helper()
	s, err := f.client.Op(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := s.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Ready{Ready: &workspacepb.Ready{WorkerId: id}}}); err != nil {
		f.t.Fatal(err)
	}
	return s
}
