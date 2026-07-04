package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// mockConnector implements every Connector method as a no-op except
// the ones explicitly exercised. Lets tests focus on what they care
// about without faking unrelated surfaces.
type mockConnector struct {
	spawnReq    atomic.Value // connector.SpawnRunRequest (last)
	spawnResult connector.SpawnRunResult
	spawnErr    error
	// spawnGate, when non-nil, makes SpawnRun block until the channel is
	// closed (or ctx is cancelled) — lets a test hold a spawn_run "in
	// flight" to exercise concurrent dispatch. spawnActive/spawnMaxSeen
	// track in-flight + high-water concurrency for the cap test.
	spawnGate    chan struct{}
	spawnActive  atomic.Int32
	spawnMaxSeen atomic.Int32
	regCalls     atomic.Int32
	regResult    connector.AgentDescriptor
	chanDefCalls atomic.Int32 // CreateChannel + UpdateChannel + DeleteChannel + PurgeChannel
	pauseResult  connector.PauseResult
	listCallback func()

	// Hook-management injection points.
	registerHookID      string                        // return value for RegisterHook (default "hook_test")
	registerHookErr     error                         // overrides ID when set
	lastRegisterHookReq connector.RegisterHookRequest // captures the most recent call
	listHookHooks       []*hooks.Hook                 // return slice for ListHooks
	deleteHookErr       error                         // return value for DeleteHook
	lastDeleteHookID    string                        // id passed to the most recent DeleteHook call

	// v0.9.x n8n RFC Phase 0 injection points.
	listChannelsResp connector.ListChannelsResponse
	listChannelsErr  error
	streamEvents     []connector.RunStateEvent
	streamErr        error
	lastStreamReq    connector.StreamUserRunStatesRequest

	// RFC Y fan-out (spawn_runs) injection points.
	batchReq    atomic.Value // connector.BatchSpawnRequest (last)
	batchResult connector.BatchSpawnResult
	batchErr    error

	// compact_run (compaction MCP tool) injection points.
	getRunResult  connector.Run // returned by GetRun (agent_id→run_id resolution)
	compactRunID  atomic.Value  // string: the run_id CompactRun was called with
	compactResult connector.CompactResult
	compactErr    error
}

func (m *mockConnector) SpawnRun(ctx context.Context, r connector.SpawnRunRequest) (connector.SpawnRunResult, error) {
	m.spawnReq.Store(r)
	if m.spawnGate != nil {
		n := m.spawnActive.Add(1)
		for { // record the high-water mark of concurrent in-flight calls
			old := m.spawnMaxSeen.Load()
			if n <= old || m.spawnMaxSeen.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-m.spawnGate:
		case <-ctx.Done():
		}
		m.spawnActive.Add(-1)
	}
	return m.spawnResult, m.spawnErr
}
func (m *mockConnector) SpawnRunBatch(_ context.Context, r connector.BatchSpawnRequest) (connector.BatchSpawnResult, error) {
	m.batchReq.Store(r)
	return m.batchResult, m.batchErr
}
func (m *mockConnector) CancelRun(_ context.Context, _, _ string) (connector.CancelRunResult, error) {
	return connector.CancelRunResult{Cancelled: true}, nil
}
func (m *mockConnector) GetRun(_ context.Context, _ string) (connector.Run, error) {
	return m.getRunResult, nil
}
func (m *mockConnector) CompactRun(_ context.Context, runID string) (connector.CompactResult, error) {
	m.compactRunID.Store(runID)
	return m.compactResult, m.compactErr
}
func (m *mockConnector) ListRuns(_ context.Context, _ connector.ListRunsFilter) ([]connector.Run, error) {
	if m.listCallback != nil {
		m.listCallback()
	}
	return nil, nil
}
func (m *mockConnector) RegisterAgent(_ context.Context, _ connector.RegisterAgentRequest) (connector.AgentDescriptor, error) {
	m.regCalls.Add(1)
	return m.regResult, nil
}
func (m *mockConnector) UnregisterAgent(_ context.Context, _ string) error { return nil }
func (m *mockConnector) ListAgents(_ context.Context, _ bool) ([]connector.AgentDescriptor, error) {
	return nil, nil
}
func (m *mockConnector) Memory(_ context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{Text: `{"ok":true}`}, nil
}
func (m *mockConnector) Channel(_ context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) AgentDef(_ context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) SkillDef(_ context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) Evaluation(_ context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) Context(_ context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) PauseRuntime(_ context.Context, _ int) (connector.PauseResult, error) {
	return m.pauseResult, nil
}
func (m *mockConnector) ResumeRuntime(_ context.Context) (connector.ResumeResult, error) {
	return connector.ResumeResult{}, nil
}
func (m *mockConnector) GetRuntimeState(_ context.Context) (connector.RuntimeState, error) {
	return connector.RuntimeState{}, nil
}
func (m *mockConnector) ResolveProbe(_ context.Context) (connector.ResolverMatrix, error) {
	return connector.ResolverMatrix{
		Providers: map[string]connector.ResolverProviderAvailability{
			"mock": {Reachable: true, Models: map[string]connector.ResolverModelStatus{"mock-generic": {Listed: true}}},
		},
	}, nil
}
func (m *mockConnector) CreateSnapshot(_ context.Context, _ connector.CreateSnapshotRequest) (connector.SnapshotDescriptor, error) {
	return connector.SnapshotDescriptor{}, nil
}
func (m *mockConnector) ListSnapshots(_ context.Context) ([]connector.SnapshotDescriptor, error) {
	return nil, nil
}
func (m *mockConnector) GetSnapshot(_ context.Context, _ string) (connector.SnapshotEnvelope, error) {
	return connector.SnapshotEnvelope{}, errors.New("not implemented")
}
func (m *mockConnector) ExportSnapshot(_ context.Context, _ string) (connector.ExportSnapshotResult, error) {
	return connector.ExportSnapshotResult{}, errors.New("not implemented")
}
func (m *mockConnector) RestoreSnapshot(_ context.Context, _ connector.RestoreSnapshotRequest) (connector.RestoreSnapshotResult, error) {
	return connector.RestoreSnapshotResult{}, errors.New("not implemented")
}
func (m *mockConnector) DeleteSnapshot(_ context.Context, _ string) error {
	return errors.New("not implemented")
}

// Hook-management methods. The hook fields on mockConnector let the
// MCP handler tests below inject canned responses (registerHookID +
// registerHookErr drive register_hook; deleteHookErr drives
// delete_hook; listHookHooks drives list_hooks).
func (m *mockConnector) RegisterHook(_ context.Context, req connector.RegisterHookRequest) (connector.RegisterHookResponse, error) {
	m.lastRegisterHookReq = req
	if m.registerHookErr != nil {
		return connector.RegisterHookResponse{}, m.registerHookErr
	}
	id := m.registerHookID
	if id == "" {
		id = "hook_test"
	}
	return connector.RegisterHookResponse{ID: id}, nil
}
func (m *mockConnector) ListHooks(_ context.Context) (connector.ListHooksResponse, error) {
	return connector.ListHooksResponse{Hooks: m.listHookHooks}, nil
}
func (m *mockConnector) DeleteHook(_ context.Context, id string) error {
	m.lastDeleteHookID = id
	return m.deleteHookErr
}

