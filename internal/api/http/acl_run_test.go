package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
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
)

const workerBearer = "tok-worker"

// aclRunFixture is a server with a `worker` agent that holds Read, Bash and
// Agent, a `child` it may spawn, one member token of tenant acme (subject
// dev), the RFC EY resolver in compat mode with a ttl of zero, and a
// scripted provider.
func aclRunFixture(t *testing.T, scripts ...[]providers.Event) (*httptest.Server, store.Store) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{"Read", "Bash", "Agent"}, SystemPrompt: "worker"},
		"child":  {Model: "stub-model", Tools: []string{"Read"}, SystemPrompt: "child"},
	}
	done := []providers.Event{
		{Type: providers.EventText, Text: "done"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "aclrun.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_worker", Name: "u-def_worker", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, workerBearer), AllowedScopes: member,
	}); err != nil {
		t.Fatal(err)
	}
	reg := []tools.Tool{namedStubTool{"Read"}, namedStubTool{"Bash"}}
	srv := New(cfg, &stubResolver{p: &scriptedProvider{scripts: scripts, defaultS: done}}, reg, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeCompat, 0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, st
}

// namedStubTool is a registered tool that answers every call.
type namedStubTool struct{ name string }

func (n namedStubTool) Name() string                 { return n.name }
func (n namedStubTool) Description() string          { return "stub " + n.name }
func (n namedStubTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n namedStubTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Text: n.name + " ran"}, nil
}

func runEvents(t *testing.T, st store.Store, agentID string) []store.Event {
	t.Helper()
	run, err := st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := st.GetRunEventsSince(context.Background(), run.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func hasEvent(evs []store.Event, typ, contains string) bool {
	for _, ev := range evs {
		if ev.Type == typ && strings.Contains(string(ev.Payload), contains) {
			return true
		}
	}
	return false
}

// The owner's third case, inside a run: `deny invoke tool:Bash` bound to the
// caller removes Bash from the agent's schema — the model's call to it is
// unknown, the run reports the removal once at start — while Read, which the
// rule does not name, still runs.
func TestACLRun_DeniedToolIsWithheldFromTheRun(t *testing.T) {
	callBash := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Bash", Input: json.RawMessage(`{"command":"id"}`)}},
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_2", Name: "Read", Input: json.RawMessage(`{"path":"x"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	// The provider answers in CALL order: run one makes two calls (the
	// tool turn, then the turn after the results), run two the same.
	done := []providers.Event{
		{Type: providers.EventText, Text: "done"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	ts, st := aclRunFixture(t, callBash, done, callBash)
	ctx := context.Background()
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-shell", Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-shell", SubjectKind: "user", SubjectID: "dev"}); err != nil {
		t.Fatal(err)
	}
	body := `{"agent":"worker","agent_id":"a_w1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, out := bearerCall(t, ts, workerBearer, "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	evs := runEvents(t, st, "a_w1")
	if !hasEvent(evs, "capability_inert", `"gate":"acl"`) || !hasEvent(evs, "capability_inert", `"tool":"Bash"`) {
		t.Errorf("no acl inert-grant event for Bash; events: %s", aclEventTypes(evs))
	}
	if !hasEvent(evs, "tool_result", "Read ran") {
		t.Errorf("Read should still run; events: %s", aclEventTypes(evs))
	}
	if hasEvent(evs, "tool_result", "Bash ran") {
		t.Errorf("Bash ran despite the deny; events: %s", aclEventTypes(evs))
	}
	// The same run without the binding runs Bash and reports nothing.
	if ok, err := st.AclBindingRetire(ctx, "acme", firstBindingID(t, st)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	body2 := strings.Replace(body, "a_w1", "a_w2", 1)
	if code, out := bearerCall(t, ts, workerBearer, "POST", "/v1/runs", body2); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	evs = runEvents(t, st, "a_w2")
	if !hasEvent(evs, "tool_result", "Bash ran") || hasEvent(evs, "capability_inert", `"gate":"acl"`) {
		t.Errorf("after retiring the binding Bash should run with no inert event; events: %s", aclEventTypes(evs))
	}
}

// `deny spawn agent:child` bound to the caller stops the parent's Agent tool
// from starting the child; the parent itself still runs.
func TestACLRun_DeniedSpawnIsRefusedAtTheAgentTool(t *testing.T) {
	spawn := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"work"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	ts, st := aclRunFixture(t, spawn)
	ctx := context.Background()
	if _, err := st.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-fanout", Rules: json.RawMessage(`[{"effect":"deny","actions":["spawn"],"resources":["agent:*"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-fanout", SubjectKind: "user", SubjectID: "dev"}); err != nil {
		t.Fatal(err)
	}
	body := `{"agent":"worker","agent_id":"a_p1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, out := bearerCall(t, ts, workerBearer, "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	evs := runEvents(t, st, "a_p1")
	if !hasEvent(evs, "tool_result", "acl_denied") {
		t.Errorf("the Agent call should fail with acl_denied; events: %s", aclEventTypes(evs))
	}
	parent, _ := st.GetRunByAgentID(ctx, "a_p1")
	if kids, _ := st.ListRunsByParentRunID(ctx, parent.ID); len(kids) != 0 {
		t.Errorf("a child was started: %+v", kids)
	}
}

func firstBindingID(t *testing.T, st store.Store) string {
	t.Helper()
	rows, err := st.AclBindingList(context.Background(), "acme", false)
	if err != nil || len(rows) == 0 {
		t.Fatalf("bindings: %v %v", rows, err)
	}
	return rows[0].BindingID
}

func aclEventTypes(evs []store.Event) string {
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return strings.Join(out, ",")
}
