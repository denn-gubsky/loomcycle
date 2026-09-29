package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A parent pins its child to an AgentDef version by def_id, a handle global
// across tenants. The pinned version must be one the child's name could
// resolve to in the parent's tenant: that tenant's own, or the shared one's.

// pinTenantFixture is a server where "worker" resolves, in every tenant, to a
// shared active version, and three more versions of it exist: acme's, globex's
// and a second shared one; plus a globex version of another agent.
func pinTenantFixture(t *testing.T) *versionFixture {
	t.Helper()
	f := newVersionFixture(t, versionConfig())
	putAgentDef(t, f.st, "def_worker_base", "worker", 1, workerDef("prompt base", "Narrow"), true)
	putAgentDef(t, f.st, "def_worker_shared", "worker", 2, workerDef("prompt shared pin", "Narrow"), false)
	for _, r := range []store.AgentDefRow{
		{DefID: "def_worker_acme", Name: "worker", Version: 1, TenantID: "acme", Definition: []byte(workerDef("prompt acme pin", "Narrow"))},
		{DefID: "def_worker_globex", Name: "worker", Version: 1, TenantID: "globex", Definition: []byte(workerDef("prompt globex pin", "Narrow", "Wider"))},
		{DefID: "def_secret_globex", Name: "secret-agent", Version: 1, TenantID: "globex", Definition: []byte(workerDef("prompt secret", "Narrow"))},
	} {
		r.CreatedAt = time.Now()
		if _, err := f.st.AgentDefCreate(context.Background(), r); err != nil {
			t.Fatalf("AgentDefCreate %s: %v", r.DefID, err)
		}
	}
	return f
}

// spawnPinnedFromAcme spawns worker, pinned to defID, from a parent in acme.
func spawnPinnedFromAcme(t *testing.T, f *versionFixture, defID string) (string, error) {
	t.Helper()
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		UserID: "alice", AgentID: "a_acme_parent", TenantID: "acme",
	})
	_, _, runID, err := f.srv.runSubAgent(ctx, "worker", "", "hi", defID)
	return runID, err
}

func TestSubAgent_PinnedToAnotherTenantsVersionIsRefusedAsUnknown(t *testing.T) {
	f := pinTenantFixture(t)

	_, missing := spawnPinnedFromAcme(t, f, "def_no_such")
	if missing == nil {
		t.Fatal("pinned to a def_id that does not exist: spawned, want refused")
	}
	unknown := strings.ReplaceAll(missing.Error(), "def_no_such", "<id>")

	for _, defID := range []string{
		"def_worker_globex", // globex's version of the same agent
		"def_secret_globex", // globex's version of an agent acme has no name for
	} {
		t.Run(defID, func(t *testing.T) {
			runID, err := spawnPinnedFromAcme(t, f, defID)
			if err == nil {
				t.Fatalf("pinned to %s from acme: spawned run %s, want refused", defID, runID)
			}
			got := strings.ReplaceAll(err.Error(), defID, "<id>")
			if got != unknown {
				t.Errorf("refusal = %q, want the unknown-def_id refusal %q", got, unknown)
			}
			if strings.Contains(got, "globex") || strings.Contains(got, "secret-agent") {
				t.Errorf("refusal %q names another tenant's definition", err)
			}
		})
	}
	if n := len(f.prov.requests()); n != 0 {
		t.Errorf("the provider saw %d calls, want none: a refused child must not run", n)
	}
	if n := f.wider.calls.Load(); n != 0 {
		t.Errorf("Wider, which only globex's version grants, ran %d time(s)", n)
	}
}

func TestSubAgent_PinnedToOwnOrSharedVersionRunsOnIt(t *testing.T) {
	for _, tc := range []struct {
		defID, prompt string
	}{
		{"def_worker_acme", "prompt acme pin"},
		{"def_worker_shared", "prompt shared pin"},
	} {
		t.Run(tc.defID, func(t *testing.T) {
			f := pinTenantFixture(t)
			if _, err := spawnPinnedFromAcme(t, f, tc.defID); err != nil {
				t.Fatalf("pinned to %s from acme: %v", tc.defID, err)
			}
			if sys := systemText(f.prov.waitForRequests(t, 1)[0]); !strings.Contains(sys, tc.prompt) {
				t.Errorf("the child ran on system prompt %q, want its pinned version's %q", sys, tc.prompt)
			}
		})
	}
}
