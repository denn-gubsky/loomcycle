package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC EY §3.7: a caller whose rules deny WRITE on a keyspace while allowing
// read runs an agent that may read that keyspace and may not change it. The
// run's policy used to carry only a read-or-nothing scope list, so the deny
// was not enforced at all: the scope stayed in memory_scopes and every write
// op on it went through.

// keyspaceWriteEnv serves `keeper` (the real Memory tool, memory_scopes
// [user, tenant]) to the member acme/dev, under the RFC EY resolver in mode.
type keyspaceWriteEnv struct {
	srv   *Server
	st    store.Store
	ts    *httptest.Server
	probe *policyProbe
}

func keyspaceWriteFixture(t *testing.T, mode authz.Mode, rules string, script [][]providers.Event) *keyspaceWriteEnv {
	t.Helper()
	return keyspaceWriteFixtureScripts(t, mode, rules, map[string][][]providers.Event{"keeper": script})
}

// keyspaceWriteFixtureScripts also serves `child`, which `keeper` may spawn;
// both hold the Probe tool, which records the run policy.
func keyspaceWriteFixtureScripts(t *testing.T, mode authz.Mode, rules string, scripts map[string][][]providers.Event) *keyspaceWriteEnv {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Agents = map[string]config.AgentDef{
		"keeper": {Model: "stub-model", Tools: []string{"Memory", "Probe", "Agent"}, MemoryScopes: []string{"user", "tenant"}, SystemPrompt: "role:keeper."},
		"child":  {Model: "stub-model", Tools: []string{"Memory", "Probe"}, MemoryScopes: []string{"user", "tenant"}, SystemPrompt: "role:child."},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "keyspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_worker", Name: "u-def_worker", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, workerBearer), AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "dev-keyspace", Rules: json.RawMessage(rules)}); err != nil {
		t.Fatal(err)
	}
	bindACL(t, st, "dev-keyspace")
	if err := st.MemorySet(ctx, "acme", store.MemoryScopeTenant, "", "seed", json.RawMessage(`"shared-note"`), 0); err != nil {
		t.Fatal(err)
	}
	prov := &roleProvider{roles: []string{"keeper", "child"}, scripts: scripts, calls: map[string]int{}}
	probe := &policyProbe{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{&builtin.Memory{Store: st}, probe}, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, mode, 0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &keyspaceWriteEnv{srv: srv, st: st, ts: ts, probe: probe}
}

func (e *keyspaceWriteEnv) run(t *testing.T, agentID string) store.Run {
	t.Helper()
	body := `{"agent":"keeper","agent_id":"` + agentID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, out := bearerCall(t, e.ts, workerBearer, "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	run, err := e.st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// memoryCalls is one model turn per call, in order (calls in one turn may run
// concurrently), then the end.
func memoryCalls(calls ...[2]string) [][]providers.Event {
	var turns [][]providers.Event
	for _, c := range calls {
		turns = append(turns, toolCallTurn(c[0], "Memory", c[1]))
	}
	return append(turns, endTurn())
}

func TestACLKeyspace_WriteDenyKeepsReadAndRefusesEveryWrite(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			// The member baseline allows both verbs on every memory scope, so
			// under either mode it is the deny that narrows.
			rules := `[{"effect":"deny","actions":["write"],"resources":["memory:tenant"]}]`
			e := keyspaceWriteFixture(t, mode, rules, memoryCalls(
				[2]string{"tu_get_tenant", `{"op":"get","scope":"tenant","key":"seed"}`},
				[2]string{"tu_set_tenant", `{"op":"set","scope":"tenant","key":"written","value":"x"}`},
				[2]string{"tu_delete_tenant", `{"op":"delete","scope":"tenant","key":"seed"}`},
				[2]string{"tu_set_user", `{"op":"set","scope":"user","key":"mine","value":"y"}`},
			))
			run := e.run(t, "a_keeper_"+string(mode))
			ctx := context.Background()

			if res := resumedToolResult(t, e.srv, run, "tu_get_tenant"); !strings.Contains(res, "shared-note") {
				t.Errorf("a read of the write-denied scope was refused: %s", res)
			}
			for _, id := range []string{"tu_set_tenant", "tu_delete_tenant"} {
				if res := resumedToolResult(t, e.srv, run, id); !strings.Contains(res, "read-only") {
					t.Errorf("%s on the write-denied scope was not refused as read-only: %s", id, res)
				}
			}
			if _, err := e.st.MemoryGet(ctx, "acme", store.MemoryScopeTenant, "", "written"); err == nil {
				t.Error("the write-denied tenant keyspace was written")
			}
			if _, err := e.st.MemoryGet(ctx, "acme", store.MemoryScopeTenant, "", "seed"); err != nil {
				t.Errorf("the write-denied tenant keyspace lost a key: %v", err)
			}
			if res := resumedToolResult(t, e.srv, run, "tu_set_user"); !strings.Contains(res, `\"ok\":true`) {
				t.Errorf("a write to the user scope, which the rules allow, was refused: %s", res)
			}
		})
	}
}

