package dispatch_test

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
		"no workers:attach":       tokNoScope,
		"an admin token":          tokAdmin,
		"workers:attach and more": tokMixed,
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

// The refusal says which kind: a bearer that does not resolve is Unavailable
// (the resolver cannot tell an unknown token from a store it could not
// read, so the worker retries), a resolved token that may not attach is
// PermissionDenied (final).
func TestAttach_RefusalCodes(t *testing.T) {
	_, target := core(t, newMemStore(), dispatch.Options{})
	for tok, want := range map[string]codes.Code{tokConversion: codes.Unavailable, tokAdmin: codes.PermissionDenied, tokNoScope: codes.PermissionDenied} {
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

	if err := d.Open(ctx, "acme", "", id, "ws1"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, "ws1")) {
		t.Fatal("open made no directory")
	}
	if ok, err := d.Status(ctx, "acme", "", id, "ws1"); err != nil || !ok {
		t.Fatalf("status = %v, %v", ok, err)
	}
	// An open repeated is harmless.
	if err := d.Open(ctx, "acme", "", id, "ws1"); err != nil {
		t.Fatalf("a repeated open: %v", err)
	}
	if err := d.Open(ctx, "acme", "", id, "ws2"); dispatch.ClassOf(err) != workspace.ClassAtCapacity {
		t.Fatalf("an open past capacity: %v, want at_capacity", err)
	}
	if err := d.Remove(ctx, "acme", "", id, "ws1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Status(ctx, "acme", "", id, "ws1"); err != nil || ok || exists(filepath.Join(root, "ws1")) {
		t.Fatalf("after remove: status = %v, %v", ok, err)
	}
	if _, err := d.Do(ctx, id, dispatch.Request{TenantID: "acme", WorkspaceID: "ws1", Op: "launch"}); dispatch.ClassOf(err) != workspace.ClassUnsupportedOp {
		t.Fatalf("an op the worker lacks: %v, want unsupported_op", err)
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
	if err := d.Open(ctx, "other", "", acmeID("w1"), "ws9"); dispatch.ClassOf(err) != workspace.ClassNoWorker {
		t.Fatalf("another tenant's op: %v, want no_worker", err)
	}
	if exists(filepath.Join(root, "ws9")) {
		t.Fatal("another tenant's op reached the worker")
	}

	// A shared worker must run an isolating engine (a host one is refused at
	// attach), which no real worker of this release does: a fake one.
	shared := dialFake(t, target, tokShared)
	if _, err := shared.hello(&workspacepb.Hello{WorkerId: "s1", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineContainer, Capacity: 4}); err != nil {
		t.Fatal(err)
	}
	shared.heartbeat(time.Second)
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
	st.put(store.WorkspaceRow{ID: "ws1", TenantID: "acme", WorkerID: acmeID("w1"), State: store.WorkspaceReady})
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
		if _, err := d.Status(context.Background(), "acme", "", acmeID("w1"), "ws1"); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
	}

	// A worker holding one stream, busy with an op: the core asks for more.
	f := dialFake(t, target, tokOther)
	f.attach("f1")
	f.heartbeat(100 * time.Millisecond)
	s := f.stream("f1")
	go func() { _, _ = d.Status(context.Background(), "other", "", otherID("f1"), "ws1") }()
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
			if err := d.Open(context.Background(), "acme", "", acmeID("f1"), "ws1"); !errors.Is(err, dispatch.ErrWorkerLost) {
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
		"revoked":                  {nil, codes.Unavailable},
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
			id := acmeID("f1")
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

// A token the core cannot verify (a resolver that fails, or a token
// revoked) never stops the worker: it is detached, refused at every attach
// while the token does not resolve, and served again only once it does.
func TestAttach_AnUnverifiableTokenIsRetriedNeverServed(t *testing.T) {
	const tok = "tok-flaky"
	acme := principals[tokAcme]
	acme.TokenDefID = "def-flaky"
	setToken(t, tok, &acme)
	d, target := core(t, newMemStore(), dispatch.Options{RecheckEvery: 50 * time.Millisecond})
	_, done := realWorker(t, target, tok, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))

	setToken(t, tok, nil) // the resolver now answers nothing for it
	waitFor(t, "the worker to detach", func() bool { return len(d.Workers()) == 0 })
	// Long enough for at least one re-attach attempt (the backoff starts at
	// a second): it is refused, and the worker keeps going.
	time.Sleep(2500 * time.Millisecond)
	if n := len(d.Workers()); n != 0 {
		t.Fatalf("%d workers attached on a token that does not resolve", n)
	}
	if err := d.Open(context.Background(), "acme", "", acmeID("w1"), "ws1"); !errors.Is(err, dispatch.ErrWorkerLost) {
		t.Fatalf("an op while the token does not resolve: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("the worker stopped on a token the core could not verify: %v", err)
	default:
	}

	setToken(t, tok, &acme) // the resolver recovers
	waitAttached(t, d, acmeID("w1"))
}

// A worker's id names what its token grants, not the token: a worker that
// comes back on a rotated token attaches under the id it had, its workspace
// rows still name it, and reconciliation removes nothing of theirs.
func TestAttach_ARotatedTokenKeepsTheWorkerAndItsWorkspaces(t *testing.T) {
	id := acmeID("w1")
	st := newMemStore(store.WorkspaceRow{ID: "ws1", TenantID: "acme", WorkerID: id, State: store.WorkspaceReady})
	d, target := core(t, st, dispatch.Options{})
	root := t.TempDir()
	mkdirs(t, root, "ws1")
	_, _, stop := stoppableWorker(t, target, tokAcme, "w1", worker.Options{Root: root, IdleStreams: 1})
	waitAttached(t, d, id)
	stop()
	waitFor(t, "the first worker to detach", func() bool { return len(d.Workers()) == 0 })

	realWorker(t, target, tokAcme2, "w1", worker.Options{Root: root, IdleStreams: 1})
	waitAttached(t, d, id)
	if ok, err := d.Status(context.Background(), "acme", "", id, "ws1"); err != nil || !ok {
		t.Fatalf("the workspace after the rotation: status = %v, %v", ok, err)
	}
	if !st.has("ws1") || st.state("ws1") != store.WorkspaceReady {
		t.Fatal("the workspace's row changed across the rotation")
	}
}

// Two tokens of one grant cannot attach two workers of one name at once:
// the second is refused AlreadyExists, which a worker retries.
func TestAttach_ASecondWorkerOfTheSameIDIsRefused(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))
	f := dialFake(t, target, tokAcme2)
	_, err := f.hello(&workspacepb.Hello{WorkerId: "w1", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("err = %v, want AlreadyExists", err)
	}
}

// A lost workspace whose directory comes back with its worker is ready again
// while its owner run lives; once that run has ended, or is gone, its
// directory is removed and its row deleted.
func TestAttach_ALostWorkspaceIsRecoveredOrRemovedByItsRun(t *testing.T) {
	id := acmeID("w1")
	lost := func(ws, run string) store.WorkspaceRow {
		return store.WorkspaceRow{ID: ws, TenantID: "acme", WorkerID: id, OwnerRunID: run, State: store.WorkspaceLost}
	}
	st := newMemStore(lost("ws-live", "run-live"), lost("ws-done", "run-done"), lost("ws-gone", "run-gone"))
	st.runs["run-live"] = store.RunRunning
	st.runs["run-done"] = store.RunCompleted
	d, target := core(t, st, dispatch.Options{})
	root := t.TempDir()
	mkdirs(t, root, "ws-live", "ws-done", "ws-gone")
	realWorker(t, target, tokAcme, "w1", worker.Options{Root: root, IdleStreams: 1})
	waitAttached(t, d, id)

	waitFor(t, "the ended runs' workspaces to go", func() bool {
		return !exists(filepath.Join(root, "ws-done")) && !exists(filepath.Join(root, "ws-gone")) && !st.has("ws-done") && !st.has("ws-gone")
	})
	waitFor(t, "the live run's workspace to be ready", func() bool { return st.state("ws-live") == store.WorkspaceReady })
	if !exists(filepath.Join(root, "ws-live")) {
		t.Fatal("the live run's workspace was removed")
	}
}

// A durable workspace that comes back with its worker is never removed,
// whatever became of its last run (RFC GW §8 "Worker death": durable disks
// keep their workspaces): a parked one and one pinned to an ended run are
// ready again with their directories; the sweeper parks the second.
func TestAttach_ALostDurableWorkspaceIsNeverRemoved(t *testing.T) {
	id := acmeID("w1")
	durable := func(ws, owner, kind string) store.WorkspaceRow {
		return store.WorkspaceRow{ID: ws, TenantID: "acme", UserID: "alice", WorkerID: id, OwnerRunID: owner, OwnerKind: kind,
			Lifetime: store.WorkspaceLifetimeDurable, State: store.WorkspaceLost}
	}
	st := newMemStore(durable("ws-parked", store.WorkspaceParkedKey("ws-parked"), store.WorkspaceOwnerParked),
		durable("ws-ended", "run-done", store.WorkspaceOwnerRun), durable("ws-gone", "run-gone", store.WorkspaceOwnerRun))
	st.runs["run-done"] = store.RunCompleted
	d, target := core(t, st, dispatch.Options{})
	root := t.TempDir()
	mkdirs(t, root, "ws-parked", "ws-ended", "ws-gone")
	realWorker(t, target, tokAcme, "w1", worker.Options{Root: root, IdleStreams: 1})
	waitAttached(t, d, id)
	for _, ws := range []string{"ws-parked", "ws-ended", "ws-gone"} {
		waitFor(t, ws+" to be ready", func() bool { return st.state(ws) == store.WorkspaceReady })
		if !exists(filepath.Join(root, ws)) {
			t.Errorf("%s's directory was removed", ws)
		}
	}
}

// A worker that comes back without a ready workspace's directory (a new
// disk) leaves it lost, never ready: its files are gone (RFC GW P12 plan
// §3.4, G-C). A row made in the last minutes may be one another replica is
// opening on the worker as it attaches, and is left; an opening row is left;
// each row marked lost is told to the lost hook.
func TestAttach_AWorkspaceTheWorkerNoLongerHoldsIsLost(t *testing.T) {
	id := acmeID("w1")
	old := time.Now().Add(-10 * time.Minute)
	row := func(ws string, state store.WorkspaceState, created time.Time, lifetime string) store.WorkspaceRow {
		return store.WorkspaceRow{ID: ws, TenantID: "acme", UserID: "alice", WorkerID: id, OwnerRunID: "run-1", OwnerKind: store.WorkspaceOwnerRun,
			Lifetime: lifetime, State: state, CreatedAt: created}
	}
	st := newMemStore(
		row("ws-gone", store.WorkspaceReady, old, store.WorkspaceLifetimeEphemeral),
		row("ws-durable", store.WorkspaceReady, old, store.WorkspaceLifetimeDurable),
		row("ws-new", store.WorkspaceReady, time.Now(), store.WorkspaceLifetimeEphemeral),
		row("ws-opening", store.WorkspaceOpening, old, store.WorkspaceLifetimeEphemeral),
		row("ws-here", store.WorkspaceReady, old, store.WorkspaceLifetimeEphemeral),
	)
	st.runs["run-1"] = store.RunRunning
	d, target := core(t, st, dispatch.Options{})
	var mu sync.Mutex
	var told []string
	d.SetOnLost(func(_ context.Context, r store.WorkspaceRow) {
		mu.Lock()
		told = append(told, r.ID)
		mu.Unlock()
	})
	root := t.TempDir()
	mkdirs(t, root, "ws-here")
	realWorker(t, target, tokAcme, "w1", worker.Options{Root: root, IdleStreams: 1})
	waitAttached(t, d, id)
	waitFor(t, "the gone workspaces to be lost", func() bool {
		return st.state("ws-gone") == store.WorkspaceLost && st.state("ws-durable") == store.WorkspaceLost
	})
	for ws, want := range map[string]store.WorkspaceState{"ws-new": store.WorkspaceReady, "ws-opening": store.WorkspaceOpening, "ws-here": store.WorkspaceReady} {
		if got := st.state(ws); got != want {
			t.Errorf("%s = %s, want %s", ws, got, want)
		}
	}
	waitFor(t, "the lost hook to be told", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(told) == 2
	})
	mu.Lock()
	sort.Strings(told)
	if strings.Join(told, ",") != "ws-durable,ws-gone" {
		t.Errorf("told = %v", told)
	}
	mu.Unlock()
}