func (m *mockConnector) InterruptionResolve(_ context.Context, _ connector.InterruptionResolveRequest) (connector.InterruptionResolveResult, error) {
	return connector.InterruptionResolveResult{}, errors.New("not implemented")
}

// v0.9.x n8n RFC Phase 0 — overridable surface.
func (m *mockConnector) ListChannels(context.Context) (connector.ListChannelsResponse, error) {
	if m.listChannelsResp.Channels != nil {
		return m.listChannelsResp, m.listChannelsErr
	}
	return connector.ListChannelsResponse{}, m.listChannelsErr
}
func (m *mockConnector) StreamUserRunStates(_ context.Context, req connector.StreamUserRunStatesRequest, visit connector.RunStateVisitor) error {
	m.lastStreamReq = req
	for _, evt := range m.streamEvents {
		if err := visit(evt); err != nil {
			if errors.Is(err, connector.ErrStopStreaming) {
				return nil
			}
			return err
		}
	}
	return m.streamErr
}

// RFC AI interactive-session stubs.
func (m *mockConnector) SteerRun(context.Context, string, string, string) (bool, error) {
	return false, connector.ErrRunNotInFlight
}
func (m *mockConnector) StreamRunEvents(context.Context, string, int64, connector.RunEventVisitor) error {
	return connector.ErrRunNotInFlight
}

// v0.9.x MCPServerDef stub.
func (m *mockConnector) MCPServerDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}

// v1.x ScheduleDef stub.
func (m *mockConnector) ScheduleDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}

// v1.x RFC G A2A substrate stubs.
func (m *mockConnector) A2AServerCardDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}

func (m *mockConnector) A2AAgentDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}

// v1.x RFC H Input Webhooks substrate stub.
func (m *mockConnector) WebhookDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}

func (m *mockConnector) MemoryBackendDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) OperatorTokenDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) VolumeDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) CredentialDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) Path(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) Document(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}

// v0.9.x Channel CRUD stubs.
func (m *mockConnector) PublishChannel(context.Context, connector.ChannelPublishRequest) (connector.ChannelPublishResult, error) {
	return connector.ChannelPublishResult{}, nil
}
func (m *mockConnector) SubscribeChannel(context.Context, connector.ChannelSubscribeRequest) (connector.ChannelSubscribeResult, error) {
	return connector.ChannelSubscribeResult{}, nil
}
func (m *mockConnector) PeekChannel(context.Context, connector.ChannelPeekRequest) (connector.ChannelPeekResult, error) {
	return connector.ChannelPeekResult{}, nil
}
func (m *mockConnector) AckChannel(context.Context, connector.ChannelAckRequest) (connector.ChannelAckResult, error) {
	return connector.ChannelAckResult{}, nil
}
func (m *mockConnector) CreateChannel(context.Context, connector.ChannelCreateRequest) (connector.ChannelDescriptor, error) {
	m.chanDefCalls.Add(1)
	return connector.ChannelDescriptor{}, nil
}
func (m *mockConnector) UpdateChannel(context.Context, string, connector.ChannelUpdateRequest) (connector.ChannelDescriptor, error) {
	m.chanDefCalls.Add(1)
	return connector.ChannelDescriptor{}, nil
}
func (m *mockConnector) DeleteChannel(context.Context, string) error {
	m.chanDefCalls.Add(1)
	return nil
}
func (m *mockConnector) PurgeChannel(context.Context, string) (connector.ChannelPurgeResult, error) {
	m.chanDefCalls.Add(1)
	return connector.ChannelPurgeResult{}, nil
}
func (m *mockConnector) AwaitChannels(context.Context, connector.ChannelAwaitRequest) (connector.ChannelAwaitResult, error) {
	return connector.ChannelAwaitResult{}, nil
}
func (m *mockConnector) BroadcastChannels(context.Context, connector.ChannelBroadcastRequest) (connector.ChannelBroadcastResult, error) {
	return connector.ChannelBroadcastResult{}, nil
}

// driveServer runs the server against the given input lines and
// returns the response frames (one per request). Notifications are
// captured separately.
func driveServer(t *testing.T, srv *Server, input string) (responses []loommcp.Response, notifications []loommcp.Notification) {
	t.Helper()
	return driveServerCtx(t, srv, context.Background(), input)
}

// driveServerCtx is driveServer with a caller-supplied base ctx — used to drive
// the server as a specific authenticated principal (auth.WithPrincipal) so the
// tools/list filter + tools/call gate (RFC AG §3.3) can be exercised.
func driveServerCtx(t *testing.T, srv *Server, base context.Context, input string) (responses []loommcp.Response, notifications []loommcp.Notification) {
	t.Helper()
	stdout := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	if err := srv.Serve(ctx, strings.NewReader(input), stdout); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line == "" {
			continue
		}
		// Probe by presence of "id" — responses have one, notifications don't.
		var probe struct {
			ID *int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Logf("skip undecodable line: %s", line)
			continue
		}
		if probe.ID != nil {
			var r loommcp.Response
			if err := json.Unmarshal([]byte(line), &r); err == nil {
				responses = append(responses, r)
			}
		} else {
			var n loommcp.Notification
			if err := json.Unmarshal([]byte(line), &n); err == nil {
				notifications = append(notifications, n)
			}
		}
	}
	return
}

func TestServer_Handshake(t *testing.T) {
	srv := New(Config{
		Connector:     &mockConnector{},
		Logf:          func(string, ...any) {},
		ServerName:    "loomcycle-test",
		ServerVersion: "v0.8.15-test",
	})
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claude-code","version":"1.0"}}}` + "\n"
	resps, notifs := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if len(notifs) != 0 {
		t.Errorf("got %d unexpected notifications", len(notifs))
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resps[0].Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.ProtocolVersion != loommcp.ProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", result.ProtocolVersion, loommcp.ProtocolVersion)
	}
	if result.ServerInfo.Name != "loomcycle-test" {
		t.Errorf("serverInfo.name = %q, want %q", result.ServerInfo.Name, "loomcycle-test")
	}
}

func TestServer_ToolsList_ReturnsFullCatalogue(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var result loommcp.ToolsListResult
	if err := json.Unmarshal(resps[0].Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Tools) != 47 {
		t.Errorf("got %d tools, want 47 (+credentialdef on top of the path/document/volumedef/spawn_runs/compact_run list)", len(result.Tools))
	}
	names := map[string]bool{}
	for _, td := range result.Tools {
		names[td.Name] = true
	}
	if !names["credentialdef"] {
		t.Error("catalogue missing the credentialdef meta-tool")
	}
	// Spot-check across categories — through the v1.x additions.
	for _, want := range []string{"spawn_run", "spawn_runs", "compact_run", "register_agent", "memory", "agentdef", "skilldef", "mcpserverdef", "scheduledef", "a2aservercarddef", "a2aagentdef", "webhookdef", "memorybackenddef", "operatortokendef", "volumedef", "path", "document", "pause_runtime", "create_snapshot", "get_snapshot", "resolve_probe", "interruption_resolve", "register_hook", "list_hooks", "delete_hook", "list_channels", "stream_user_run_states", "publish_channel", "subscribe_channel", "peek_channel", "ack_channel"} {
		if !names[want] {
			t.Errorf("missing tool %q in tools/list", want)
		}
	}
}