// A sub-agent is bound by the originating caller's rules: the child keyspace
// stays readable and read-only though its own definition grants it.
func TestACLKeyspace_SubAgentKeepsTheCallersWriteDeny(t *testing.T) {
	e := keyspaceWriteFixtureScripts(t, authz.ModeCompat, `[{"effect":"deny","actions":["write"],"resources":["memory:tenant"]}]`,
		map[string][][]providers.Event{
			"keeper": {toolCallTurn("tu_child", "Agent", `{"name":"child","prompt":"work"}`), endTurn()},
			"child": memoryCalls(
				[2]string{"tu_child_get", `{"op":"get","scope":"tenant","key":"seed"}`},
				[2]string{"tu_child_set", `{"op":"set","scope":"tenant","key":"from-child","value":"x"}`},
			),
		})
	e.run(t, "a_keeper_parent")
	kids, err := e.st.ListRunsByParentAgentID(context.Background(), "a_keeper_parent")
	if err != nil || len(kids) != 1 {
		t.Fatalf("children: %d, %v", len(kids), err)
	}
	if res := resumedToolResult(t, e.srv, kids[0], "tu_child_get"); !strings.Contains(res, "shared-note") {
		t.Errorf("the child's read was refused: %s", res)
	}
	if res := resumedToolResult(t, e.srv, kids[0], "tu_child_set"); !strings.Contains(res, "read-only") {
		t.Errorf("the child wrote the keyspace its caller may not write: %s", res)
	}
	if _, err := e.st.MemoryGet(context.Background(), "acme", store.MemoryScopeTenant, "", "from-child"); err == nil {
		t.Error("the child's write landed")
	}
}

// A paused run is narrowed again by its caller's current rules when it
// resumes: the scope is read-only on the live run's policy and stays so after
// the resume.
func TestACLKeyspace_ResumedRunKeepsTheScopeReadOnly(t *testing.T) {
	e := keyspaceWriteFixture(t, authz.ModeCompat, `[{"effect":"deny","actions":["write"],"resources":["memory:tenant"]}]`,
		[][]providers.Event{
			toolCallTurn("tu_probe", "Probe", `{}`), endTurn(),
			toolCallTurn("tu_resumed_set", "Memory", `{"op":"set","scope":"tenant","key":"after-resume","value":"x"}`), endTurn(),
		})
	run := e.run(t, "a_keeper_resume")
	waitFor(t, "the live probe", func() bool { return len(e.probe.calls()) >= 1 })
	live := e.probe.calls()[0]
	if !slices.Equal(live.Memory.ReadOnlyScopes, []string{"tenant"}) || !slices.Contains(live.Memory.AllowedScopes, "tenant") {
		t.Fatalf("live policy: allowed %v read-only %v; want tenant readable and read-only", live.Memory.AllowedScopes, live.Memory.ReadOnlyScopes)
	}
	resumeAndFinish(t, e.srv, run)
	if res := resumedToolResult(t, e.srv, run, "tu_resumed_set"); !strings.Contains(res, "read-only") {
		t.Errorf("the resumed run wrote the read-only scope: %s", res)
	}
}
