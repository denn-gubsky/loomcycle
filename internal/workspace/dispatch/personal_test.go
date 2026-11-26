package dispatch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// RFC GW P13: a personal worker serves one user's root workspaces.

// personalID is the core-side id of a personal worker of user in tenant
// (D-P13-5): the user named by a hash, so the id holds only safe bytes and
// never the token.
func personalID(tenant, user, declared string) string {
	h := sha256.Sum256([]byte(user))
	return "user:" + tenant + "/" + hex.EncodeToString(h[:])[:16] + "/" + declared
}

func personalOn() dispatch.Options {
	return dispatch.Options{PersonalWorkers: func() bool { return true }}
}

// helloPersonal attaches a fake personal worker offering root "work".
func helloPersonal(t *testing.T, target, tok, declared string) *fakeWorker {
	t.Helper()
	f := dialFake(t, target, tok)
	if _, err := f.hello(&workspacepb.Hello{WorkerId: declared, ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost,
		Capacity: 4, Labels: []string{workspace.RootLabelPrefix + "work"}}); err != nil {
		t.Fatalf("personal attach: %v", err)
	}
	f.heartbeat(20 * time.Millisecond)
	return f
}

// RFC GW §11 "A personal worker receives no run of another user": placement
// gives it only a root open of its own (tenant, user) naming a root it
// offers; another user of the tenant, another tenant, an ordinary open, and
// a root it does not offer get no_worker.
func TestPersonalWorker_ReceivesNoRunOfAnotherUserOrTenant(t *testing.T) {
	d, target := core(t, newMemStore(), personalOn())
	helloPersonal(t, target, tokPersonal, "laptop")
	id := personalID("acme", "alice", "laptop")
	waitAttached(t, d, id)
	ctx := context.Background()
	got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", UserID: "alice", Root: "work"})
	if err != nil || len(got) != 1 || got[0] != id {
		t.Fatalf("alice's root open = %v, %v; want %s", got, err, id)
	}
	for name, req := range map[string]dispatch.PlaceRequest{
		"another user of the tenant": {TenantID: "acme", UserID: "bob", Root: "work"},
		"no user":                    {TenantID: "acme", Root: "work"},
		"another tenant":             {TenantID: "other", UserID: "alice", Root: "work"},
		"an ordinary open":           {TenantID: "acme", UserID: "alice"},
		"a root it does not offer":   {TenantID: "acme", UserID: "alice", Root: "docs"},
	} {
		if got, err := d.Place(ctx, req); !isClass(err, workspace.ClassNoWorker) {
			t.Errorf("%s: placed on %v (%v), want no_worker", name, got, err)
		}
	}
	// A tenant worker takes ordinary opens of every user, never a root.
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	waitAttached(t, d, acmeID("w1"))
	if got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", UserID: "alice"}); err != nil || len(got) != 1 || got[0] != acmeID("w1") {
		t.Errorf("an ordinary open = %v, %v; want only the tenant worker", got, err)
	}
	if got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", UserID: "bob", Root: "work"}); !isClass(err, workspace.ClassNoWorker) {
		t.Errorf("bob's root open = %v, %v; want no_worker", got, err)
	}
}