// TestServer_ToolsList_FiltersAdminToolsForTenant drives tools/list as a
// non-admin (substrate:tenant) principal and asserts the admin-only meta-tools
// are absent while the tenant-confinable ones remain (RFC AG §3.3). Contrast
// with TestServer_ToolsList_ReturnsFullCatalogue (no principal → full set).
func TestServer_ToolsList_FiltersAdminToolsForTenant(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{"substrate:tenant"}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	resps, _ := driveServerCtx(t, srv, ctx, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var result loommcp.ToolsListResult
	if err := json.Unmarshal(resps[0].Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := map[string]bool{}
	for _, td := range result.Tools {
		names[td.Name] = true
	}
	for _, hidden := range []string{"operatortokendef", "pause_runtime", "restore_snapshot", "list_channels"} {
		if names[hidden] {
			t.Errorf("admin-only tool %q must be hidden from a tenant principal's tools/list", hidden)
		}
	}
	for _, shown := range []string{"document", "agentdef", "memory", "spawn_run", "path", "context", "register_hook", "list_hooks", "delete_hook"} {
		if !names[shown] {
			t.Errorf("tenant-confinable tool %q must remain in a tenant principal's tools/list", shown)
		}
	}
}

// TestServer_ToolsCall_GatesAdminToolForTenant is the RFC AG §3.3 / §6
// enforcement test: a non-admin principal that calls a (hidden) admin-only tool
// anyway gets a clean forbidden error — the gate, not just the hide. Fail-before:
// drop the principalMayCallTool check in handleToolsCall and the call dispatches
// (pause_runtime returns a tool result instead of mcpErrForbidden).
func TestServer_ToolsCall_GatesAdminToolForTenant(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{"substrate:tenant"}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pause_runtime","arguments":{}}}` + "\n"
	resps, _ := driveServerCtx(t, srv, ctx, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if resps[0].Error == nil {
		t.Fatalf("expected a JSON-RPC error for a gated tool, got result: %s", resps[0].Result)
	}
	if resps[0].Error.Code != mcpErrForbidden {
		t.Errorf("error code = %d, want mcpErrForbidden (%d)", resps[0].Error.Code, mcpErrForbidden)
	}
}

// TestServer_ToolsCall_AdminToolAllowedForNoPrincipal confirms the gate is inert
// on the stdio / no-principal path: pause_runtime dispatches (operator-trust),
// it is NOT a forbidden error. (It may surface a tool-level error from the mock
// connector, but never mcpErrForbidden.)
func TestServer_ToolsCall_AdminToolAllowedForNoPrincipal(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pause_runtime","arguments":{}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if resps[0].Error != nil && resps[0].Error.Code == mcpErrForbidden {
		t.Errorf("no-principal (stdio) path must not be gated; got forbidden error")
	}
}

// TestBuiltinWrapperSchemas_CoverAllWrappers asserts every op-dispatched
// builtin tool exposed as an MCP meta-tool advertises that tool's real
// discriminated-op schema — not the bare {"type":"object"} the wrappers
// shipped with, which hid the `op` enum + every property from clients
// introspecting the server. Iterating builtin.MCPWrapperNames() (rather
// than a hand-kept list) means a newly-exposed wrapper that forgets to
// source its schema via builtinSchema() fails here.
func TestBuiltinWrapperSchemas_CoverAllWrappers(t *testing.T) {
	byName := map[string]json.RawMessage{}
	for _, td := range toolDescriptors() {
		byName[td.Name] = td.InputSchema
	}
	for _, name := range builtin.MCPWrapperNames() {
		schema, ok := byName[name]
		if !ok {
			t.Errorf("builtin wrapper %q has a schema but no tools/list descriptor", name)
			continue
		}
		want, _ := builtin.MCPWrapperInputSchema(name)
		if !bytes.Equal(schema, want) {
			t.Errorf("wrapper %q: descriptor schema not sourced from builtin (bare fallback?)", name)
		}
		// The sourced schema must parse and expose a top-level `op` enum —
		// the whole point of the fix is that clients can discover ops.
		var parsed struct {
			Properties struct {
				Op struct {
					Enum []string `json:"enum"`
				} `json:"op"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Errorf("wrapper %q schema is not valid JSON: %v", name, err)
			continue
		}
		if len(parsed.Properties.Op.Enum) == 0 {
			t.Errorf("wrapper %q schema exposes no op enum — clients can't discover operations", name)
		}
	}
}

func TestServer_SpawnRun_BlockingPath(t *testing.T) {
	mc := &mockConnector{
		spawnResult: connector.SpawnRunResult{
			AgentID:   "a_x",
			RunID:     "r_x",
			SessionID: "s_x",
			Status:    "completed",
			FinalText: "hello world",
		},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	// No runEvents capability → blocking path via Connector.SpawnRun.
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa-agent","segments":[]}}}`,
	}, "\n") + "\n"
	resps, notifs := driveServer(t, srv, in)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (init + spawn_run)", len(resps))
	}
	if len(notifs) != 0 {
		t.Errorf("blocking path emitted %d notifications, want 0", len(notifs))
	}
	stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
	if stored.Agent != "qa-agent" {
		t.Errorf("Connector saw agent=%q, want %q", stored.Agent, "qa-agent")
	}
	// Inspect the spawn_run tool result payload.
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &callRes); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("expected non-error spawn_run result, got isError=true: %v", callRes.Content)
	}
	if len(callRes.Content) != 1 {
		t.Fatalf("got %d content blocks, want 1", len(callRes.Content))
	}
	var inner connector.SpawnRunResult
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if inner.AgentID != "a_x" || inner.FinalText != "hello world" {
		t.Errorf("inner = %+v, want AgentID=a_x FinalText=\"hello world\"", inner)
	}
}

// TestServer_SpawnRun_CarriesImageSegment verifies an image content block
// (RFC AT) in a spawn_run call relays through to the connector intact — the MCP
// path carries it as base64 JSON (like HTTP), so no transform is needed.
// Fail-before: this would already pass on the handler (segments relay through
// unchanged); the test pins the contract so a future schema/handler change that
// drops image fields is caught.
func TestServer_SpawnRun_CarriesImageSegment(t *testing.T) {
	const b64 = "iVBORw0KGgo="
	mc := &mockConnector{spawnResult: connector.SpawnRunResult{Status: "completed"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"vision","segments":[{"role":"user","content":[{"type":"trusted-text","text":"what is this"},{"type":"image","media_type":"image/png","data":"` + b64 + `"}]}]}}}`,
	}, "\n") + "\n"
	driveServer(t, srv, in)

	stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
	if len(stored.Segments) != 1 || len(stored.Segments[0].Content) != 2 {
		t.Fatalf("connector saw segments %+v, want 1 segment with 2 blocks", stored.Segments)
	}
	img := stored.Segments[0].Content[1]
	if img.Type != "image" || img.MediaType != "image/png" || img.Data != b64 {
		t.Errorf("image block not relayed intact: %+v", img)
	}
}

// TestServer_SpawnRun_CarriesCompaction verifies a per-run compaction block on
// a spawn_run call decodes into SpawnRunRequest.Compaction (the new per-run
// config surface; the connector then carries it to runner.RunInput).
// TestServer_SpawnRun_AppliesPrincipalOverWireIdentity is the RFC AG §3.2
// enforcement: a non-legacy principal is authoritative over the wire
// tenant_id/user_id, so a forged tenant_id in the spawn_run body cannot place
// the run in another tenant. Fail-before: drop applyPrincipalToSpawn in
// handleSpawnRun and the connector sees the forged "evil"/"mallory".
func TestServer_SpawnRun_AppliesPrincipalOverWireIdentity(t *testing.T) {
	mc := &mockConnector{spawnResult: connector.SpawnRunResult{Status: "completed"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice"}) // real (non-legacy) principal
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa","segments":[],"tenant_id":"evil","user_id":"mallory"}}}` + "\n"
	driveServerCtx(t, srv, ctx, in)
	stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
	if stored.TenantID != "acme" || stored.UserID != "alice" {
		t.Errorf("connector saw (tenant=%q,user=%q), want (acme,alice) — wire claims must be overridden", stored.TenantID, stored.UserID)
	}
}

