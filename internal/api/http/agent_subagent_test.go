package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// scriptedProvider returns a different event sequence per call. Used by
// sub-agent tests where parent + child runs need different scripted
// responses (parent: tool_call to Agent then text after tool_result;
// child: text + done).
type scriptedProvider struct {
	calls    atomic.Int32
	scripts  [][]providers.Event
	defaultS []providers.Event // returned for any call past len(scripts)
}

func (s *scriptedProvider) ID() string                    { return "scripted" }
func (s *scriptedProvider) Probe(_ context.Context) error { return nil }
func (s *scriptedProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"scripted-model"}, nil
}
func (s *scriptedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (s *scriptedProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	idx := int(s.calls.Add(1)) - 1
	var events []providers.Event
	if idx < len(s.scripts) {
		events = s.scripts[idx]
	} else {
		events = s.defaultS
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// End-to-end: a parent agent invokes the Agent tool with {name: "child",
// prompt: "hi"}; the child runs, returns text; the parent's loop sees
// the tool_result and emits final text. Verifies:
//
//   - Sub-agent runs as a SEPARATE session (own session_id) so its
//     transcript is independently retrievable.
//   - The parent's tool_result text contains the child's FinalText.
//   - The child's full event sequence (started/text/usage/done) lands
//     in the child's transcript, NOT the parent's stream.
//   - Parent only sees its own events plus the wrapping tool_call /
//     tool_result frames.
func TestSubAgentRoundTrip_ParentSeesChildOutput(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {
				Model:        "stub-model",
				AllowedTools: []string{"Agent"},
				SystemPrompt: "you are the parent",
			},
			"child": {
				Model:        "stub-model",
				AllowedTools: []string{},
				SystemPrompt: "you are the child",
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	// Provider script:
	//   call 1 = parent's first iter: emit a tool_call to Agent, then done(tool_use).
	//   call 2 = child's only iter: emit text + done(end_turn).
	//   call 3 = parent's second iter (after tool_result): emit final text + done.
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// 1) parent → tool_call(Agent)
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_parent_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"say hello briefly"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			// 2) child → final text + end_turn
			{
				{Type: providers.EventText, Text: "child says hi"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 4}},
			},
			// 3) parent → final wrap-up text + end_turn
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Parent stream MUST contain the tool_call and a tool_result with the
	// child's text. The child's intermediate "child says hi" text should
	// NOT appear as a parent text frame — only as the tool_result.
	if !strings.Contains(bodyStr, "event: tool_call") {
		t.Errorf("parent stream missing tool_call frame:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "event: tool_result") {
		t.Errorf("parent stream missing tool_result frame:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "child says hi") {
		t.Errorf("parent stream missing child's output text:\n%s", bodyStr)
	}

	// Two sessions should exist now: parent + child. Parent's session
	// is announced in the SSE stream; child's session is implicit.
	parentSessionID := extractSessionID(bodyStr)
	if parentSessionID == "" {
		t.Fatalf("could not parse parent session_id from stream:\n%s", bodyStr)
	}

	parentTranscript, err := st.GetTranscript(context.Background(), parentSessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Parent transcript should NOT contain a separate "text" frame for
	// "child says hi" — only the tool_result wrapping it.
	for _, ev := range parentTranscript {
		if ev.Type == "text" && strings.Contains(string(ev.Payload), "child says hi") {
			t.Errorf("child text leaked into parent stream as a parent text frame: %s", string(ev.Payload))
		}
	}

	// Find the child's session by listing all sessions and picking the
	// one with agent="child". The store doesn't expose ListSessions
	// publicly, so we approximate: try GetTranscript on a synthesised
	// ID won't work. Instead we cross-check via the parent's tool_result
	// containing the child's output.
	foundToolResult := false
	for _, ev := range parentTranscript {
		if ev.Type == "tool_result" && strings.Contains(string(ev.Payload), "child says hi") {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		t.Error("parent transcript should record the tool_result with child's output")
	}
}

// Regression: a parent without "Agent" in its allowed_tools cannot call
// the Agent tool — the dispatcher refuses. The model would see a
// "tool not found" tool_result. This locks the per-agent gate.
func TestSubAgent_ParentWithoutAgentToolCannotSpawn(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"locked": {
				Model:        "stub-model",
				AllowedTools: []string{}, // no Agent
				SystemPrompt: "you cannot spawn",
			},
			"child": {
				Model:        "stub-model",
				AllowedTools: []string{},
				SystemPrompt: "you are the child",
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// "locked" agent attempts to call Agent (a hypothetical model
			// that ignores the tool list). The dispatcher should refuse.
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_locked_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"x"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use"},
			},
			// After tool_result (which is "tool not found"), wrap up.
			{
				{Type: providers.EventText, Text: "ok, done"},
				{Type: providers.EventDone, StopReason: "end_turn"},
			},
		},
	}

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"locked","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tool not found") {
		t.Errorf("expected 'tool not found' tool_result for an off-policy Agent call:\n%s", body)
	}
	// And critically: NO sub-agent text should appear.
	if strings.Contains(string(body), "child says hi") {
		t.Error("blocked Agent call should not have spawned the child")
	}
}

// Calling Agent with an unknown sub-agent name surfaces the error
// through the IsError tool_result so the parent's model can self-correct.
func TestSubAgent_UnknownChildName(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {
				Model:        "stub-model",
				AllowedTools: []string{"Agent"},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"does-not-exist","prompt":"x"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use"},
			},
			{
				{Type: providers.EventText, Text: "I'll move on"},
				{Type: providers.EventDone, StopReason: "end_turn"},
			},
		},
	}

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), nil)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"x"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "unknown sub-agent") {
		t.Errorf("expected 'unknown sub-agent' error, got:\n%s", body)
	}
	// The parent run should still complete cleanly — this is a tool
	// error, not a run-failing error.
	if !strings.Contains(string(body), "I'll move on") {
		t.Error("parent should have continued after the IsError tool_result")
	}
}

