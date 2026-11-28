package dispatch_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// helloTools attaches a fake worker on engine carrying toolchains.
func helloTools(t *testing.T, target, tok, declared, engine string, toolchains ...string) *fakeWorker {
	t.Helper()
	f := dialFake(t, target, tok)
	if _, err := f.hello(&workspacepb.Hello{WorkerId: declared, ProtocolVersion: workspace.ProtocolVersion, Engine: engine,
		Capacity: 4, Toolchains: toolchains}); err != nil {
		t.Fatalf("attach %s: %v", declared, err)
	}
	f.heartbeat(20 * time.Millisecond)
	return f
}

// toolchainFleet attaches, besides each other: a generic host worker and a
// C++/Go host worker of acme, a Rust worker of another tenant, a shared
// container worker with Java, and a personal worker of acme's alice with
// Ruby.
func toolchainFleet(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d, target := core(t, newMemStore(), personalOn())
	helloTools(t, target, tokAcme, "generic", workspace.EngineHost, "python", "node")
	helloTools(t, target, tokAcme2, "dev", workspace.EngineHost, "python", "node", "go", "cpp")
	helloTools(t, target, tokOther, "rusty", workspace.EngineHost, "rust")
	helloTools(t, target, tokShared, "boxed", workspace.EngineContainer, "python", "java")
	p := dialFake(t, target, tokPersonal)
	if _, err := p.hello(&workspacepb.Hello{WorkerId: "laptop", ProtocolVersion: workspace.ProtocolVersion, Engine: workspace.EngineHost,
		Capacity: 4, Toolchains: []string{"ruby"}, Labels: []string{workspace.RootLabelPrefix + "work"}}); err != nil {
		t.Fatalf("personal attach: %v", err)
	}
	p.heartbeat(20 * time.Millisecond)
	for _, id := range []string{acmeID("generic"), acmeID("dev"), otherID("rusty"), sharedID("boxed"), personalID("acme", "alice", "laptop")} {
		waitAttached(t, d, id)
	}
	return d
}

// RFC HH §3: the toolchains on offer to a caller are the union over the
// workers that could take its workspace, each with its engines. Another
// tenant's worker, a personal worker (it takes only root workspaces, which
// name no toolchain) and a worker below the engine asked for add nothing.
func TestOfferedToolchains_AreTheCallersEligibleWorkersOnly(t *testing.T) {
	d := toolchainFleet(t)
	got := d.OfferedToolchains("acme", "alice", "")
	want := map[string][]string{
		"python": {workspace.EngineContainer, workspace.EngineHost},
		"node":   {workspace.EngineHost},
		"go":     {workspace.EngineHost},
		"cpp":    {workspace.EngineHost},
		"java":   {workspace.EngineContainer},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("acme, any engine: %v, want %v", got, want)
	}
	got = d.OfferedToolchains("acme", "alice", workspace.EngineContainer)
	want = map[string][]string{"python": {workspace.EngineContainer}, "java": {workspace.EngineContainer}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("acme, container required: %v, want %v", got, want)
	}
	got = d.OfferedToolchains("other", "bob", "")
	want = map[string][]string{"rust": {workspace.EngineHost}, "python": {workspace.EngineContainer}, "java": {workspace.EngineContainer}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("other: %v, want %v", got, want)
	}
	// No worker serves a run with no tenant here: nothing is on offer.
	if got := d.OfferedToolchains("", "", ""); len(got) != 0 {
		t.Errorf("no tenant: %v, want none", got)
	}
}

// RFC HH §3: a toolchain both attached workers carry places on the one that
// has it, past a generic worker that is less loaded; one no worker of the
// caller carries is refused no_worker naming it and what is on offer.
func TestPlace_NoWorkerNamesTheMissingToolchainsAndTheOffer(t *testing.T) {
	d := toolchainFleet(t)
	ctx := context.Background()
	got, err := d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", UserID: "alice", Toolchains: []string{"go", "cpp"}})
	if err != nil || len(got) != 1 || got[0] != acmeID("dev") {
		t.Fatalf("place go+cpp = %v, %v; want only the dev worker", got, err)
	}

	_, err = d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", UserID: "alice", Toolchains: []string{"cpp", "rust", "ruby"}})
	var oe *dispatch.OpError
	if !asOpError(err, &oe) || oe.Class != workspace.ClassNoWorker {
		t.Fatalf("place cpp+rust+ruby = %v, want no_worker", err)
	}
	// rust is another tenant's and ruby a personal worker's: missing for
	// acme. cpp is offered, so it is not named missing.
	for _, want := range []string{"rust, ruby", "on offer: cpp, go, java, node, python"} {
		if !strings.Contains(oe.Detail, want) {
			t.Errorf("detail %q does not name %q", oe.Detail, want)
		}
	}
	if strings.Contains(oe.Detail, "rusty") || strings.Contains(oe.Detail, "laptop") {
		t.Errorf("detail %q names a worker", oe.Detail)
	}

	// Each is offered, but no one worker carries both.
	_, err = d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", UserID: "alice", Toolchains: []string{"cpp", "java"}})
	if !asOpError(err, &oe) || oe.Class != workspace.ClassNoWorker || !strings.Contains(oe.Detail, "no one worker carries cpp, java together") {
		t.Fatalf("place cpp+java = %v, want no_worker saying no one worker carries both", err)
	}

	// Under a container requirement, cpp is not on offer.
	_, err = d.Place(ctx, dispatch.PlaceRequest{TenantID: "acme", UserID: "alice", Engine: workspace.EngineContainer, Toolchains: []string{"cpp"}})
	if !asOpError(err, &oe) || !strings.Contains(oe.Detail, "cpp") || !strings.Contains(oe.Detail, "on offer: java, python") {
		t.Fatalf("place cpp on container = %v, want no_worker naming cpp and the container offer", err)
	}
}

func asOpError(err error, oe **dispatch.OpError) bool { return errors.As(err, oe) }