// TestServer_SpawnRun_LegacyHonorsWireUserID: under the legacy token the wire
// user_id is honored (single-operator, no-boundary; F18) but the tenant stays
// the legacy default. Mirrors HTTP's applyPrincipal via the shared rule.
func TestServer_SpawnRun_LegacyHonorsWireUserID(t *testing.T) {
	mc := &mockConnector{spawnResult: connector.SpawnRunResult{Status: "completed"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa","segments":[],"tenant_id":"ignored","user_id":"exp1"}}}` + "\n"
	driveServerCtx(t, srv, ctx, in)
	stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
	if stored.TenantID != "default" || stored.UserID != "exp1" {
		t.Errorf("legacy spawn saw (tenant=%q,user=%q), want (default,exp1)", stored.TenantID, stored.UserID)
	}
}

// TestServer_SpawnRun_NoPrincipalPassesWireThrough: on the stdio / open path
// (no principal) the caller-supplied identity passes through unchanged.
func TestServer_SpawnRun_NoPrincipalPassesWireThrough(t *testing.T) {
	mc := &mockConnector{spawnResult: connector.SpawnRunResult{Status: "completed"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa","segments":[],"tenant_id":"wire-t","user_id":"wire-u"}}}` + "\n"
	driveServer(t, srv, in)
	stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
	if stored.TenantID != "wire-t" || stored.UserID != "wire-u" {
		t.Errorf("no-principal spawn saw (tenant=%q,user=%q), want the wire values", stored.TenantID, stored.UserID)
	}
}

// TestServer_SpawnRun_ContinuationSkipsOverride: a continuation (session_id set)
// must NOT have its identity rewritten — the session's stored identity is
// authoritative downstream, and the connector ignores these fields for a
// session_id call. So applyPrincipalToSpawn is a deliberate no-op here.
func TestServer_SpawnRun_ContinuationSkipsOverride(t *testing.T) {
	mc := &mockConnector{spawnResult: connector.SpawnRunResult{Status: "completed"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice"})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_run","arguments":{"session_id":"s_prior","segments":[],"tenant_id":"wire-t"}}}` + "\n"
	driveServerCtx(t, srv, ctx, in)
	stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
	if stored.TenantID != "wire-t" {
		t.Errorf("continuation tenant = %q, want it left untouched (wire-t) — override must skip continuations", stored.TenantID)
	}
}

// TestServer_SpawnRuns_AppliesPrincipalToEachChild: the RFC Y batch fan-out
// stamps the caller's authoritative identity on EVERY child, so a forged
// tenant_id in any child spec can't cross the tenant boundary (RFC AG §3.2).
func TestServer_SpawnRuns_AppliesPrincipalToEachChild(t *testing.T) {
	mc := &mockConnector{batchResult: connector.BatchSpawnResult{}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice"})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_runs","arguments":{"spawns":[{"agent":"a","segments":[],"tenant_id":"evil"},{"agent":"b","segments":[],"tenant_id":"evil2","user_id":"mallory"}]}}}` + "\n"
	driveServerCtx(t, srv, ctx, in)
	stored, _ := mc.batchReq.Load().(connector.BatchSpawnRequest)
	if len(stored.Spawns) != 2 {
		t.Fatalf("connector saw %d spawns, want 2", len(stored.Spawns))
	}
	for i, sp := range stored.Spawns {
		if sp.TenantID != "acme" || sp.UserID != "alice" {
			t.Errorf("child[%d] = (tenant=%q,user=%q), want (acme,alice)", i, sp.TenantID, sp.UserID)
		}
	}
}

func TestServer_SpawnRun_CarriesCompaction(t *testing.T) {
	mc := &mockConnector{spawnResult: connector.SpawnRunResult{AgentID: "a", RunID: "r", Status: "completed"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa","segments":[],"compaction":{"enabled":true,"keep_last_n":6,"autocompact_at_pct":75}}}}`,
	}, "\n") + "\n"
	driveServer(t, srv, in)
	stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
	if stored.Compaction == nil {
		t.Fatal("SpawnRunRequest.Compaction is nil — per-run compaction did not decode")
	}
	if stored.Compaction.Enabled == nil || !*stored.Compaction.Enabled {
		t.Error("compaction.enabled did not decode as true")
	}
	if stored.Compaction.KeepLastN == nil || *stored.Compaction.KeepLastN != 6 {
		t.Errorf("compaction.keep_last_n = %v, want 6", stored.Compaction.KeepLastN)
	}
	if stored.Compaction.AutoCompactAtPct == nil || *stored.Compaction.AutoCompactAtPct != 75 {
		t.Errorf("compaction.autocompact_at_pct = %v, want 75", stored.Compaction.AutoCompactAtPct)
	}
}

// TestServer_CompactRun_ResolvesAgentIDThenCompacts verifies the compact_run
// tool resolves agent_id→run_id via GetRun, calls CompactRun with that run_id,
// and returns the compaction envelope.
func TestServer_CompactRun_ResolvesAgentIDThenCompacts(t *testing.T) {
	mc := &mockConnector{
		getRunResult:  connector.Run{AgentID: "a_x", RunID: "r_x", Status: "awaiting_input"},
		compactResult: connector.CompactResult{RunID: "r_x", Compacted: true, BeforeTokens: 900, AfterTokens: 120, Applied: "live"},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"compact_run","arguments":{"agent_id":"a_x"}}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	if got, _ := mc.compactRunID.Load().(string); got != "r_x" {
		t.Errorf("CompactRun saw run_id=%q, want r_x (agent_id must resolve via GetRun)", got)
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &callRes); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("expected non-error compact_run result, got: %v", callRes.Content)
	}
	var inner connector.CompactResult
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if !inner.Compacted || inner.Applied != "live" || inner.AfterTokens != 120 {
		t.Errorf("inner = %+v, want compacted live with after_tokens=120", inner)
	}
}

// TestServer_CompactRun_RequiresAgentID verifies the agent_id guard short-
// circuits before any connector call.
func TestServer_CompactRun_RequiresAgentID(t *testing.T) {
	mc := &mockConnector{}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"compact_run","arguments":{}}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !callRes.IsError {
		t.Error("compact_run with no agent_id must be a tool error")
	}
	if mc.compactRunID.Load() != nil {
		t.Error("CompactRun must NOT be called when agent_id is missing")
	}
}

// TestServer_SpawnRuns_DispatchesBatch verifies the RFC Y spawn_runs tool
// parses the spawns array, hands it to Connector.SpawnRunBatch, and returns the
// combined envelope as the tool result.
func TestServer_SpawnRuns_DispatchesBatch(t *testing.T) {
	mc := &mockConnector{
		batchResult: connector.BatchSpawnResult{
			Spawned: 2,
			Results: []connector.SpawnRunResult{
				{AgentID: "a_0", RunID: "r_0", Status: "completed", FinalText: "zero"},
				{AgentID: "a_1", RunID: "r_1", Status: "completed", FinalText: "one"},
			},
		},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_runs","arguments":{"spawns":[{"agent":"rev"},{"agent":"rev"}]}}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (init + spawn_runs)", len(resps))
	}
	stored, _ := mc.batchReq.Load().(connector.BatchSpawnRequest)
	if len(stored.Spawns) != 2 || stored.Spawns[0].Agent != "rev" {
		t.Errorf("Connector saw spawns=%+v, want 2× agent=rev", stored.Spawns)
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &callRes); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("expected non-error spawn_runs result, got: %v", callRes.Content)
	}
	var inner connector.BatchSpawnResult
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if inner.Spawned != 2 || len(inner.Results) != 2 || inner.Results[1].RunID != "r_1" {
		t.Errorf("inner = %+v, want 2 results with r_1 at index 1", inner)
	}
}

