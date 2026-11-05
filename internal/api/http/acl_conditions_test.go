package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FY Part E: every condition a rule may carry is filled by the surface
// that decides it. Before, the gates decided with the transport and the clock
// alone, so a deny on any other field never applied.

// bindTenantWide stores one list and binds it to the whole of tenant acme.
func bindTenantWide(t *testing.T, st store.Store, name, rules string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: name, Rules: json.RawMessage(rules)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: name, SubjectKind: "tenant"}); err != nil {
		t.Fatal(err)
	}
}

const condWorkerRun = `{"agent":"worker","agent_id":"%s","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]%s}`

func startCondWorker(t *testing.T, ts *httptest.Server, agentID, extra string) (int, string) {
	t.Helper()
	body := strings.Replace(strings.Replace(condWorkerRun, "%s", agentID, 1), "%s", extra, 1)
	return bearerCall(t, ts, workerBearer, "POST", "/v1/runs", body)
}

// The audit's probe: in enforce mode a tenant-wide deny of a run from any
// address refuses the run at intake; a deny for an address range the client
// is not in does not.
func TestACLGate_ASourceCIDRDenyAppliesAtRunIntake(t *testing.T) {
	ts, st := aclRunFixtureMode(t, authz.ModeEnforce)
	bindTenantWide(t, st, "elsewhere", `[{"effect":"deny","actions":["run"],"resources":["agent:worker"],"when":{"source_cidr":["203.0.113.0/24"]}}]`)
	if code, out := startCondWorker(t, ts, "a_cidr_1", ""); code != 200 {
		t.Fatalf("a deny for another address range refused the run: %d %s", code, out)
	}
	bindTenantWide(t, st, "anywhere", `[{"effect":"deny","actions":["run"],"resources":["agent:worker"],"when":{"source_cidr":["0.0.0.0/0","::/0"]}}]`)
	code, out := startCondWorker(t, ts, "a_cidr_2", "")
	if code != 403 || !strings.Contains(out, "acl_denied") {
		t.Errorf("a deny from every address: %d %s, want 403 acl_denied", code, out)
	}
}

// A run request names its own user tier, so the tier is no fact a rule may
// rely on: a deny on it is refused at write, and one stored before holds for
// every run whatever tier the request asserts — asserting another tier does
// not evade it (an allow on it never holds: TestEvaluate_AnUnboundTierNeverSatisfiesAnAllow).
func TestACLGate_ARunCannotAssertItsWayPastAUserTierDeny(t *testing.T) {
	ts, st := aclRunFixtureMode(t, authz.ModeCompat)
	code, out := bearerCall(t, ts, "legacy-admin-secret", "POST", "/v1/_acldef",
		`{"op":"create","name":"no-free","rules":[{"effect":"deny","actions":["run"],"resources":["agent:worker"],"when":{"user_tier":["free"]}}]}`)
	if code != 400 || !strings.Contains(out, "rule_not_enforceable") {
		t.Errorf("a rule on user_tier was written: %d %s", code, out)
	}
	bindTenantWide(t, st, "no-free", `[{"effect":"deny","actions":["run"],"resources":["agent:worker"],"when":{"user_tier":["free"]}}]`)
	for i, extra := range []string{`,"user_tier":"pro"`, `,"user_tier":"free"`, ""} {
		if code, out := startCondWorker(t, ts, fmt.Sprintf("a_ut_%d", i), extra); code != 403 || !strings.Contains(out, "acl_denied") {
			t.Errorf("request %q: %d %s, want 403 acl_denied", extra, code, out)
		}
	}
}

// An interactive deny holds for an interactive run only.
func TestACLGate_AnInteractiveDenyAppliesAtRunIntake(t *testing.T) {
	ts, st := aclRunFixtureMode(t, authz.ModeCompat)
	bindTenantWide(t, st, "no-terminal", `[{"effect":"deny","actions":["run"],"resources":["agent:worker"],"when":{"interactive":true}}]`)
	if code, out := startCondWorker(t, ts, "a_int_1", ""); code != 200 {
		t.Fatalf("a run that is not interactive was refused: %d %s", code, out)
	}
	if code, out := startCondWorker(t, ts, "a_int_2", `,"interactive":true`); code != 403 || !strings.Contains(out, "acl_denied") {
		t.Errorf("an interactive run: %d %s, want 403 acl_denied", code, out)
	}
}

