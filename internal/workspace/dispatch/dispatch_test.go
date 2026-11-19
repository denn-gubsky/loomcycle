package dispatch_test

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workerlink"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// A worker whose token may not attach a workspace worker is refused for
// good: the worker stops with ErrRefused, and nothing attaches.
func TestAttach_RefusedTokensStopTheWorker(t *testing.T) {
	for name, tok := range map[string]string{
		"no workers:attach":         tokNoScope,
		"an admin token":            tokAdmin,
		"workers:attach and more":   tokMixed,
		"a conversion worker token": tokConversion,
	} {
		t.Run(name, func(t *testing.T) {
			d, target := core(t, newMemStore(), dispatch.Options{})
			_, done := realWorker(t, target, tok, "w1", worker.Options{})
			select {
			case err := <-done:
				if !errors.Is(err, worker.ErrRefused) {
					t.Fatalf("Run = %v, want ErrRefused", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the worker kept trying; a refused token must stop it")
			}
			if n := len(d.Workers()); n != 0 {
				t.Fatalf("%d workers attached", n)
			}
		})
	}
}

// The refusal says which kind: an unknown bearer is Unauthenticated, a known
// token that may not attach is PermissionDenied.
func TestAttach_RefusalCodes(t *testing.T) {
	_, target := core(t, newMemStore(), dispatch.Options{})
	for tok, want := range map[string]codes.Code{tokConversion: codes.Unauthenticated, tokAdmin: codes.PermissionDenied, tokNoScope: codes.PermissionDenied} {
		f := dialFake(t, target, tok)
		_, err := f.hello(&workspacepb.Hello{WorkerId: "w1", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost})
		if status.Code(err) != want {
			t.Errorf("%s: err = %v, want %s", tok, err, want)
		}
	}
}

// A worker of another protocol version is refused, and says so.
func TestAttach_AnotherProtocolVersionIsRefused(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	f := dialFake(t, target, tokAcme)
	_, err := f.hello(&workspacepb.Hello{WorkerId: "w1", ProtocolVersion: workspace.ProtocolVersion + 1, Engine: workspace.EngineHost})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("err = %v, want FailedPrecondition naming the protocol", err)
	}
	if len(d.Workers()) != 0 {
		t.Fatal("a worker of another version attached")
	}
}

// Before the runtime activates the dispatcher, an attach is answered
// Unavailable, which a worker retries rather than stopping on.
func TestAttach_BeforeActivationIsUnavailable(t *testing.T) {
	d := dispatch.New(dispatch.Options{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := l.Addr().String()
	go func() { _ = d.Serve(l) }()
	t.Cleanup(d.Stop)
	f := dialFake(t, target, tokAcme)
	if _, err := f.hello(&workspacepb.Hello{WorkerId: "w1", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost}); status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

// An Op stream is held under the token that opens it: one naming a worker
// another token attached is refused, and two tokens' workers of one name
// are two workers.
func TestOp_StreamFromAnotherTokenIsRefused(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))

	thief := dialFake(t, target, tokOther)
	s := thief.stream("w1")
	if _, err := s.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a stream for another token's worker: err = %v, want PermissionDenied", err)
	}

	// The other token's own worker of the same name is a second worker.
	realWorker(t, target, tokOther, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, otherID("w1"))
	if n := len(d.Workers()); n != 2 {
		t.Fatalf("%d workers, want 2", n)
	}
}

// The ops of this layer on a real worker: open makes the directory, status
// sees it, remove takes it away, and a worker at capacity refuses an open.
func TestDo_OpenStatusRemoveOnARealWorker(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	root, _ := realWorker(t, target, tokAcme, "w1", worker.Options{Capacity: 1, IdleStreams: 2})
	id := acmeID("w1")
	waitAttached(t, d, id)
	ctx := context.Background()

	if err := d.Open(ctx, "acme", id, "ws1"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, "ws1")) {
		t.Fatal("open made no directory")
	}
	if ok, err := d.Status(ctx, "acme", id, "ws1"); err != nil || !ok {
		t.Fatalf("status = %v, %v", ok, err)
	}
	// An open repeated is harmless.
	if err := d.Open(ctx, "acme", id, "ws1"); err != nil {
		t.Fatalf("a repeated open: %v", err)
	}
	if err := d.Open(ctx, "acme", id, "ws2"); dispatch.ClassOf(err) != workspace.ClassAtCapacity {
		t.Fatalf("an open past capacity: %v, want at_capacity", err)
	}
	if err := d.Remove(ctx, "acme", id, "ws1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Status(ctx, "acme", id, "ws1"); err != nil || ok || exists(filepath.Join(root, "ws1")) {
		t.Fatalf("after remove: status = %v, %v", ok, err)
	}
	if _, err := d.Do(ctx, id, dispatch.Request{TenantID: "acme", WorkspaceID: "ws1", Op: "exec"}); dispatch.ClassOf(err) != workspace.ClassUnsupportedOp {
		t.Fatalf("an op this layer lacks: %v, want unsupported_op", err)
	}
	if _, err := d.Do(ctx, id, dispatch.Request{TenantID: "acme", WorkspaceID: "../x", Op: workspace.OpOpen}); dispatch.ClassOf(err) != workspace.ClassInvalid {
		t.Fatalf("a malformed id: %v, want invalid", err)
	}
	if st := d.Stats(); st.Ops["open/ok"] != 2 || st.Ops["open/at_capacity"] != 1 || st.Workers != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// A tenant-bound worker is never placed for another tenant, nor sent its
// op; a shared worker is placed for any.
func TestPlace_ATenantWorkerServesOnlyItsTenant(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	root, _ := realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))
	ctx := context.Background()

	if got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", Toolchains: []string{"python"}}); err != nil || len(got) != 1 || got[0] != acmeID("w1") {
		t.Fatalf("place for acme = %v, %v", got, err)
	}
	if got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "other"}); dispatch.ClassOf(err) != workspace.ClassNoWorker {
		t.Fatalf("place for another tenant = %v, %v; want no_worker", got, err)
	}
	// A caller holding the worker's id still cannot send it another
	// tenant's op.
	if err := d.Open(ctx, "other", acmeID("w1"), "ws9"); dispatch.ClassOf(err) != workspace.ClassNoWorker {
		t.Fatalf("another tenant's op: %v, want no_worker", err)
	}
	if exists(filepath.Join(root, "ws9")) {
		t.Fatal("another tenant's op reached the worker")
	}

	realWorker(t, target, tokShared, "s1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, sharedID("s1"))
	if got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "other"}); err != nil || len(got) != 1 || got[0] != sharedID("s1") {
		t.Fatalf("place for another tenant with a shared worker = %v, %v", got, err)
	}
	if got, _ := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme"}); len(got) != 2 {
		t.Fatalf("place for acme = %v, want both workers", got)
	}
}