// TestServer_SpawnRuns_RejectsChildMissingAgent verifies per-child validation:
// a spawn with no agent is a tool error, never reaches the connector.
func TestServer_SpawnRuns_RejectsChildMissingAgent(t *testing.T) {
	mc := &mockConnector{}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_runs","arguments":{"spawns":[{"agent":""}]}}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &callRes); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if !callRes.IsError {
		t.Error("spawn_runs with an empty agent must be a tool error")
	}
	if mc.batchReq.Load() != nil {
		t.Error("connector.SpawnRunBatch must NOT be called when a child fails validation")
	}
}

// TestServer_ResolveProbe_DispatchesThroughConnector verifies the
// resolve_probe meta-tool dispatches to Connector.ResolveProbe and
// returns the matrix as the tool result payload (issue #88).
func TestServer_ResolveProbe_DispatchesThroughConnector(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"resolve_probe","arguments":{}}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (init + resolve_probe)", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &callRes); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if callRes.IsError {
		t.Fatalf("expected non-error resolve_probe result, got isError=true: %v", callRes.Content)
	}
	var matrix connector.ResolverMatrix
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &matrix); err != nil {
		t.Fatalf("unmarshal matrix: %v", err)
	}
	mock, ok := matrix.Providers["mock"]
	if !ok || !mock.Reachable || !mock.Models["mock-generic"].Listed {
		t.Errorf("matrix did not carry the mock provider through: %+v", matrix.Providers)
	}
}

// fakeRunner implements runner.Runner: on RunOnce, fires
// OnRegistered then a scripted event sequence on OnEvent.
type fakeRunner struct {
	agentID, runID, sessionID string
	events                    []providers.Event
}

func (f *fakeRunner) RunOnce(_ context.Context, _ runner.RunInput, cb runner.RunCallbacks) error {
	if cb.OnRegistered != nil {
		cb.OnRegistered(f.agentID, f.runID, f.sessionID, "")
	}
	for _, ev := range f.events {
		if cb.OnEvent != nil {
			cb.OnEvent(ev)
		}
	}
	return nil
}

func TestServer_SpawnRun_StreamingPath(t *testing.T) {
	usage := &providers.Usage{InputTokens: 10, OutputTokens: 4, Model: "stub"}
	fr := &fakeRunner{
		agentID:   "a_s",
		runID:     "r_s",
		sessionID: "s_s",
		events: []providers.Event{
			{Type: providers.EventStarted},
			{Type: providers.EventText, Text: "hi "},
			{Type: providers.EventText, Text: "there"},
			{Type: providers.EventUsage, Usage: usage},
			{Type: providers.EventDone, StopReason: "end_turn"},
		},
	}
	mc := &mockConnector{}
	srv := New(Config{Connector: mc, Runner: fr, Logf: func(string, ...any) {}})

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{"loomcycle":{"runEvents":true}},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa-agent","segments":[]}}}`,
	}, "\n") + "\n"
	resps, notifs := driveServer(t, srv, in)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2", len(resps))
	}
	// 5 events → 5 notifications.
	if len(notifs) != 5 {
		t.Errorf("got %d notifications, want 5 (one per fake event)", len(notifs))
	}
	for _, n := range notifs {
		if n.Method != "notifications/loomcycle/run_event" {
			t.Errorf("notification method = %q, want %q", n.Method, "notifications/loomcycle/run_event")
		}
	}
	// Final result should have FinalText accumulated from the two
	// text events and the usage attached.
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &callRes); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	var inner connector.SpawnRunResult
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if inner.FinalText != "hi there" {
		t.Errorf("FinalText = %q, want %q", inner.FinalText, "hi there")
	}
	if inner.AgentID != "a_s" {
		t.Errorf("AgentID = %q, want %q", inner.AgentID, "a_s")
	}
	if inner.Status != "completed" {
		t.Errorf("Status = %q, want %q", inner.Status, "completed")
	}
}

// TestServer_SpawnRun_NotificationsArriveBeforeResponse pins the
// wire-ordering invariant: every notifications/loomcycle/run_event
// for a streaming spawn_run lands on stdout BEFORE the final
// tools/call response. Without this guarantee, MCP orchestrators
// rendering live agent output would see the run complete and only
// then receive the event stream — useless for live progress UIs.
//
// Distinct from TestServer_SpawnRun_StreamingPath above (which only
// counts notifications). This test re-runs the same fixture but
// captures stdout in order to assert positional invariants.
func TestServer_SpawnRun_NotificationsArriveBeforeResponse(t *testing.T) {
	fr := &fakeRunner{
		agentID:   "a_o",
		runID:     "r_o",
		sessionID: "s_o",
		events: []providers.Event{
			{Type: providers.EventStarted},
			{Type: providers.EventText, Text: "hello"},
			{Type: providers.EventDone, StopReason: "end_turn"},
		},
	}
	mc := &mockConnector{}
	srv := New(Config{Connector: mc, Runner: fr, Logf: func(string, ...any) {}})

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{"loomcycle":{"runEvents":true}},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa-agent","segments":[]}}}`,
	}, "\n") + "\n"

	stdout := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Serve(ctx, strings.NewReader(in), stdout); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// Walk stdout lines in order. Track:
	//   - index of the spawn_run response (id=2)
	//   - indexes of all run_event notifications
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	var spawnRespIdx int = -1
	var notifIdxs []int
	for i, line := range lines {
		var probe struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("undecodable line %d: %s", i, line)
		}
		if probe.ID != nil && *probe.ID == 2 {
			spawnRespIdx = i
		}
		if probe.ID == nil && probe.Method == "notifications/loomcycle/run_event" {
			notifIdxs = append(notifIdxs, i)
		}
	}
	if spawnRespIdx < 0 {
		t.Fatalf("spawn_run response (id=2) not found in stdout:\n%s", stdout.String())
	}
	if len(notifIdxs) == 0 {
		t.Fatal("no run_event notifications observed")
	}
	for _, ni := range notifIdxs {
		if ni >= spawnRespIdx {
			t.Errorf("notification at line %d came at-or-after spawn response at line %d; ordering invariant broken",
				ni, spawnRespIdx)
		}
	}
}