// D-P13-12: an op reaches a personal worker only for a request of its own
// (tenant, user). Another user, no user at all (a call site that forgot it)
// and another tenant are refused before anything is sent.
func TestPersonalWorker_OpForAnotherUsersRowIsRefused(t *testing.T) {
	d, target := core(t, newMemStore(), personalOn())
	user := t.TempDir()
	realWorker(t, target, tokPersonal, "laptop", worker.Options{IdleStreams: 1, Personal: true, Roots: []worker.Root{{Name: "work", Path: user}}})
	id := personalID("acme", "alice", "laptop")
	waitAttached(t, d, id)
	ctx := context.Background()
	for name, req := range map[string]dispatch.Request{
		"another user":   {TenantID: "acme", UserID: "bob", WorkspaceID: "ws1", Op: workspace.OpStatus},
		"no user":        {TenantID: "acme", WorkspaceID: "ws1", Op: workspace.OpStatus},
		"another tenant": {TenantID: "other", UserID: "alice", WorkspaceID: "ws1", Op: workspace.OpStatus},
	} {
		if _, err := d.Do(ctx, id, req); !isClass(err, workspace.ClassNoWorker) {
			t.Errorf("%s: %v, want no_worker", name, err)
		}
	}
	if _, err := d.Do(ctx, id, dispatch.Request{TenantID: "acme", UserID: "alice", WorkspaceID: "ws1", Op: workspace.OpStatus}); err != nil {
		t.Errorf("alice's own op: %v", err)
	}
	if exists, err := d.Status(ctx, "acme", "bob", id, "ws1"); err == nil || exists {
		t.Errorf("status for bob: %v, %v; want refused", exists, err)
	}
	if !d.Attached(id, "acme", "alice") || d.Attached(id, "acme", "bob") || d.Attached(id, "acme", "") {
		t.Error("Attached does not hold a personal worker to its user")
	}
}

// A personal worker attaches only with workspace.personal_workers on, and a
// revoked token, or the setting turned off, detaches it within one check.
func TestPersonalWorker_RevokedTokenOrSwitchOffDetachesWithinOneCheck(t *testing.T) {
	_, target := core(t, newMemStore(), dispatch.Options{})
	f := dialFake(t, target, tokPersonal)
	if _, err := f.hello(&workspacepb.Hello{WorkerId: "laptop", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost, Capacity: 4}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("switch unset: %v, want PermissionDenied", err)
	}

	for name, change := range map[string]func(*atomic.Bool){
		"revoked":    func(*atomic.Bool) { setToken(t, tokPersonal, nil) },
		"switch off": func(on *atomic.Bool) { on.Store(false) },
	} {
		t.Run(name, func(t *testing.T) {
			principalsMu.Lock()
			p := principals[tokPersonal]
			principalsMu.Unlock()
			setToken(t, tokPersonal, &p)
			var on atomic.Bool
			on.Store(true)
			d, target := core(t, newMemStore(), dispatch.Options{RecheckEvery: 50 * time.Millisecond, PersonalWorkers: on.Load})
			helloPersonal(t, target, tokPersonal, "laptop")
			id := personalID("acme", "alice", "laptop")
			waitAttached(t, d, id)
			change(&on)
			waitFor(t, "the personal worker detached", func() bool { return !d.Attached(id, "acme", "alice") })
		})
	}
}

// Two users' personal workers that declare the same id get distinct ids,
// and neither takes a tenant worker's id.
func TestPersonalWorker_CannotTakeAnotherWorkersID(t *testing.T) {
	bob := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-bob",
		Restrictions: auth.Restrictions{WorkerPersonal: true}}
	setToken(t, "tok-bob", &bob)
	d, target := core(t, newMemStore(), personalOn())
	helloPersonal(t, target, tokPersonal, "w1")
	helloPersonal(t, target, "tok-bob", "w1")
	realWorker(t, target, tokAcme, "w1", worker.Options{IdleStreams: 1})
	for _, id := range []string{personalID("acme", "alice", "w1"), personalID("acme", "bob", "w1"), acmeID("w1")} {
		waitAttached(t, d, id)
	}
}

// The attach of a personal worker is held to a loopback peer, behind the
// boot check (D-P13-8).
func TestPersonalWorker_RefusedFromANonLoopbackPeer(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:5000": true, "[::1]:5000": true, "10.0.0.7:5000": false, "192.168.1.2:1": false} {
		a, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := dispatch.LoopbackPeer(a); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	if dispatch.LoopbackPeer(nil) {
		t.Error("no peer address reads as loopback")
	}
}

func isClass(err error, class string) bool {
	var oe *dispatch.OpError
	return errors.As(err, &oe) && oe.Class == class
}