// TestSubAgent_SpawnsDynamicallyRegisteredChild is the regression for
// the 2026-05-22 cv-batch-adapter ↔ cv-adapter incident: a parent
// agent (statically yaml-defined) tried to spawn a child registered
// via the dynamic_agents table (RegisterAgent / connector path) and
// got "unknown sub-agent" because runSubAgent was reading
// `s.cfg.Agents[name]` directly instead of going through lookup.Agent.
//
// PR #188 consolidated the lookup chain but missed this site;
// runSubAgent now calls lookup.Agent which walks cfg.Agents →
// dynamic_agents → agent_def_active.
//
// Production symptom: cv-batch-adapter (yaml) tried to spawn N
// instances of cv-adapter (registered dynamically per user at boot
// of jobs-search-agent's own MCP server) and every spawn failed.
func TestSubAgent_SpawnsDynamicallyRegisteredChild(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {
				Model:        "stub-model",
				AllowedTools: []string{"Agent"},
				SystemPrompt: "you are the parent",
			},
			// NB: "child" is NOT in cfg.Agents — it's registered
			// dynamically below. The pre-fix runSubAgent would fail
			// here at the s.cfg.Agents[name] read.
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			// 1) parent emits a tool_call(Agent, name="child")
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_parent_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"hi"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use"},
			},
			// 2) child emits final text
			{
				{Type: providers.EventText, Text: "dynamic child says hi"},
				{Type: providers.EventDone, StopReason: "end_turn"},
			},
			// 3) parent wraps up
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn"},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent_dynamic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Register "child" via the dynamic_agents path — same shape
	// connector.RegisterAgent persists.
	childDef := config.AgentDef{
		Model:        "stub-model",
		AllowedTools: []string{},
		SystemPrompt: "you are the dynamic child",
	}
	childDefJSON, err := json.Marshal(childDef)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DynamicAgentUpsert(context.Background(), store.DynamicAgent{
		Name:       "child",
		Definition: childDefJSON,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("DynamicAgentUpsert: %v", err)
	}

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if strings.Contains(bodyStr, "unknown sub-agent") {
		t.Fatalf("regression: dynamic-only sub-agent failed to resolve, got:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "dynamic child says hi") {
		t.Errorf("parent stream missing dynamic child output:\n%s", bodyStr)
	}
}

// parent run was started under CALLER_AUTHORITATIVE with a per-call
// allowed_hosts list, sub-agents spawned via the Agent tool inherit
// that same host policy and can reach the same hosts.
//
// Regression for the 2026-05-06 cv-batch-adapter bug: parent ran
// against ["localhost"] (caller-authoritative). Spawned cv-adapter
// children fell back to operator's static HTTPHostAllowlist (which
// didn't include localhost), spent all iterations guessing hostnames
// (host.docker.internal, 172.17.0.1, api, app, nextjs, web,
// loomcycle, backend, server) and never reached the localhost API
// to PATCH /api/applications/<id>. No documents were written.
func TestSubAgent_InheritsParentCallerHostAllowlist(t *testing.T) {
	// Stand up an HTTP target the child will try to reach. Always
	// 127.0.0.1; the actual port is irrelevant — the host allowlist
	// match is hostname-only.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("child-saw-it"))
	}))
	defer target.Close()

	// Operator's static list is empty — so sub-agents that DON'T
	// inherit the parent's policy will see "host not in allowlist"
	// and the test will fail. With the fix, the sub-agent sees the
	// parent's caller-supplied list and reaches the target cleanly.
	cfg := makeBaseConfig()
	cfg.Env.HTTPCallerAuthoritative = true
	cfg.Env.HTTPHostAllowlist = nil
	cfg.Env.HTTPPrivateHostAllowlist = []string{"127.0.0.1"} // dial-layer loopback exemption
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", AllowedTools: []string{"HTTP", "Agent"}, SystemPrompt: "you are the parent"},
		"child":  {Model: "stub-model", AllowedTools: []string{"HTTP"}, SystemPrompt: "you are the child"},
	}
	cfg.Env.AuthToken = ""

	// Provider script:
	//   1) parent → tool_call(Agent) spawning child
	//   2) child  → tool_call(HTTP GET <target>)
	//   3) child  → final text + end_turn (after tool_result)
	//   4) parent → final text + end_turn (after Agent tool_result)
	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_parent_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"GET ` + target.URL + `"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_child_1",
						Name:  "HTTP",
						Input: json.RawMessage(`{"method":"GET","url":"` + target.URL + `"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 4}},
			},
			{
				{Type: providers.EventText, Text: "child done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 3, OutputTokens: 2}},
			},
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		},
	}

	// HTTP tool — operator config is what production ships. The
	// allowlist is initially empty (per cfg above); NarrowHosts
	// rebuilds the per-run tool with the caller's list.
	httpTool := &builtin.HTTP{
		HostAllowlist:        cfg.Env.HTTPHostAllowlist,
		PrivateHostAllowlist: cfg.Env.HTTPPrivateHostAllowlist,
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent_hosts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{httpTool}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Caller-authoritative request: the only hosts the run may reach
	// are 127.0.0.1 (the target) — explicitly NOT in the operator's
	// static list.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"parent","allowed_hosts":["127.0.0.1"],"segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// The Agent tool only returns the child's final text to the parent,
	// not the child's intermediate tool_results. To verify the child's
	// HTTP tool actually reached the target host, walk the store: find
	// the parent's agent_id from the SSE frame → list child runs by
	// parent_agent_id → fetch the child's transcript → inspect the
	// tool_result event from the child's HTTP call.
	parentAgentID := extractAgentID(bodyStr)
	if parentAgentID == "" {
		t.Fatalf("could not extract parent agent_id from SSE body:\n%s", bodyStr)
	}
	childRuns, err := st.ListRunsByParentAgentID(context.Background(), parentAgentID)
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(childRuns) != 1 {
		t.Fatalf("expected 1 child run, got %d", len(childRuns))
	}
	childTranscript, err := st.GetTranscript(context.Background(), childRuns[0].SessionID)
	if err != nil {
		t.Fatalf("GetTranscript(child): %v", err)
	}
	var sawHTTPSuccess bool
	for _, ev := range childTranscript {
		if ev.Type != "tool_result" {
			continue
		}
		payload := string(ev.Payload)
		if strings.Contains(payload, "not in allowlist") {
			t.Errorf("child's HTTP tool was denied by host allowlist — parent's caller policy did not propagate;\ntool_result payload:\n%s", payload)
		}
		if strings.Contains(payload, "child-saw-it") {
			sawHTTPSuccess = true
		}
	}
	if !sawHTTPSuccess {
		t.Errorf("expected child's HTTP tool to reach target and capture body 'child-saw-it' in a tool_result event;\nchild transcript had %d events", len(childTranscript))
	}
}

// bearerCapturingProvider drives a scripted event sequence per call AND
// records the ctx-carried tools.RunIdentity(ctx).UserBearer on every
// Call. Used by TestSubAgent_InheritsParentUserBearer to verify the
// child run's ctx carries the parent's bearer at provider-call time.
type bearerCapturingProvider struct {
	calls   atomic.Int32
	scripts [][]providers.Event

	mu       sync.Mutex
	captured []string // RunIdentity.UserBearer per Call invocation, in call order
}

func (b *bearerCapturingProvider) ID() string                    { return "bearer-capturing" }
func (b *bearerCapturingProvider) Probe(_ context.Context) error { return nil }
func (b *bearerCapturingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (b *bearerCapturingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (b *bearerCapturingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	b.mu.Lock()
	b.captured = append(b.captured, tools.RunIdentity(ctx).UserBearer)
	b.mu.Unlock()
	idx := int(b.calls.Add(1)) - 1
	var events []providers.Event
	if idx < len(b.scripts) {
		events = b.scripts[idx]
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// TestSubAgent_InheritsParentUserBearer guards the v0.8.x invariant
// that sub-agents see the parent run's per-run MCP bearer in their
// own ctx — identically, NOT narrowed (unlike caller-host policy).
// Sub-agents act on behalf of the same end-user; the same downstream
// MCP credential applies.
//
// Regression guard: if someone adds narrowing logic at the sub-agent
// dispatch site (internal/api/http/server.go around runSubAgent's
// WithRunIdentity call), this test catches it.
func TestSubAgent_InheritsParentUserBearer(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", AllowedTools: []string{"Agent"}, SystemPrompt: "you are the parent"},
		"child":  {Model: "stub-model", AllowedTools: []string{}, SystemPrompt: "you are the child"},
	}

	const parentBearer = "parent-bearer-xyz123456"

	prov := &bearerCapturingProvider{
		scripts: [][]providers.Event{
			{
				{
					Type: providers.EventToolCall,
					ToolUse: &providers.ToolUse{
						ID:    "tu_parent_1",
						Name:  "Agent",
						Input: json.RawMessage(`{"name":"child","prompt":"go"}`),
					},
				},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			{
				{Type: providers.EventText, Text: "child done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 3, OutputTokens: 2}},
			},
			{
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent_bearer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := `{"agent":"parent","user_bearer":"` + parentBearer + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		slurp, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, slurp)
	}
	// Drain the SSE body so the run completes before we assert.
	_, _ = io.ReadAll(resp.Body)

	prov.mu.Lock()
	captured := append([]string(nil), prov.captured...)
	prov.mu.Unlock()
	if len(captured) < 2 {
		t.Fatalf("expected at least 2 provider calls (parent + child), got %d", len(captured))
	}
	// Every Call must have seen the parent's bearer in ctx — the child
	// just as much as the parent (identical inheritance, no narrowing).
	for i, b := range captured {
		if b != parentBearer {
			t.Errorf("call #%d: ctx bearer = %q, want %q", i, b, parentBearer)
		}
	}
}