func TestServer_RegisterAgent_DispatchesThroughConnector(t *testing.T) {
	mc := &mockConnector{
		regResult: connector.AgentDescriptor{Name: "x", Source: "dynamic"},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"register_agent","arguments":{"name":"x","system_prompt":"p","tools":["Memory"]}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if mc.regCalls.Load() != 1 {
		t.Errorf("Connector.RegisterAgent called %d times, want 1", mc.regCalls.Load())
	}
}

// F20: the channeldef meta-tool dispatches create/update/delete to the
// Connector's channel-admin methods (the MCP twin of REST /v1/_channels).
func TestServer_ChannelDef_DispatchesThroughConnector(t *testing.T) {
	for _, tc := range []struct {
		op   string
		args string
	}{
		{"create", `{"op":"create","name":"runtime-ch","scope":"global","semantic":"queue"}`},
		{"update", `{"op":"update","name":"runtime-ch","max_messages":100}`},
		{"delete", `{"op":"delete","name":"runtime-ch"}`},
		{"purge", `{"op":"purge","name":"runtime-ch"}`},
	} {
		t.Run(tc.op, func(t *testing.T) {
			mc := &mockConnector{}
			srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
			in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"channeldef","arguments":` + tc.args + `}}` + "\n"
			resps, _ := driveServer(t, srv, in)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			if resps[0].Error != nil {
				t.Fatalf("unexpected JSON-RPC error: %+v", resps[0].Error)
			}
			if mc.chanDefCalls.Load() != 1 {
				t.Errorf("%s: Connector channel-admin called %d times, want 1", tc.op, mc.chanDefCalls.Load())
			}
		})
	}
}

// A channeldef op with no name is refused before reaching the Connector.
func TestServer_ChannelDef_RequiresName(t *testing.T) {
	mc := &mockConnector{}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"channeldef","arguments":{"op":"create"}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if mc.chanDefCalls.Load() != 0 {
		t.Errorf("Connector called %d times on a name-less request, want 0", mc.chanDefCalls.Load())
	}
}

func TestServer_UnknownTool_Returns32601(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nonexistent","arguments":{}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if resps[0].Error == nil {
		t.Fatal("expected error response")
	}
	if resps[0].Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resps[0].Error.Code)
	}
}

// TestServer_PauseRuntime_DispatchesToConnector — v0.8.18: pause_runtime
// returns the real Connector result. mockConnector.pauseResult is
// what the test plumbs through; the wire shape carries Status,
// DurationMs, ForceCancelledCount, PausedRunsCount — no FeatureStatus
// in the real path.
func TestServer_PauseRuntime_DispatchesToConnector(t *testing.T) {
	mc := &mockConnector{
		pauseResult: connector.PauseResult{
			Status:              "paused",
			DurationMS:          12,
			ForceCancelledCount: 1,
			PausedRunsCount:     2,
		},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pause_runtime","arguments":{"timeout_ms":5000}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if callRes.IsError {
		t.Errorf("pause_runtime should NOT be a tool error on the happy path")
	}
	var inner connector.PauseResult
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if inner.Status != "paused" {
		t.Errorf("status = %q, want paused", inner.Status)
	}
	if inner.PausedRunsCount != 2 {
		t.Errorf("paused_runs_count = %d, want 2", inner.PausedRunsCount)
	}
	if inner.FeatureStatus != "" {
		t.Errorf("feature_status = %q, want empty (v0.8.18 real impls drop the marker)", inner.FeatureStatus)
	}
}

// TestServer_SequentialDispatch_AllResponsesPresent pins the property
// that 5 back-to-back requests on a single stdio connection each
// receive a response with the correct id. Frame dispatch is sequential
// (see Server.Serve doc); this test doesn't exercise concurrent goroutine
// safety — that's an explicit v0.9.x non-goal. If the dispatch loop is
// ever made concurrent (per-request goroutines), this test would still
// pass but the writeMu would become load-bearing; in that future,
// extend the test to assert no torn frames on stdout.
func TestServer_SequentialDispatch_AllResponsesPresent(t *testing.T) {
	var listCalls atomic.Int32
	mc := &mockConnector{}
	mc.listCallback = func() {
		listCalls.Add(1)
		time.Sleep(5 * time.Millisecond)
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})

	var sb strings.Builder
	for i := 1; i <= 5; i++ {
		sb.WriteString(`{"jsonrpc":"2.0","id":`)
		sb.WriteString(string(rune('0' + i)))
		sb.WriteString(`,"method":"tools/call","params":{"name":"list_runs","arguments":{"user_id":"u_a"}}}` + "\n")
	}
	resps, _ := driveServer(t, srv, sb.String())
	if len(resps) != 5 {
		t.Fatalf("got %d responses, want 5", len(resps))
	}
	seen := map[int64]bool{}
	for _, r := range resps {
		seen[r.ID] = true
	}
	for i := int64(1); i <= 5; i++ {
		if !seen[i] {
			t.Errorf("missing response id=%d", i)
		}
	}
	if listCalls.Load() != 5 {
		t.Errorf("Connector.ListRuns called %d times, want 5", listCalls.Load())
	}
}

// ---- Hook management (PR B of the hooks-connector series) ----

func TestServer_RegisterHook_DispatchesAndReturnsID(t *testing.T) {
	mc := &mockConnector{registerHookID: "hook_abc"}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"register_hook","arguments":{"owner":"jobs-search-web","name":"scan","phase":"pre","tools":["WebFetch"],"callback_url":"https://callback.local/h","fail_mode":"open","timeout_ms":3000}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if callRes.IsError {
		t.Errorf("register_hook should not be a tool error on happy path")
	}
	var inner connector.RegisterHookResponse
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if inner.ID != "hook_abc" {
		t.Errorf("id = %q, want hook_abc", inner.ID)
	}
	// Verify the connector received the full body shape.
	if mc.lastRegisterHookReq.Owner != "jobs-search-web" ||
		mc.lastRegisterHookReq.CallbackURL != "https://callback.local/h" ||
		mc.lastRegisterHookReq.Phase != "pre" ||
		mc.lastRegisterHookReq.TimeoutMs != 3000 {
		t.Errorf("connector saw %+v", mc.lastRegisterHookReq)
	}
}

func TestServer_RegisterHook_InvalidArguments_ToolError(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	// malformed JSON inside `arguments`
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"register_hook","arguments":"not-an-object"}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !callRes.IsError {
		t.Errorf("expected IsError=true on malformed args")
	}
}

func TestServer_DeleteHook_SurfacesConnectorError(t *testing.T) {
	mc := &mockConnector{deleteHookErr: connector.ErrHookNotFound}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_hook","arguments":{"id":"hook_gone"}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !callRes.IsError {
		t.Error("expected IsError=true on ErrHookNotFound")
	}
	if mc.lastDeleteHookID != "hook_gone" {
		t.Errorf("connector saw id %q, want hook_gone", mc.lastDeleteHookID)
	}
}

func TestServer_ListHooks_ReturnsConnectorList(t *testing.T) {
	mc := &mockConnector{listHookHooks: []*hooks.Hook{{ID: "h_1", Owner: "a"}, {ID: "h_2", Owner: "b"}}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_hooks","arguments":{}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var inner connector.ListHooksResponse
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if len(inner.Hooks) != 2 || inner.Hooks[0].ID != "h_1" {
		t.Errorf("inner = %+v", inner)
	}
}

// v0.9.x n8n RFC Phase 0 meta-tools.

func TestServer_ListChannels_DispatchesToConnector(t *testing.T) {
	mc := &mockConnector{
		listChannelsResp: connector.ListChannelsResponse{
			Channels: []connector.ChannelDescriptor{
				{Name: "alpha", MessageCount: 5},
				{Name: "beta", MessageCount: 0},
			},
		},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_channels","arguments":{}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var inner connector.ListChannelsResponse
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if len(inner.Channels) != 2 || inner.Channels[0].Name != "alpha" {
		t.Errorf("inner = %+v", inner)
	}
}

func TestServer_StreamUserRunStates_BlockingPath_CollectsEvents(t *testing.T) {
	mc := &mockConnector{
		streamEvents: []connector.RunStateEvent{
			{RunID: "r1", UserID: "user-a", Status: "running"},
			{RunID: "r2", UserID: "user-a", Status: "completed"},
		},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stream_user_run_states","arguments":{"user_id":"user-a","max_events":2,"timeout_ms":500}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var inner struct {
		Events []connector.RunStateEvent `json:"events"`
		Count  int                       `json:"count"`
	}
	if err := json.Unmarshal([]byte(callRes.Content[0].Text), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	if inner.Count != 2 || len(inner.Events) != 2 || inner.Events[0].RunID != "r1" {
		t.Errorf("inner = %+v", inner)
	}
	if mc.lastStreamReq.UserID != "user-a" {
		t.Errorf("connector got wrong req: %+v", mc.lastStreamReq)
	}
}

func TestServer_StreamUserRunStates_StreamingPath_EmitsNotifications(t *testing.T) {
	mc := &mockConnector{
		streamEvents: []connector.RunStateEvent{
			{RunID: "r1", UserID: "user-a", Status: "completed"},
			{RunID: "r2", UserID: "user-a", Status: "completed"},
		},
	}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	// First initialize with runEvents=true so the streaming path engages.
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{"loomcycle":{"runEvents":true}},"clientInfo":{"name":"t","version":"v"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"stream_user_run_states","arguments":{"user_id":"user-a","max_events":2,"timeout_ms":500}}}` + "\n"
	resps, notifs := driveServer(t, srv, in)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (initialize + tools/call)", len(resps))
	}
	// Expect two notifications (one per event), each with method run_state.
	got := 0
	for _, n := range notifs {
		if n.Method == "notifications/loomcycle/run_state" {
			got++
		}
	}
	if got != 2 {
		t.Errorf("got %d run_state notifications, want 2 (all %d notifs: %+v)", got, len(notifs), notifs)
	}
}

func TestServer_StreamUserRunStates_RequiresUserID(t *testing.T) {
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stream_user_run_states","arguments":{}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses", len(resps))
	}
	var callRes loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &callRes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !callRes.IsError {
		t.Error("expected IsError=true on missing user_id")
	}
}