// Placement asks for the toolchain and engine, and counts capacity from the
// store's rows.
func TestPlace_ChecksToolchainEngineAndCapacity(t *testing.T) {
	st := newMemStore()
	d, target := core(t, st, dispatch.Options{})
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1, Capacity: 1, Toolchains: []string{"python"}})
	waitAttached(t, d, acmeID("w1"))
	ctx := context.Background()
	if _, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", Toolchains: []string{"python", "node"}}); dispatch.ClassOf(err) != workspace.ClassNoWorker {
		t.Errorf("a toolchain no worker has: %v", err)
	}
	if _, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", Engine: workspace.EngineContainer}); dispatch.ClassOf(err) != workspace.ClassNoWorker {
		t.Errorf("an engine stronger than the worker's: %v", err)
	}
	st.rows["ws1"] = store.WorkspaceRow{ID: "ws1", TenantID: "acme", WorkerID: acmeID("w1"), State: store.WorkspaceReady}
	if _, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme"}); dispatch.ClassOf(err) != workspace.ClassAtCapacity {
		t.Errorf("a full worker: %v, want at_capacity", err)
	}
}

// A worker's pool refills: more ops than it keeps streams for run one after
// another, and when the core runs low it asks for more streams.
func TestPool_Refills(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))
	for i := range 20 {
		if _, err := d.Status(context.Background(), "acme", acmeID("w1"), "ws1"); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}

	// A worker holding one stream, busy with an op: the core asks for more.
	f := dialFake(t, target, tokOther)
	f.attach("f1")
	f.heartbeat(100 * time.Millisecond)
	s := f.stream("f1")
	go func() { _, _ = d.Status(context.Background(), "other", otherID("f1"), "ws1") }()
	if _, err := s.Recv(); err != nil { // the OpStart; never answered
		t.Fatal(err)
	}
	m, err := f.att.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if n := m.GetOpenStreams().GetN(); n == 0 {
		t.Fatalf("control = %v, want OpenStreams", m)
	}
}

