package dispatch_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/workerlink"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// tokLocal stands for the per-boot token of the worker the runtime starts
// itself (workspace.mode local). The test resolver does not know it, as the
// runtime's token resolver does not.
const tokLocal = "local-per-boot-token-0123456789abcdef0123456789abcdef"

// localCore serves one dispatcher on a loopback port, as core does, and on a
// Unix socket through RegisterLocal, as the runtime serves its local worker.
func localCore(t *testing.T, tenant string) (d *dispatch.Dispatcher, tcp, unix string) {
	t.Helper()
	d, tcp = core(t, newMemStore(), dispatch.Options{LocalToken: tokLocal, LocalTenant: tenant})
	// The socket path stays short: t.TempDir under macOS's temp directory
	// is over sun_path's 104 bytes.
	sock := filepath.Join(shortTempDir(t), "w.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(workerlink.ServerOptions()...)
	d.RegisterLocal(srv)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	return d, tcp, "unix://" + sock
}

// The local worker's token attaches on the private socket, as a worker that
// serves the configured tenant only, under an id no network token can name.
func TestLocal_TokenAttachesOnThePrivateSocketForOneTenant(t *testing.T) {
	d, _, unix := localCore(t, "default")
	realWorker(t, unix, tokLocal, "local", worker.Options{IdleStreams: 1})
	waitAttached(t, d, "local:default/local")
	ws := d.Workers()
	if len(ws) != 1 || ws[0].TenantID != "default" || ws[0].Shared {
		t.Fatalf("workers = %+v, want one serving tenant default, not shared", ws)
	}
	if !d.Attached("local:default/local", "default", "") {
		t.Fatal("the local worker does not serve its tenant")
	}
	if d.Attached("local:default/local", "acme", "") {
		t.Fatal("the local worker serves a tenant it was not configured for")
	}
	if _, err := d.Place(context.Background(), dispatch.PlaceRequest{TenantID: "acme"}); err == nil {
		t.Fatal("a workspace of another tenant was placed on the local worker")
	}
	got, err := d.Place(context.Background(), dispatch.PlaceRequest{TenantID: "default"})
	if err != nil || len(got) != 1 || got[0] != "local:default/local" {
		t.Fatalf("Place(default) = %v, %v; want the local worker", got, err)
	}
}

// The local worker's token is refused on the network endpoint, for good: it
// is accepted on the private socket only.
func TestLocal_TokenIsRefusedOnTheNetworkEndpoint(t *testing.T) {
	d, tcp, _ := localCore(t, "default")
	f := dialFake(t, tcp, tokLocal)
	_, err := f.hello(&workspacepb.Hello{WorkerId: "local", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the local token on the network endpoint: err = %v, want PermissionDenied", err)
	}
	_, done := realWorker(t, tcp, tokLocal, "local", worker.Options{})
	select {
	case err := <-done:
		if !errors.Is(err, worker.ErrRefused) {
			t.Fatalf("Run = %v, want ErrRefused", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker kept trying with the local token on the network endpoint")
	}
	if n := len(d.Workers()); n != 0 {
		t.Fatalf("%d workers attached", n)
	}
}

// The private socket takes the local worker's token and nothing else: an
// operator token that attaches on the network endpoint does not attach there.
func TestLocal_PrivateSocketRefusesEveryOtherToken(t *testing.T) {
	d, _, unix := localCore(t, "default")
	for _, tok := range []string{tokAcme, tokShared, "wrong-" + tokLocal} {
		f := dialFake(t, unix, tok)
		_, err := f.hello(&workspacepb.Hello{WorkerId: "w1", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s on the private socket: err = %v, want PermissionDenied", tok, err)
		}
	}
	if n := len(d.Workers()); n != 0 {
		t.Fatalf("%d workers attached", n)
	}
}

// A network worker of the local worker's tenant, with the local worker's
// declared id, is a second worker: it can neither take the local worker's
// id nor open an Op stream under it.
func TestLocal_ANetworkWorkerOfTheSameTenantCannotTakeItsStreams(t *testing.T) {
	d, tcp, unix := localCore(t, "acme")
	realWorker(t, unix, tokLocal, "local", worker.Options{IdleStreams: 1})
	waitAttached(t, d, "local:acme/local")

	thief := dialFake(t, tcp, tokAcme)
	s := thief.stream("local")
	if _, err := s.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a network stream naming the local worker: err = %v, want PermissionDenied", err)
	}
	realWorker(t, tcp, tokAcme, "local", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("local"))
	if n := len(d.Workers()); n != 2 {
		t.Fatalf("%d workers, want 2", n)
	}
}

// shortTempDir is a fresh directory under /tmp, whose path leaves room for
// a socket name within sun_path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lc-wst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// In open mode a run that names no tenant carries the empty tenant. The
// local worker configured for it is placed for, and runs ops of, the empty
// tenant; no other tenant reaches it.
func TestLocal_TheLocalWorkerServesTheEmptyTenantOfAnOpenModeInstall(t *testing.T) {
	d, _, unix := localCore(t, "")
	realWorker(t, unix, tokLocal, "local", worker.Options{IdleStreams: 2})
	id := "local:/local"
	waitAttached(t, d, id)
	ctx := context.Background()
	got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: ""})
	if err != nil || len(got) != 1 || got[0] != id {
		t.Fatalf("Place(empty tenant) = %v, %v; want the local worker", got, err)
	}
	if !d.Attached(id, "", "") || d.Attached(id, "acme", "") {
		t.Fatal("the local worker of the empty tenant must serve it and no other")
	}
	if err := d.Open(ctx, "", "", id, "ws1"); err != nil {
		t.Fatalf("Open for the empty tenant: %v", err)
	}
	if err := d.Open(ctx, "acme", "", id, "ws2"); err == nil {
		t.Fatal("an op of another tenant ran on the local worker")
	}
}

// The empty tenant is never placed on a network worker, a tenant-bound or a
// shared one, even while the local worker serves it: the exception is the
// local worker's alone.
func TestPlace_ANetworkWorkerIsNeverPlacedForTheEmptyTenant(t *testing.T) {
	d, tcp, _ := localCore(t, "acme")
	realWorker(t, tcp, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))
	if got, err := d.Place(context.Background(), dispatch.PlaceRequest{TenantID: ""}); err == nil {
		t.Fatalf("Place(empty tenant) = %v on a network worker", got)
	}
	if d.Attached(acmeID("w1"), "", "") {
		t.Fatal("a network worker reports serving the empty tenant")
	}
	if err := d.Open(context.Background(), "", "", acmeID("w1"), "ws1"); err == nil {
		t.Fatal("an op of the empty tenant ran on a network worker")
	}
}