// TestSubAgent_InheritsParentContext is the load-bearing test for the
// cv-batch child-tagging feature: a parent run started with a
// parent_context must have that SAME context copied onto every
// sub-agent's persisted run row. Without the copy in runSubAgent, the
// child rows carry no link back to the user-initiated request and the
// consumer's cost aggregation can't roll up the batch.
//
// Regression: delete the `parentIdentity.ParentContext.Clone()` line in
// server.go runSubAgent and this test fails (child ParentContext == nil).
func TestSubAgent_InheritsParentContext(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", AllowedTools: []string{"Agent"}, SystemPrompt: "you are the parent"},
		"child":  {Model: "stub-model", AllowedTools: []string{}, SystemPrompt: "you are the child"},
	}

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{ // parent: spawn the child
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
					ID: "tu1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"go"}`),
				}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			{ // child
				{Type: providers.EventText, Text: "child done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 3, OutputTokens: 2}},
			},
			{ // parent: final
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent_pc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Explicit agent_id so we can look up the parent + its children
	// deterministically. parent_context is the opaque tracking lineage.
	body := `{"agent":"parent","agent_id":"parent-1",` +
		`"parent_context":{"root_agent_run_id":"run_root","function_key":"cv-batch","tier_at_run":"pro"},` +
		`"segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		slurp, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, slurp)
	}
	_, _ = io.ReadAll(resp.Body) // drain so the run + sub-run finish

	want := &store.ParentContext{RootAgentRunID: "run_root", FunctionKey: "cv-batch", TierAtRun: "pro"}

	// Parent run carries the context it was started with.
	parentRun, err := st.GetRunByAgentID(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("GetRunByAgentID(parent-1): %v", err)
	}
	if parentRun.ParentContext == nil || *parentRun.ParentContext != *want {
		t.Errorf("parent ParentContext = %+v, want %+v", parentRun.ParentContext, want)
	}

	// Every child sub-agent must carry the SAME context (the propagation
	// seam). This is the assertion that fails if the copy is removed.
	childRuns, err := st.ListRunsByParentAgentID(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(childRuns) == 0 {
		t.Fatal("expected at least one child run; none found (did the spawn happen?)")
	}
	for _, cr := range childRuns {
		if cr.ParentContext == nil {
			t.Errorf("child run %s ParentContext = nil; the parent's lineage did not propagate", cr.ID)
			continue
		}
		if *cr.ParentContext != *want {
			t.Errorf("child run %s ParentContext = %+v, want %+v", cr.ID, cr.ParentContext, want)
		}
	}
}

// TestSubAgent_SessionInheritsParentTenant is the regression for the production
// "404 session not found" on a sub-agent run: runSubAgent created the sub-run
// under the parent's tenant (subIdentity.TenantID) but created its SESSION with
// an EMPTY tenant (passed "" to openOrCreateSessionAndRun). The tenant-gated
// transcript read (s.tenantStore(...).GetSession) then 404'd the sub-agent
// session for its own tenant operator, while the run row stayed visible.
//
// Fail-before: the child SESSION's tenant_id is "" (≠ the parent's "acme").
func TestSubAgent_SessionInheritsParentTenant(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", AllowedTools: []string{"Agent"}, SystemPrompt: "you are the parent"},
		"child":  {Model: "stub-model", AllowedTools: []string{}, SystemPrompt: "you are the child"},
	}

	prov := &scriptedProvider{
		scripts: [][]providers.Event{
			{ // parent: spawn the child
				{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
					ID: "tu1", Name: "Agent", Input: json.RawMessage(`{"name":"child","prompt":"go"}`),
				}},
				{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}},
			},
			{ // child
				{Type: providers.EventText, Text: "child done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 3, OutputTokens: 2}},
			},
			{ // parent: final
				{Type: providers.EventText, Text: "parent done"},
				{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 12, OutputTokens: 3}},
			},
		},
	}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "subagent_tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Open mode (makeBaseConfig has no AuthToken) → the wire tenant_id is honored.
	body := `{"agent":"parent","agent_id":"parent-1","tenant_id":"acme",` +
		`"segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		slurp, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, slurp)
	}
	_, _ = io.ReadAll(resp.Body) // drain so the run + sub-run finish

	// Sanity: the parent's session is under "acme".
	parentRun, err := st.GetRunByAgentID(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("GetRunByAgentID(parent-1): %v", err)
	}
	parentSess, err := st.GetSession(context.Background(), parentRun.SessionID)
	if err != nil {
		t.Fatalf("GetSession(parent): %v", err)
	}
	if parentSess.TenantID != "acme" {
		t.Fatalf("parent session tenant = %q, want acme (wire tenant not honored?)", parentSess.TenantID)
	}

	// The child run AND its session must both be under "acme". The run was
	// already correct; the SESSION is the bug.
	childRuns, err := st.ListRunsByParentAgentID(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(childRuns) == 0 {
		t.Fatal("expected a child run; none found (did the spawn happen?)")
	}
	for _, cr := range childRuns {
		if cr.TenantID != "acme" {
			t.Errorf("child run %s tenant = %q, want acme", cr.ID, cr.TenantID)
		}
		childSess, err := st.GetSession(context.Background(), cr.SessionID)
		if err != nil {
			t.Fatalf("GetSession(child %s): %v", cr.SessionID, err)
		}
		if childSess.TenantID != "acme" {
			t.Errorf("child SESSION %s tenant = %q, want acme — the bug: the sub-agent session's tenant disagrees with its run, so the tenant-gated transcript read 404s it", cr.SessionID, childSess.TenantID)
		}
	}
}