// An agent condition reads the run's own agent inside the run: Bash is
// withheld from worker and not from a run the rule does not describe.
func TestACLGate_AnAgentConditionReadsTheRunsAgent(t *testing.T) {
	ts, st := aclRunFixtureMode(t, authz.ModeCompat)
	bindTenantWide(t, st, "other", `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"],"when":{"agent":"child"}}]`)
	if code, out := startCondWorker(t, ts, "a_agent_1", ""); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if evs := runEvents(t, st, "a_agent_1"); hasEvent(evs, "capability_inert", `"tool":"Bash"`) {
		t.Error("a deny for another agent withheld Bash")
	}
	bindTenantWide(t, st, "this", `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"],"when":{"agent":"worker"}}]`)
	if code, out := startCondWorker(t, ts, "a_agent_2", ""); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if evs := runEvents(t, st, "a_agent_2"); !hasEvent(evs, "capability_inert", `"tool":"Bash"`) {
		t.Errorf("a deny for the run's own agent did not withhold Bash; events: %s", aclEventTypes(evs))
	}
}

// A depth bound reads the spawn depth: a deny of Read for top-level runs
// (depth at most 0) withholds it from worker and not from the child it
// spawns, which runs Read.
func TestACLGate_ADepthDenyReadsTheSpawnDepth(t *testing.T) {
	spawn := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"work"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	childRead := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_c1", Name: "Read", Input: json.RawMessage(`{"path":"x"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	ts, st := aclRunFixtureMode(t, authz.ModeCompat, spawn, childRead)
	bindTenantWide(t, st, "top-no-read", `[{"effect":"deny","actions":["invoke"],"resources":["tool:Read"],"when":{"depth":{"max":0}}}]`)
	if code, out := startCondWorker(t, ts, "a_depth", ""); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if evs := runEvents(t, st, "a_depth"); !hasEvent(evs, "capability_inert", `"tool":"Read"`) {
		t.Errorf("the top-level run kept Read; events: %s", aclEventTypes(evs))
	}
	parent, err := st.GetRunByAgentID(context.Background(), "a_depth")
	if err != nil {
		t.Fatal(err)
	}
	kids, err := st.ListRunsByParentRunID(context.Background(), parent.ID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("children: %+v %v", kids, err)
	}
	evs, err := st.GetRunEventsSince(context.Background(), kids[0].ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if hasEvent(evs, "capability_inert", `"tool":"Read"`) || !hasEvent(evs, "tool_result", "Read ran") {
		t.Errorf("the child at depth 1 lost Read; events: %s", aclEventTypes(evs))
	}
}

// The transport a condition reads: mcp on the MCP endpoint, webui for a
// cookie-authenticated request, http otherwise.
func TestRequestTransport_NamesMCPAndTheWebUI(t *testing.T) {
	for _, c := range []struct {
		path   string
		cookie bool
		want   string
	}{
		{"/v1/_mcp", false, "mcp"},
		{"/v1/runs", true, "webui"},
		{"/v1/runs", false, "http"},
	} {
		r := httptest.NewRequest("POST", c.path, nil)
		if got := requestTransport(r, c.cookie); got != c.want {
			t.Errorf("%s cookie=%v: %q, want %q", c.path, c.cookie, got, c.want)
		}
	}
}

// A resumed run has no recorded client address, so a source_cidr condition is
// unknown for it: a deny on it holds after the resume as it held at the start
// (fail closed) — before, the unknown address lifted the deny and the resumed
// run wrote the scope its caller may not write from anywhere.
func TestACLGate_ASourceCIDRDenyHoldsForAResumedRun(t *testing.T) {
	e := keyspaceWriteFixture(t, authz.ModeCompat, `[{"effect":"deny","actions":["write"],"resources":["memory:tenant"],"when":{"source_cidr":["0.0.0.0/0","::/0"]}}]`,
		[][]providers.Event{
			toolCallTurn("tu_probe", "Probe", `{}`), endTurn(),
			toolCallTurn("tu_resumed_set", "Memory", `{"op":"set","scope":"tenant","key":"after-resume","value":"x"}`), endTurn(),
		})
	run := e.run(t, "a_keeper_cidr")
	waitFor(t, "the live probe", func() bool { return len(e.probe.calls()) >= 1 })
	if live := e.probe.calls()[0]; !slices.Equal(live.Memory.ReadOnlyScopes, []string{"tenant"}) {
		t.Fatalf("live policy: read-only %v; the deny from every address did not hold at the start", live.Memory.ReadOnlyScopes)
	}
	resumeAndFinish(t, e.srv, run)
	if res := resumedToolResult(t, e.srv, run, "tu_resumed_set"); !strings.Contains(res, "read-only") {
		t.Errorf("the resumed run wrote the scope a source_cidr deny covers: %s", res)
	}
}