// A worker that stops reading holds the core to a bounded amount of the
// op's input, and the caller's deadline still ends the op.
func TestDo_AStalledReaderHoldsABoundedAmount(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	f := dialFake(t, target, tokAcme)
	f.attach("f1")
	f.heartbeat(100 * time.Millisecond)
	s := f.stream("f1")
	waitFor(t, "the stream", func() bool { return len(d.Workers()) == 1 })

	src := &countingZeros{}
	errc := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := d.Do(ctx, acmeID("f1"), dispatch.Request{TenantID: "acme", WorkspaceID: "ws1", Op: "write", Input: src})
		errc <- err
	}()
	if _, err := s.Recv(); err != nil { // OpStart, then nothing more is read
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Do = %v, want the caller's deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Do did not end at the caller's deadline")
	}
	// One stream window, plus what gRPC queues before it blocks and a chunk.
	if n, bound := src.n.Load(), int64(2*workerlink.StreamWindow); n > bound {
		t.Fatalf("the core read %d bytes of input for a worker that read none; want at most %d", n, bound)
	}
}

// A worker that sends more output than the op's bound fails the op rather
// than growing the core's memory.
func TestDo_OutputPastTheBoundFails(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	f := dialFake(t, target, tokAcme)
	f.attach("f1")
	f.heartbeat(100 * time.Millisecond)
	s := f.stream("f1")
	waitFor(t, "the worker", func() bool { return len(d.Workers()) == 1 })
	errc := make(chan error, 1)
	go func() {
		_, err := d.Do(context.Background(), acmeID("f1"), dispatch.Request{TenantID: "acme", WorkspaceID: "ws1", Op: "read", MaxOutput: 100 << 10})
		errc <- err
	}()
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := s.Send(&workspacepb.OpFromWorker{Msg: &workspacepb.OpFromWorker_Output{Output: &workspacepb.Chunk{Data: make([]byte, workspace.ChunkSize)}}}); err != nil {
			break
		}
	}
	if err := <-errc; dispatch.ClassOf(err) != workspace.ClassOutputTooLarge {
		t.Fatalf("Do = %v, want output_too_large", err)
	}
}