// helper to satisfy io.Reader vs strings.NewReader type contract in
// case Go's stdlib evolves; kept as a guard.
var _ io.Reader = strings.NewReader("")
var _ sync.Locker = (*sync.Mutex)(nil)

// --- RFC O: concurrent stdio dispatch ---------------------------------
//
// liveServer drives a Server over real pipes so a test can write frames
// over time and read responses as they arrive (driveServer is batch —
// it can't observe the non-blocking property). Responses/notifications
// are demuxed onto channels; waitResp blocks for a specific JSON-RPC id.

type liveServer struct {
	t      *testing.T
	stdinW *io.PipeWriter
	cancel context.CancelFunc
	done   chan error

	mu    sync.Mutex
	got   map[int64]loommcp.Response // id → response, as they arrive
	notes []loommcp.Notification
	cond  *sync.Cond
}

func newLiveServer(t *testing.T, srv *Server) *liveServer {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	ls := &liveServer{t: t, stdinW: stdinW, cancel: cancel, done: make(chan error, 1), got: map[int64]loommcp.Response{}}
	ls.cond = sync.NewCond(&ls.mu)

	go func() {
		err := srv.Serve(ctx, stdinR, stdoutW)
		_ = stdoutW.Close() // unblock the reader goroutine
		ls.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			var probe struct {
				ID *int64 `json:"id"`
			}
			if json.Unmarshal(line, &probe) != nil {
				continue
			}
			ls.mu.Lock()
			if probe.ID != nil {
				var r loommcp.Response
				if json.Unmarshal(line, &r) == nil {
					ls.got[*probe.ID] = r
				}
			} else {
				var n loommcp.Notification
				if json.Unmarshal(line, &n) == nil {
					ls.notes = append(ls.notes, n)
				}
			}
			ls.cond.Broadcast()
			ls.mu.Unlock()
		}
	}()
	return ls
}

func (ls *liveServer) send(frame string) {
	if _, err := io.WriteString(ls.stdinW, frame+"\n"); err != nil {
		ls.t.Fatalf("send frame: %v", err)
	}
}

// waitResp blocks until the response for id arrives or timeout elapses.
func (ls *liveServer) waitResp(id int64, timeout time.Duration) loommcp.Response {
	ls.t.Helper()
	deadline := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		ls.mu.Lock()
		close(deadline)
		ls.cond.Broadcast()
		ls.mu.Unlock()
	})
	defer timer.Stop()
	ls.mu.Lock()
	for {
		if r, ok := ls.got[id]; ok {
			ls.mu.Unlock()
			return r
		}
		select {
		case <-deadline:
			// Unlock before Fatalf: Fatalf runs runtime.Goexit, and we
			// must not leave ls.mu held while the stdout-reader goroutine
			// is trying to acquire it.
			ls.mu.Unlock()
			ls.t.Fatalf("timed out after %s waiting for response id=%d", timeout, id)
			return loommcp.Response{} // unreachable; Fatalf exits the goroutine
		default:
		}
		ls.cond.Wait()
	}
}

// hasResp reports whether a response for id has arrived yet (non-blocking).
func (ls *liveServer) hasResp(id int64) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	_, ok := ls.got[id]
	return ok
}

func (ls *liveServer) close() {
	_ = ls.stdinW.Close()
	ls.cancel()
}

func toolCallFrame(id int64, name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}

// handshake runs initialize + initialized and waits for the init response.
func (ls *liveServer) handshake() {
	ls.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	ls.waitResp(1, 2*time.Second)
	ls.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// TestServer_LongCallDoesNotBlockSubsequent is the RFC O regression test:
// a blocked spawn_run must NOT delay a subsequent cheap list_runs. On the
// pre-RFC-O serial loop the list_runs response never arrives until the
// spawn_run completes, so waitResp(id=3) times out and the test fails.
func TestServer_LongCallDoesNotBlockSubsequent(t *testing.T) {
	gate := make(chan struct{})
	mc := &mockConnector{spawnGate: gate}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	ls := newLiveServer(t, srv)
	defer ls.close()

	ls.handshake()
	ls.send(toolCallFrame(2, "spawn_run", `{"agent":"x","segments":[]}`)) // blocks on gate
	ls.send(toolCallFrame(3, "list_runs", `{"user_id":"u"}`))             // cheap; must not be HOL-blocked

	// id=3 must arrive while id=2 is still blocked.
	ls.waitResp(3, 2*time.Second)
	if ls.hasResp(2) {
		t.Fatal("spawn_run (id=2) responded before the gate was released — test setup broken")
	}

	close(gate) // release spawn_run
	ls.waitResp(2, 2*time.Second)
}

// TestServer_CancelRunBypassesCapDuringSaturation proves the selective
// cap: with the cap fully occupied by a blocked spawn_run, a control
// call (cancel_run) is NOT bounded and still runs.
func TestServer_CancelRunBypassesCapDuringSaturation(t *testing.T) {
	gate := make(chan struct{})
	mc := &mockConnector{spawnGate: gate}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}, MaxConcurrentCalls: 1})
	ls := newLiveServer(t, srv)
	defer ls.close()

	ls.handshake()
	ls.send(toolCallFrame(2, "spawn_run", `{"agent":"x","segments":[]}`)) // fills the only slot
	ls.send(toolCallFrame(3, "cancel_run", `{"agent_id":"a"}`))           // not bounded → must run

	ls.waitResp(3, 2*time.Second) // cancel_run responds despite the saturated cap
	if ls.hasResp(2) {
		t.Fatal("spawn_run responded before gate release")
	}
	close(gate)
	ls.waitResp(2, 2*time.Second)
}

