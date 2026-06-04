package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	spawnReq     atomic.Value // connector.SpawnRunRequest (last)
	spawnResult  connector.SpawnRunResult
	spawnErr     error
	regCalls     atomic.Int32
	regResult    connector.AgentDescriptor
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
}

func (m *mockConnector) SpawnRun(_ context.Context, r connector.SpawnRunRequest) (connector.SpawnRunResult, error) {
	m.spawnReq.Store(r)
	return m.spawnResult, m.spawnErr
}
func (m *mockConnector) CancelRun(_ context.Context, _, _ string) (connector.CancelRunResult, error) {
	return connector.CancelRunResult{Cancelled: true}, nil
}
func (m *mockConnector) GetRun(_ context.Context, _ string) (connector.Run, error) {
	return connector.Run{}, nil
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
	return connector.ChannelDescriptor{}, nil
}
func (m *mockConnector) UpdateChannel(context.Context, string, connector.ChannelUpdateRequest) (connector.ChannelDescriptor, error) {
	return connector.ChannelDescriptor{}, nil
}
func (m *mockConnector) DeleteChannel(context.Context, string) error {
	return nil
}

// driveServer runs the server against the given input lines and
// returns the response frames (one per request). Notifications are
// captured separately.
func driveServer(t *testing.T, srv *Server, input string) (responses []loommcp.Response, notifications []loommcp.Notification) {
	t.Helper()
	stdout := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if len(result.Tools) != 40 {
		t.Errorf("got %d tools, want 40 (RFC L adds operatortokendef on top of the resolve_probe list)", len(result.Tools))
	}
	names := map[string]bool{}
	for _, td := range result.Tools {
		names[td.Name] = true
	}
	// Spot-check across categories — through the v1.x additions.
	for _, want := range []string{"spawn_run", "register_agent", "memory", "agentdef", "skilldef", "mcpserverdef", "scheduledef", "a2aservercarddef", "a2aagentdef", "webhookdef", "memorybackenddef", "operatortokendef", "pause_runtime", "create_snapshot", "get_snapshot", "resolve_probe", "interruption_resolve", "register_hook", "list_hooks", "delete_hook", "list_channels", "stream_user_run_states", "publish_channel", "subscribe_channel", "peek_channel", "ack_channel"} {
		if !names[want] {
			t.Errorf("missing tool %q in tools/list", want)
		}
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
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"register_agent","arguments":{"name":"x","system_prompt":"p","allowed_tools":["Memory"]}}}` + "\n"
	resps, _ := driveServer(t, srv, in)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	if mc.regCalls.Load() != 1 {
		t.Errorf("Connector.RegisterAgent called %d times, want 1", mc.regCalls.Load())
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