// A worker that dies mid-op fails that op with worker_lost, and its
// heartbeats stopping does the same.
func TestDo_AKilledWorkerFailsItsOpWithWorkerLost(t *testing.T) {
	for name, kill := range map[string]func(f *fakeWorker){
		"connection closed":  func(f *fakeWorker) { _ = f.conn.Close() },
		"heartbeats stopped": func(*fakeWorker) {},
	} {
		t.Run(name, func(t *testing.T) {
			d, target := core(t, newMemStore(), dispatch.Options{HeartbeatTimeout: 300 * time.Millisecond})
			f := dialFake(t, target, tokAcme)
			f.attach("f1")
			s := f.stream("f1")
			waitFor(t, "the worker", func() bool { return len(d.Workers()) == 1 })
			errc := make(chan error, 1)
			go func() {
				_, err := d.Do(context.Background(), acmeID("f1"), dispatch.Request{TenantID: "acme", WorkspaceID: "ws1", Op: workspace.OpStatus})
				errc <- err
			}()
			if _, err := s.Recv(); err != nil {
				t.Fatal(err)
			}
			kill(f)
			select {
			case err := <-errc:
				if !errors.Is(err, dispatch.ErrWorkerLost) {
					t.Fatalf("Do = %v, want worker_lost", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the op outlived its worker")
			}
			waitFor(t, "the worker to detach", func() bool { return len(d.Workers()) == 0 })
			if d.Stats().Deaths != 1 {
				t.Fatalf("deaths = %d, want 1", d.Stats().Deaths)
			}
			// An op for a worker that is gone answers worker_lost at once.
			if err := d.Open(context.Background(), "acme", acmeID("f1"), "ws1"); !errors.Is(err, dispatch.ErrWorkerLost) {
				t.Fatalf("an op for a gone worker: %v", err)
			}
		})
	}
}

// At attach, a workspace released while the worker was away has its
// directory removed and its row deleted; a directory no row knows is
// removed; a releasing row with no directory is deleted; a live one stays.
func TestAttach_ReconcilesHeldWorkspacesWithTheStore(t *testing.T) {
	id := acmeID("w1")
	row := func(ws string, s store.WorkspaceState) store.WorkspaceRow {
		return store.WorkspaceRow{ID: ws, TenantID: "acme", WorkerID: id, State: s}
	}
	st := newMemStore(row("ws-rel", store.WorkspaceReleasing), row("ws-ready", store.WorkspaceReady), row("ws-gone", store.WorkspaceReleasing),
		// Another worker's releasing row of the same id is not this one's.
		store.WorkspaceRow{ID: "ws-x", TenantID: "other", WorkerID: otherID("w1"), State: store.WorkspaceReleasing})
	d, target := core(t, st, dispatch.Options{})
	root := t.TempDir()
	mkdirs(t, root, "ws-rel", "ws-ready", "ws-unknown")
	realWorker(t, target, tokAcme, "w1", worker.Options{Root: root, IdleStreams: 1})
	waitAttached(t, d, id)

	waitFor(t, "the released and unknown directories to go", func() bool {
		return !exists(filepath.Join(root, "ws-rel")) && !exists(filepath.Join(root, "ws-unknown"))
	})
	waitFor(t, "the released rows to be deleted", func() bool { return !st.has("ws-rel") && !st.has("ws-gone") })
	if !exists(filepath.Join(root, "ws-ready")) || !st.has("ws-ready") {
		t.Fatal("a live workspace was removed")
	}
	if !st.has("ws-x") {
		t.Fatal("another worker's row was deleted")
	}
}

type countingZeros struct{ n atomic.Int64 }

func (c *countingZeros) Read(p []byte) (int, error) {
	clear(p)
	c.n.Add(int64(len(p)))
	return len(p), nil
}

var _ io.Reader = (*countingZeros)(nil)

// A worker whose token stops attaching it while it is attached is detached
// within the re-check interval: its op in flight fails worker_revoked, it is
// no longer placed, its attachment ends with a refusal the worker treats as
// permanent, and a new Op stream on the token is refused.
func TestAttach_ATokenThatStopsHoldingDetachesTheWorker(t *testing.T) {
	const tok = "tok-revocable"
	acme := principals[tokAcme]
	acme.TokenDefID = "def-revocable"
	rebound := acme
	rebound.TenantID = "other"
	attachOnly := acme
	attachOnly.Scopes = append([]string{}, acme.Scopes...)
	attachOnly.Scopes = append(attachOnly.Scopes, "runs:create")
	for name, change := range map[string]struct {
		p    *auth.Principal
		code codes.Code
	}{
		"revoked":                  {nil, codes.Unauthenticated},
		"re-bound to other tenant": {&rebound, codes.PermissionDenied},
		"widened past the scope":   {&attachOnly, codes.PermissionDenied},
	} {
		t.Run(name, func(t *testing.T) {
			setToken(t, tok, &acme)
			d, target := core(t, newMemStore(), dispatch.Options{RecheckEvery: 50 * time.Millisecond})
			f := dialFake(t, target, tok)
			f.attach("f1")
			f.heartbeat(20 * time.Millisecond)
			s := f.stream("f1")
			id := "tok:def-revocable/f1"
			waitAttached(t, d, id)
			errc := make(chan error, 1)
			go func() {
				_, err := d.Do(context.Background(), id, dispatch.Request{TenantID: "acme", WorkspaceID: "ws1", Op: workspace.OpStatus})
				errc <- err
			}()
			if _, err := s.Recv(); err != nil { // the OpStart; never answered
				t.Fatal(err)
			}

			setToken(t, tok, change.p)
			select {
			case err := <-errc:
				if !errors.Is(err, dispatch.ErrWorkerRevoked) {
					t.Fatalf("the op in flight: %v, want worker_revoked", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the op in flight outlived its worker's token")
			}
			// The core ends the op's stream too, so the worker stops it.
			ended := make(chan error, 1)
			go func() {
				_, err := s.Recv()
				ended <- err
			}()
			select {
			case err := <-ended:
				if err == nil {
					t.Fatal("the op's stream carried another frame after the revoke")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the op's stream stayed open after the revoke")
			}
			waitFor(t, "the worker to detach", func() bool { return len(d.Workers()) == 0 })
			if _, err := d.Place(context.Background(), dispatch.PlaceRequest{TenantID: "acme"}); dispatch.ClassOf(err) != workspace.ClassNoWorker {
				t.Fatalf("place after the revoke: %v, want no_worker", err)
			}
			// The attachment ends with a code the worker reads as final.
			for {
				_, err := f.att.Recv()
				if err == nil {
					continue
				}
				if status.Code(err) != change.code {
					t.Fatalf("attach ended with %v, want %s", err, change.code)
				}
				break
			}
			// A new stream on the token is refused on its own bearer.
			if _, err := f.stream("f1").Recv(); status.Code(err) != change.code {
				t.Fatalf("a new stream after the revoke: %v", err)
			}
		})
	}
}