// TestServer_LongCallsRespectConcurrencyCap proves the cap bounds the
// number of long-running tools executing at once: with cap=2 and three
// blocked spawn_runs outstanding, only two reach the connector.
func TestServer_LongCallsRespectConcurrencyCap(t *testing.T) {
	gate := make(chan struct{})
	mc := &mockConnector{spawnGate: gate}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}, MaxConcurrentCalls: 2})
	ls := newLiveServer(t, srv)
	defer ls.close()

	ls.handshake()
	ls.send(toolCallFrame(2, "spawn_run", `{"agent":"x","segments":[]}`))
	ls.send(toolCallFrame(3, "spawn_run", `{"agent":"x","segments":[]}`))
	ls.send(toolCallFrame(4, "spawn_run", `{"agent":"x","segments":[]}`))

	// Wait for two to enter the connector; the third must stay parked on
	// the semaphore (never enters SpawnRun).
	deadline := time.After(2 * time.Second)
	for mc.spawnActive.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("only %d spawn_runs entered the connector; want 2", mc.spawnActive.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Give the third a chance to (wrongly) slip through, then assert it didn't.
	time.Sleep(100 * time.Millisecond)
	if got := mc.spawnMaxSeen.Load(); got != 2 {
		t.Fatalf("max concurrent spawn_run = %d; cap=2 should bound it to 2", got)
	}

	close(gate) // release all; the third now proceeds
	ls.waitResp(2, 2*time.Second)
	ls.waitResp(3, 2*time.Second)
	ls.waitResp(4, 2*time.Second)
}

// TestServer_QueuedCallErrorsOnShutdown locks the shutdown branch: a
// bounded call still waiting for a concurrency slot when global shutdown
// fires gets an explicit -32603 (not a silent drop the client would
// wait out). An unbuffered sem makes the slot unacquirable, so the call
// deterministically parks until the context is cancelled.
func TestServer_QueuedCallErrorsOnShutdown(t *testing.T) {
	mc := &mockConnector{}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	srv.sem = make(chan struct{}) // unbuffered: a bounded call can never acquire → parks until shutdown
	ls := newLiveServer(t, srv)
	defer ls.close()

	ls.handshake()
	ls.send(toolCallFrame(2, "spawn_run", `{"agent":"x","segments":[]}`)) // bounded → parks on the sem
	time.Sleep(50 * time.Millisecond)                                     // let the read loop dispatch the goroutine
	ls.cancel()                                                           // simulate global shutdown (bgCtx / SIGTERM)

	r := ls.waitResp(2, 2*time.Second)
	if r.Error == nil || r.Error.Code != -32603 {
		t.Fatalf("queued call on shutdown: want -32603 error, got %+v", r)
	}
}

// --- RFC P: spawn_run transport timeout ------------------------------

func decodeSpawnResult(t *testing.T, r loommcp.Response) connector.SpawnRunResult {
	t.Helper()
	var call loommcp.CallToolResult
	if err := json.Unmarshal(r.Result, &call); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if len(call.Content) == 0 {
		t.Fatalf("no content blocks in spawn_run result: %+v", call)
	}
	var sr connector.SpawnRunResult
	if err := json.Unmarshal([]byte(call.Content[0].Text), &sr); err != nil {
		t.Fatalf("unmarshal spawn result: %v", err)
	}
	return sr
}

func TestEffectiveSpawnTimeoutMS(t *testing.T) {
	cases := []struct{ op, caller, want int }{
		{0, 0, 0},       // both unset → no cap
		{0, 100, 100},   // caller only
		{200, 0, 200},   // operator only
		{200, 100, 100}, // caller narrows below the cap
		{100, 200, 100}, // caller can't exceed the operator cap
		{100, 100, 100}, // equal
	}
	for _, c := range cases {
		if got := effectiveSpawnTimeoutMS(c.op, c.caller); got != c.want {
			t.Errorf("effectiveSpawnTimeoutMS(op=%d, caller=%d) = %d; want %d", c.op, c.caller, got, c.want)
		}
	}
}

// TestServer_SpawnRunTimesOut: a per-call timeout_ms bounds a spawn_run
// whose run never finishes (gate never closed). Without the RFC P
// timeout the gated call would block forever and waitResp would fatal —
// so this is also the fail-before regression.
func TestServer_SpawnRunTimesOut(t *testing.T) {
	mc := &mockConnector{spawnGate: make(chan struct{})} // never closed
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	ls := newLiveServer(t, srv)
	defer ls.close()

	ls.handshake()
	ls.send(toolCallFrame(2, "spawn_run", `{"agent":"x","segments":[],"timeout_ms":120}`))
	got := decodeSpawnResult(t, ls.waitResp(2, 3*time.Second))
	if got.Status != "timeout" {
		t.Fatalf("spawn_run status = %q; want \"timeout\"", got.Status)
	}
}

// TestServer_SpawnRunOperatorTimeout: the operator default
// (Config.SpawnRunTimeoutMS) bounds a call that supplies no timeout_ms.
func TestServer_SpawnRunOperatorTimeout(t *testing.T) {
	mc := &mockConnector{spawnGate: make(chan struct{})} // never closed
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}, SpawnRunTimeoutMS: 120})
	ls := newLiveServer(t, srv)
	defer ls.close()

	ls.handshake()
	ls.send(toolCallFrame(2, "spawn_run", `{"agent":"x","segments":[]}`)) // no per-call timeout_ms
	got := decodeSpawnResult(t, ls.waitResp(2, 3*time.Second))
	if got.Status != "timeout" {
		t.Fatalf("spawn_run status = %q; want \"timeout\" (operator default)", got.Status)
	}
}

// TestServer_SpawnRunNoTimeoutByDefault: with neither knob set, a
// completing run returns its normal status (the default imposes no
// transport cap).
func TestServer_SpawnRunNoTimeoutByDefault(t *testing.T) {
	mc := &mockConnector{spawnResult: connector.SpawnRunResult{Status: "completed", FinalText: "ok"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	ls := newLiveServer(t, srv)
	defer ls.close()

	ls.handshake()
	ls.send(toolCallFrame(2, "spawn_run", `{"agent":"x","segments":[]}`))
	got := decodeSpawnResult(t, ls.waitResp(2, 3*time.Second))
	if got.Status != "completed" {
		t.Fatalf("spawn_run status = %q; want \"completed\" (no timeout by default)", got.Status)
	}
}