// On the host engine every command runs as the worker's user over the
// worker's whole disk, so a shared worker on it would let one tenant's
// command reach another tenant's workspaces. Such an attachment is refused
// for good, and the worker stops; a shared worker on an isolating engine,
// and a tenant-bound host worker, attach.
func TestAttach_ASharedHostWorkerIsRefused(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	_, done := realWorker(t, target, tokShared, "s1", worker.Options{})
	select {
	case err := <-done:
		if !errors.Is(err, worker.ErrRefused) || !strings.Contains(err.Error(), "isolating engine") {
			t.Fatalf("Run = %v, want ErrRefused naming the engine", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a shared host worker kept trying; the refusal must stop it")
	}
	f := dialFake(t, target, tokShared)
	_, err := f.hello(&workspacepb.Hello{WorkerId: "s2", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost, Capacity: 4})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("shared + host: err = %v, want PermissionDenied", err)
	}
	if n := len(d.Workers()); n != 0 {
		t.Fatalf("%d workers attached", n)
	}

	iso := dialFake(t, target, tokShared)
	if _, err := iso.hello(&workspacepb.Hello{WorkerId: "s3", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineContainer, Capacity: 4}); err != nil {
		t.Fatalf("shared + container: %v, want attached", err)
	}
	iso.heartbeat(time.Second)
	waitAttached(t, d, sharedID("s3"))
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))
}

// An attached worker's rows are marked as seen at attach and with each
// recheck, so the sweeper on any replica can tell a live worker's
// workspaces from those of one that went away.
func TestAttach_MarksTheWorkersWorkspacesAsSeen(t *testing.T) {
	st := newMemStore(store.WorkspaceRow{ID: "ws1", TenantID: "acme", WorkerID: acmeID("w1"), State: store.WorkspaceReady})
	d, target := core(t, st, dispatch.Options{RecheckEvery: 50 * time.Millisecond})
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))
	waitFor(t, "two touches", func() bool { return st.touched() >= 2 })
}

// RFC GW P13: with workspace.personal_workers off (Options.PersonalWorkers
// unset), a personal worker token attaches nothing.
func TestAttach_APersonalWorkerTokenIsRefusedWithPersonalWorkersOff(t *testing.T) {
	d, target := core(t, newMemStore(), dispatch.Options{})
	f := dialFake(t, target, tokPersonal)
	_, err := f.hello(&workspacepb.Hello{WorkerId: "w1", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost, Capacity: 4})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("personal: err = %v, want PermissionDenied", err)
	}
	if n := len(d.Workers()); n != 0 {
		t.Fatalf("%d workers attached", n)
	}
}
