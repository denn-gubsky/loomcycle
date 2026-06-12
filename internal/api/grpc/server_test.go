package grpc

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// newTestStore opens a fresh sqlite store for the gRPC tests. The
// gRPC server is store-agnostic — sqlite keeps the tests fast +
// avoids requiring a Postgres fixture for unit tests.
func newTestStore(t *testing.T) *storesqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := storesqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// startTestServer brings up an in-process gRPC server on a unix-style
// random port, returns a connected client. Caller defers cleanup().
//
// authToken is what the server expects clients to present. Empty =
// open-mode (no auth).
func startTestServer(t *testing.T, authToken string) (loomcyclepb.LoomcycleClient, *Server, store.Store, func()) {
	t.Helper()

	st := newTestStore(t)
	reg := cancel.NewRegistry()

	adapter := New(Config{
		Store:       st,
		CancelReg:   reg,
		AuthToken:   authToken,
		BuildCommit: "test-commit",
		BuildTime:   "test-time",
	})

	grpcSrv := googlegrpc.NewServer(
		googlegrpc.UnaryInterceptor(adapter.UnaryAuthInterceptor()),
		googlegrpc.StreamInterceptor(adapter.StreamAuthInterceptor()),
	)
	loomcyclepb.RegisterLoomcycleServer(grpcSrv, adapter)

	lis, err := net.Listen("tcp", "127.0.0.1:0") // random port
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := googlegrpc.NewClient(lis.Addr().String(),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	client := loomcyclepb.NewLoomcycleClient(conn)
	cleanup := func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
	}
	return client, adapter, st, cleanup
}

// withAuth attaches an Authorization metadata header to outgoing
// gRPC calls.
func withAuth(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// ---- Health ----

// Health is unauthenticated even when the server has a token set.
// Mirrors HTTP /healthz exemption.
func TestHealth_Unauthenticated(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "secret-token")
	defer cleanup()

	// No auth metadata.
	resp, err := client.Health(context.Background(), &loomcyclepb.HealthRequest{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Ok {
		t.Errorf("ok=false")
	}
	if resp.Commit != "test-commit" {
		t.Errorf("commit = %q, want test-commit", resp.Commit)
	}
	if resp.UptimeSeconds < 0 {
		t.Errorf("uptime negative: %d", resp.UptimeSeconds)
	}
}

// ---- Auth ----

func TestAuth_Required(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "secret-token")
	defer cleanup()

	// No metadata → Unauthenticated.
	_, err := client.GetAgent(context.Background(), &loomcyclepb.GetAgentRequest{AgentId: "a_x"})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("missing-token: code=%s, want Unauthenticated", status.Code(err))
	}

	// Wrong token → Unauthenticated.
	_, err = client.GetAgent(withAuth(context.Background(), "wrong"),
		&loomcyclepb.GetAgentRequest{AgentId: "a_x"})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("wrong-token: code=%s, want Unauthenticated", status.Code(err))
	}

	// Correct token → 404 (no run with that ID — but auth passed).
	_, err = client.GetAgent(withAuth(context.Background(), "secret-token"),
		&loomcyclepb.GetAgentRequest{AgentId: "a_x"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("correct-token: code=%s, want NotFound", status.Code(err))
	}
}

func TestAuth_OpenMode(t *testing.T) {
	// Empty authToken = open mode; no auth required.
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	_, err := client.GetAgent(context.Background(), &loomcyclepb.GetAgentRequest{AgentId: "a_x"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("open-mode no-auth: code=%s, want NotFound", status.Code(err))
	}
}

// ---- GetAgent ----

func TestGetAgent_Found(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()

	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "default", "alice")
	run, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_top1", UserID: "alice",
	})
	_ = st.UpdateHeartbeat(ctx, run.ID)

	got, err := client.GetAgent(ctx, &loomcyclepb.GetAgentRequest{AgentId: "a_top1"})
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.AgentId != "a_top1" || got.SessionId != sess.ID {
		t.Errorf("agent: %+v", got)
	}
	if got.Status != string(store.RunRunning) {
		t.Errorf("status: %q, want running", got.Status)
	}
	if got.LastHeartbeatAt == nil {
		t.Errorf("last_heartbeat_at: nil")
	}
}

func TestGetAgent_NotFound(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	_, err := client.GetAgent(context.Background(), &loomcyclepb.GetAgentRequest{AgentId: "a_nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code=%s, want NotFound", status.Code(err))
	}
}

func TestGetAgent_InvalidIdent(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	_, err := client.GetAgent(context.Background(), &loomcyclepb.GetAgentRequest{AgentId: "bad ident with spaces"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code=%s, want InvalidArgument", status.Code(err))
	}
}

// ---- CancelAgent ----

func TestCancelAgent_Live(t *testing.T) {
	client, adapter, st, cleanup := startTestServer(t, "")
	defer cleanup()

	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "default", "alice")
	_, _ = st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_live", UserID: "alice",
	})
	// Register the agent in the cancel registry so CancelAgent can
	// find it as live.
	cancelled := make(chan struct{})
	err := adapter.cancelReg.Register(cancel.Entry{
		AgentID:   "a_live",
		RunID:     "r_live",
		SessionID: sess.ID,
		UserID:    "alice",
		StartedAt: time.Now(),
	}, func(_ error) { close(cancelled) })
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer adapter.cancelReg.Deregister("a_live")

	resp, err := client.CancelAgent(ctx, &loomcyclepb.CancelAgentRequest{AgentId: "a_live", Reason: "test"})
	if err != nil {
		t.Fatalf("CancelAgent: %v", err)
	}
	if resp.CancelledCount < 1 {
		t.Errorf("cancelled_count = %d, want ≥ 1", resp.CancelledCount)
	}
	select {
	case <-cancelled:
		// good — registry fired the cancelFn
	case <-time.After(200 * time.Millisecond):
		t.Errorf("cancelFn not invoked within 200ms")
	}
}

func TestCancelAgent_AlreadyTerminated(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()

	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "default", "alice")
	run, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_done", UserID: "alice",
	})
	_ = st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// Not in registry but exists in store → idempotent 200 with
	// cancelled_count=0.
	resp, err := client.CancelAgent(ctx, &loomcyclepb.CancelAgentRequest{AgentId: "a_done"})
	if err != nil {
		t.Fatalf("CancelAgent: %v", err)
	}
	if resp.CancelledCount != 0 {
		t.Errorf("cancelled_count = %d, want 0 (already terminated)", resp.CancelledCount)
	}
}

func TestCancelAgent_NotFound(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	_, err := client.CancelAgent(context.Background(), &loomcyclepb.CancelAgentRequest{AgentId: "a_nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code=%s, want NotFound", status.Code(err))
	}
}

// ---- ListUserAgents ----

func TestListUserAgents(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()

	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "default", "alice")
	r1, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_1", UserID: "alice"})
	_, _ = st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_2", UserID: "alice"})
	_, _ = st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_other", UserID: "bob"})
	_ = st.FinishRun(ctx, r1.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	// All statuses for alice.
	resp, err := client.ListUserAgents(ctx, &loomcyclepb.ListUserAgentsRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("ListUserAgents: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Errorf("alice all: got %d, want 2", len(resp.Agents))
	}

	// Status filter.
	resp, err = client.ListUserAgents(ctx, &loomcyclepb.ListUserAgentsRequest{UserId: "alice", Status: "running"})
	if err != nil {
		t.Fatalf("ListUserAgents running: %v", err)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].AgentId != "a_2" {
		t.Errorf("alice running: %+v", resp.Agents)
	}

	// Bob's runs don't leak.
	for _, a := range resp.Agents {
		if a.UserId == "bob" {
			t.Errorf("bob leaked into alice's list")
		}
	}
}

// ---- GetTranscript ----

func TestGetTranscript(t *testing.T) {
	client, _, st, cleanup := startTestServer(t, "")
	defer cleanup()

	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "default", "alice")
	run, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_x", UserID: "alice"})
	for _, payload := range [][]byte{
		[]byte(`{"type":"started"}`),
		[]byte(`{"type":"text","text":"hi"}`),
		[]byte(`{"type":"done"}`),
	} {
		_ = st.AppendEvent(ctx, run.ID, "text", payload)
	}

	resp, err := client.GetTranscript(ctx, &loomcyclepb.GetTranscriptRequest{SessionId: sess.ID})
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("events: got %d, want 3", len(resp.Events))
	}
	for i := 1; i < len(resp.Events); i++ {
		if resp.Events[i].Seq <= resp.Events[i-1].Seq {
			t.Errorf("seq not ascending at %d", i)
		}
	}
}

func TestGetTranscript_UnknownSession(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	_, err := client.GetTranscript(context.Background(), &loomcyclepb.GetTranscriptRequest{SessionId: "s_nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code=%s, want NotFound", status.Code(err))
	}
}

func TestGetTranscript_EmptySessionID(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	_, err := client.GetTranscript(context.Background(), &loomcyclepb.GetTranscriptRequest{SessionId: ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code=%s, want InvalidArgument", status.Code(err))
	}
}

// ---- Run / Continue with no runner ----
//
// startTestServer constructs the gRPC adapter without injecting a
// runner.Runner — the metadata RPC tests don't need one. Run /
// Continue should refuse cleanly with codes.Unimplemented.

func TestRun_NoRunner(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code=%s, want Unimplemented", status.Code(err))
	}
	if !strings.Contains(err.Error(), "requires a runner") {
		t.Errorf("error doesn't reference 'requires a runner': %v", err)
	}
}

func TestContinue_NoRunner(t *testing.T) {
	client, _, _, cleanup := startTestServer(t, "")
	defer cleanup()

	stream, err := client.Continue(context.Background(), &loomcyclepb.ContinueRequest{SessionId: "s_x"})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code=%s, want Unimplemented", status.Code(err))
	}
}

// ---- Run / Continue with a fakeRunner ----
//
// Streaming wiring is exercised here without spinning up a real loop.
// fakeRunner implements runner.Runner — it fires the OnRegistered
// callback synthetically + emits a few events + returns nil. Tests
// assert the proto stream conveys both the synthetic "session" /
// "agent" frames AND each provider event.

func TestRun_Streaming(t *testing.T) {
	fr := &fakeRunner{
		registered: registrationFrame{
			AgentID: "a_top", RunID: "r_top", SessionID: "s_top",
		},
		events: []proverevent{
			{typ: "text", text: "hello"},
			{typ: "tool_use", toolName: "Read", toolInput: []byte(`{"path":"/x"}`)},
			{typ: "done", stopReason: "end_turn"},
		},
	}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()

	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := drain(t, stream)
	// Expected: 2 synthetic frames (session, agent) + 3 events.
	if len(got) != 5 {
		t.Fatalf("frames: got %d, want 5\n%+v", len(got), got)
	}
	if got[0].Type != "session" || got[0].Text != "s_top" {
		t.Errorf("frame[0]: %+v", got[0])
	}
	if got[1].Type != "agent" || got[1].Text != "a_top" {
		t.Errorf("frame[1]: %+v", got[1])
	}
	if !strings.Contains(got[1].Error, "r_top") || !strings.Contains(got[1].Error, "s_top") {
		t.Errorf("frame[1].error envelope missing run/session: %s", got[1].Error)
	}
	if got[2].Type != "text" || got[2].Text != "hello" {
		t.Errorf("frame[2]: %+v", got[2])
	}
	if got[3].ToolUse == nil || got[3].ToolUse.Name != "Read" {
		t.Errorf("frame[3] tool_use: %+v", got[3])
	}
	if got[4].StopReason != "end_turn" {
		t.Errorf("frame[4] stop_reason: %+v", got[4])
	}
}

// TestRun_PerRunPolicyFields confirms that the v0.8.x per-run policy
// fields (tenant_id, user_tier, user_bearer) on RunRequest reach
// runner.RunInput — proto regen + runInputFromProto wiring regression
// guard. Without this, gRPC consumers (the Python adapter) couldn't
// pass per-tenant / per-tier / per-user-bearer signals and the HTTP
// surface was the only path that did.
func TestRun_PerRunPolicyFields(t *testing.T) {
	fr := &fakeRunner{
		registered: registrationFrame{AgentID: "a_top", RunID: "r_top", SessionID: "s_top"},
		events:     []proverevent{{typ: "done", stopReason: "end_turn"}},
	}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()

	stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{
		Agent:      "default",
		TenantId:   "acme-corp",
		UserTier:   "pro",
		UserBearer: "bearer_AbCdEfGhIjKlMnOpQrStUv0123456789",
		UserId:     "u_test",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Drain to ensure the RunOnce call completes before we read lastInput.
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if fr.lastInput.TenantID != "acme-corp" {
		t.Errorf("TenantID = %q, want acme-corp", fr.lastInput.TenantID)
	}
	if fr.lastInput.UserTier != "pro" {
		t.Errorf("UserTier = %q, want pro", fr.lastInput.UserTier)
	}
	if fr.lastInput.UserBearer != "bearer_AbCdEfGhIjKlMnOpQrStUv0123456789" {
		t.Errorf("UserBearer = %q, want the long bearer", fr.lastInput.UserBearer)
	}
	if fr.lastInput.UserID != "u_test" {
		t.Errorf("UserID = %q, want u_test (regression guard for v0.4 wiring)", fr.lastInput.UserID)
	}
}

// TestContinue_PerRunPolicyFields confirms ContinueRequest's per-call
// policy fields (user_tier, user_bearer) reach runner.RunInput. Note:
// tenant_id is deliberately NOT on ContinueRequest — the server
// inherits the session's tenant; this test pins that wire decision by
// confirming the fields that ARE accepted flow through.
func TestContinue_PerRunPolicyFields(t *testing.T) {
	fr := &fakeRunner{
		registered: registrationFrame{AgentID: "a_top", RunID: "r_top", SessionID: "s_existing"},
		events:     []proverevent{{typ: "done", stopReason: "end_turn"}},
	}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()

	stream, err := client.Continue(context.Background(), &loomcyclepb.ContinueRequest{
		SessionId:  "s_existing",
		UserTier:   "enterprise",
		UserBearer: "bearer_ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210",
	})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if fr.lastInput.UserTier != "enterprise" {
		t.Errorf("UserTier = %q, want enterprise", fr.lastInput.UserTier)
	}
	if fr.lastInput.UserBearer != "bearer_ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210" {
		t.Errorf("UserBearer = %q, want the long bearer", fr.lastInput.UserBearer)
	}
	if fr.lastInput.SessionID != "s_existing" {
		t.Errorf("SessionID = %q, want s_existing", fr.lastInput.SessionID)
	}
}

// TestEventToProto_HostWidening confirms the v0.8.17 host_widening
// payload survives provider.Event → proto.Event conversion. Without
// this, the gRPC stream would emit a `host_widened` event with no
// structured payload and consumers couldn't audit confused-deputy
// patterns.
func TestEventToProto_HostWidening(t *testing.T) {
	ev := providers.Event{
		Type: "host_widened",
		HostWidening: &providers.HostWideningEventInfo{
			ToolCallID: "tu_abc123",
			ToolName:   "WebFetch",
			URL:        "https://api.example.com/v1/things",
			HookOwner:  "jobs-search-web",
			HookName:   "scan-webfetch",
			HostsAdded: []string{"api.example.com", ".example.org"},
		},
	}
	pb := eventToProto(ev)
	if pb.GetType() != "host_widened" {
		t.Errorf("type = %q, want host_widened", pb.GetType())
	}
	hw := pb.GetHostWidening()
	if hw == nil {
		t.Fatal("host_widening unset on proto Event")
	}
	if hw.GetToolCallId() != "tu_abc123" || hw.GetToolName() != "WebFetch" {
		t.Errorf("host_widening identity: %+v", hw)
	}
	if hw.GetUrl() != "https://api.example.com/v1/things" {
		t.Errorf("host_widening url: %q", hw.GetUrl())
	}
	if hw.GetHookOwner() != "jobs-search-web" || hw.GetHookName() != "scan-webfetch" {
		t.Errorf("host_widening hook identity: owner=%q name=%q", hw.GetHookOwner(), hw.GetHookName())
	}
	if got := hw.GetHostsAdded(); len(got) != 2 || got[0] != "api.example.com" || got[1] != ".example.org" {
		t.Errorf("host_widening hosts_added: %v", got)
	}
}

func TestRun_StreamingErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		runErr   error
		wantCode codes.Code
	}{
		{"unknown agent", runner.ErrUnknownAgent, codes.InvalidArgument},
		{"invalid argument", runner.ErrInvalidArgument, codes.InvalidArgument},
		{"unknown provider", runner.ErrUnknownProvider, codes.InvalidArgument},
		{"session required", runner.ErrSessionRequired, codes.FailedPrecondition},
		{"session not found", runner.ErrSessionNotFound, codes.NotFound},
		{"session busy", runner.ErrSessionBusy, codes.FailedPrecondition},
		{"agent_id in use", runner.ErrAgentIDInUse, codes.AlreadyExists},
		{"backpressure", runner.ErrBackpressure, codes.ResourceExhausted},
		{"internal", runner.ErrInternal, codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{returnErr: tc.runErr}
			client, cleanup := startTestServerWithRunner(t, fr)
			defer cleanup()

			stream, err := client.Run(context.Background(), &loomcyclepb.RunRequest{Agent: "default"})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			_, err = stream.Recv()
			if status.Code(err) != tc.wantCode {
				t.Errorf("got code=%s, want %s; err=%v", status.Code(err), tc.wantCode, err)
			}
		})
	}
}

func TestContinue_RequiresSessionID(t *testing.T) {
	fr := &fakeRunner{}
	client, cleanup := startTestServerWithRunner(t, fr)
	defer cleanup()

	stream, err := client.Continue(context.Background(), &loomcyclepb.ContinueRequest{}) // no session_id
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code=%s, want InvalidArgument", status.Code(err))
	}
}

// ---- fakeRunner + helpers ----

// fakeRunner satisfies runner.Runner without spinning up a real
// agent loop. Tests configure it with a registration frame + a list
// of events to emit + an optional error to return. lastInput records
// the most recent RunOnce invocation so tests can verify how proto
// fields land in the runner.RunInput.
type fakeRunner struct {
	registered registrationFrame
	events     []proverevent
	returnErr  error
	lastInput  runner.RunInput
}

type registrationFrame struct {
	AgentID, RunID, SessionID, ParentAgentID string
}

type proverevent struct {
	typ        string
	text       string
	toolName   string
	toolInput  []byte
	stopReason string
}

func (f *fakeRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	f.lastInput = in
	if f.returnErr != nil {
		return f.returnErr
	}
	if cb.OnRegistered != nil {
		cb.OnRegistered(f.registered.AgentID, f.registered.RunID, f.registered.SessionID, f.registered.ParentAgentID)
	}
	for _, e := range f.events {
		ev := providers.Event{
			Type:       providers.EventType(e.typ),
			Text:       e.text,
			StopReason: e.stopReason,
		}
		if e.toolName != "" {
			ev.ToolUse = &providers.ToolUse{Name: e.toolName, Input: e.toolInput}
		}
		if cb.OnEvent != nil {
			cb.OnEvent(ev)
		}
	}
	return nil
}

// startTestServerWithRunner is the streaming-test variant of
// startTestServer — same fixture, plus a Runner injection.
func startTestServerWithRunner(t *testing.T, r runner.Runner) (loomcyclepb.LoomcycleClient, func()) {
	t.Helper()

	st := newTestStore(t)
	reg := cancel.NewRegistry()

	adapter := New(Config{
		Store: st, CancelReg: reg, Runner: r,
		BuildCommit: "test-commit", BuildTime: "test-time",
	})

	grpcSrv := googlegrpc.NewServer(
		googlegrpc.UnaryInterceptor(adapter.UnaryAuthInterceptor()),
		googlegrpc.StreamInterceptor(adapter.StreamAuthInterceptor()),
	)
	loomcyclepb.RegisterLoomcycleServer(grpcSrv, adapter)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := googlegrpc.NewClient(lis.Addr().String(),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
	}
	return loomcyclepb.NewLoomcycleClient(conn), cleanup
}

// mockConnector is a minimal connector.Connector for the gRPC
// dispatch-through-connector regression test. Every method records
// it was called; only the methods exercised by the test (CancelRun)
// return meaningful results — the rest return zero values, which is
// fine because the test only calls CancelAgent.
type mockConnector struct {
	cancelCalls atomic.Int32
	lastAgentID atomic.Value // string
	lastReason  atomic.Value // string
	cancelOK    atomic.Bool  // controls return value
	cascade     atomic.Int32 // CascadeCount returned
}

func (m *mockConnector) CancelRun(_ context.Context, agentID, reason string) (connector.CancelRunResult, error) {
	m.cancelCalls.Add(1)
	m.lastAgentID.Store(agentID)
	m.lastReason.Store(reason)
	if !m.cancelOK.Load() {
		return connector.CancelRunResult{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	return connector.CancelRunResult{
		Cancelled:    true,
		CascadeCount: int(m.cascade.Load()),
	}, nil
}

// Every other Connector method returns a zero value — the dispatch
// test only exercises CancelRun. Keeping them in one block so adding
// a new Connector method forces a compile failure here (which is the
// signal we want: "look at me, I'm new").
func (m *mockConnector) SpawnRun(context.Context, connector.SpawnRunRequest) (connector.SpawnRunResult, error) {
	return connector.SpawnRunResult{}, nil
}
func (m *mockConnector) GetRun(context.Context, string) (connector.Run, error) {
	return connector.Run{}, nil
}
func (m *mockConnector) CompactRun(context.Context, string) (connector.CompactResult, error) {
	return connector.CompactResult{}, nil
}
func (m *mockConnector) ListRuns(context.Context, connector.ListRunsFilter) ([]connector.Run, error) {
	return nil, nil
}
func (m *mockConnector) RegisterAgent(context.Context, connector.RegisterAgentRequest) (connector.AgentDescriptor, error) {
	return connector.AgentDescriptor{}, nil
}
func (m *mockConnector) UnregisterAgent(context.Context, string) error { return nil }
func (m *mockConnector) ListAgents(context.Context, bool) ([]connector.AgentDescriptor, error) {
	return nil, nil
}
func (m *mockConnector) Memory(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) Channel(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) AgentDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) SkillDef(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) Evaluation(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) Context(context.Context, json.RawMessage) (connector.ToolResult, error) {
	return connector.ToolResult{}, nil
}
func (m *mockConnector) PauseRuntime(context.Context, int) (connector.PauseResult, error) {
	return connector.PauseResult{}, nil
}
func (m *mockConnector) ResumeRuntime(context.Context) (connector.ResumeResult, error) {
	return connector.ResumeResult{}, nil
}
func (m *mockConnector) GetRuntimeState(context.Context) (connector.RuntimeState, error) {
	return connector.RuntimeState{}, nil
}
func (m *mockConnector) CreateSnapshot(context.Context, connector.CreateSnapshotRequest) (connector.SnapshotDescriptor, error) {
	return connector.SnapshotDescriptor{}, nil
}
func (m *mockConnector) ListSnapshots(context.Context) ([]connector.SnapshotDescriptor, error) {
	return nil, nil
}
func (m *mockConnector) GetSnapshot(context.Context, string) (connector.SnapshotEnvelope, error) {
	return connector.SnapshotEnvelope{}, nil
}
func (m *mockConnector) ExportSnapshot(context.Context, string) (connector.ExportSnapshotResult, error) {
	return connector.ExportSnapshotResult{}, nil
}
func (m *mockConnector) RestoreSnapshot(context.Context, connector.RestoreSnapshotRequest) (connector.RestoreSnapshotResult, error) {
	return connector.RestoreSnapshotResult{}, nil
}
func (m *mockConnector) DeleteSnapshot(context.Context, string) error { return nil }
func (m *mockConnector) ResolveProbe(context.Context) (connector.ResolverMatrix, error) {
	return connector.ResolverMatrix{}, nil
}
func (m *mockConnector) InterruptionResolve(context.Context, connector.InterruptionResolveRequest) (connector.InterruptionResolveResult, error) {
	return connector.InterruptionResolveResult{}, nil
}
func (m *mockConnector) RegisterHook(context.Context, connector.RegisterHookRequest) (connector.RegisterHookResponse, error) {
	return connector.RegisterHookResponse{}, nil
}
func (m *mockConnector) ListHooks(context.Context) (connector.ListHooksResponse, error) {
	return connector.ListHooksResponse{}, nil
}
func (m *mockConnector) DeleteHook(context.Context, string) error { return nil }
func (m *mockConnector) ListChannels(context.Context) (connector.ListChannelsResponse, error) {
	return connector.ListChannelsResponse{}, nil
}
func (m *mockConnector) StreamUserRunStates(context.Context, connector.StreamUserRunStatesRequest, connector.RunStateVisitor) error {
	return nil
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
func (m *mockConnector) PurgeChannel(context.Context, string) (connector.ChannelPurgeResult, error) {
	return connector.ChannelPurgeResult{}, nil
}

// RFC S client twins. Canned representative results so the gRPC mapping
// tests can assert the pb round-trip (map + slice shapes).
func (m *mockConnector) AwaitChannels(context.Context, connector.ChannelAwaitRequest) (connector.ChannelAwaitResult, error) {
	return connector.ChannelAwaitResult{
		Satisfied:     true,
		Mode:          "any",
		Fired:         []string{"c1"},
		TotalMessages: 1,
		Results: map[string]connector.ChannelAwaitEntry{
			"c1": {
				Messages:   []connector.ChannelMessage{{ID: "m1", Value: []byte(`{"x":1}`), PublishedAt: "2026-01-01T00:00:00Z"}},
				NextCursor: "m1",
			},
		},
	}, nil
}
func (m *mockConnector) BroadcastChannels(context.Context, connector.ChannelBroadcastRequest) (connector.ChannelBroadcastResult, error) {
	return connector.ChannelBroadcastResult{
		Published: 2,
		Results: []connector.ChannelBroadcastEntry{
			{Channel: "c1", MsgID: "m1"},
			{Channel: "c2", MsgID: "m2"},
		},
	}, nil
}

// TestGrpcServer_CancelAgent_DispatchesThroughConnector is the v0.8.15
// regression guard: when the gRPC Server is wired with a Connector,
// CancelAgent dispatches through s.connector.CancelRun rather than the
// legacy direct cancelReg+store path. Verifies the architectural
// commitment: gRPC CONSUMES Connector.
func TestGrpcServer_CancelAgent_DispatchesThroughConnector(t *testing.T) {
	mc := &mockConnector{}
	mc.cancelOK.Store(true)
	mc.cascade.Store(3) // pretend 3 sub-agents got cascaded

	adapter := New(Config{
		Store:     newTestStore(t),
		CancelReg: cancel.NewRegistry(),
		Connector: mc,
	})

	resp, err := adapter.CancelAgent(context.Background(), &loomcyclepb.CancelAgentRequest{
		AgentId: "a_test123",
		Reason:  "user requested",
	})
	if err != nil {
		t.Fatalf("CancelAgent: %v", err)
	}

	if mc.cancelCalls.Load() != 1 {
		t.Errorf("Connector.CancelRun called %d times, want 1", mc.cancelCalls.Load())
	}
	if got := mc.lastAgentID.Load(); got != "a_test123" {
		t.Errorf("Connector saw agent_id %q, want %q", got, "a_test123")
	}
	if got := mc.lastReason.Load(); got != "user requested" {
		t.Errorf("Connector saw reason %q, want %q", got, "user requested")
	}
	// CascadeCount=3 from the mock → proto cancelled_count = 1+3 = 4.
	if resp.GetCancelledCount() != 4 {
		t.Errorf("CancelledCount=%d, want 4 (1 root + 3 cascade)", resp.GetCancelledCount())
	}
}

// ---- Hook RPCs ----

// hookConnector is a minimal Connector that exercises the gRPC hook
// handlers' error translation without spinning up a real *http.Server.
// Configurable per-call via the fields below.
type hookConnector struct {
	mockConnector

	registerErr  error
	registerResp connector.RegisterHookResponse
	listResp     connector.ListHooksResponse
	listErr      error
	deleteErr    error

	gotRegister atomic.Value // connector.RegisterHookRequest
	gotDeleteID atomic.Value // string
}

func (m *hookConnector) RegisterHook(_ context.Context, req connector.RegisterHookRequest) (connector.RegisterHookResponse, error) {
	m.gotRegister.Store(req)
	return m.registerResp, m.registerErr
}
func (m *hookConnector) ListHooks(_ context.Context) (connector.ListHooksResponse, error) {
	return m.listResp, m.listErr
}
func (m *hookConnector) DeleteHook(_ context.Context, id string) error {
	m.gotDeleteID.Store(id)
	return m.deleteErr
}

func TestGrpc_RegisterHook_DelegatesAndMapsBody(t *testing.T) {
	hc := &hookConnector{registerResp: connector.RegisterHookResponse{ID: "hook_abc"}}
	adapter := New(Config{Connector: hc, CancelReg: cancel.NewRegistry()})

	resp, err := adapter.RegisterHook(context.Background(), &loomcyclepb.RegisterHookRequest{
		Owner:       "jobs-search-web",
		Name:        "scan-fetch",
		Phase:       "pre",
		Agents:      []string{"*"},
		Tools:       []string{"WebFetch"},
		CallbackUrl: "https://callback.local/h",
		FailMode:    "closed",
		TimeoutMs:   2500,
	})
	if err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	if resp.GetId() != "hook_abc" {
		t.Errorf("id = %q, want hook_abc", resp.GetId())
	}
	got := hc.gotRegister.Load().(connector.RegisterHookRequest)
	if got.Owner != "jobs-search-web" || got.Phase != "pre" || got.CallbackURL != "https://callback.local/h" || got.TimeoutMs != 2500 {
		t.Errorf("connector saw %+v", got)
	}
}

func TestGrpc_RegisterHook_InvalidArgument(t *testing.T) {
	hc := &hookConnector{registerErr: connector.ErrHookInvalidRegistration}
	adapter := New(Config{Connector: hc, CancelReg: cancel.NewRegistry()})

	_, err := adapter.RegisterHook(context.Background(), &loomcyclepb.RegisterHookRequest{Owner: "x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestGrpc_DeleteHook_NotFound(t *testing.T) {
	hc := &hookConnector{deleteErr: connector.ErrHookNotFound}
	adapter := New(Config{Connector: hc, CancelReg: cancel.NewRegistry()})

	_, err := adapter.DeleteHook(context.Background(), &loomcyclepb.DeleteHookRequest{Id: "hook_gone"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %s, want NotFound", status.Code(err))
	}
	if got := hc.gotDeleteID.Load(); got != "hook_gone" {
		t.Errorf("connector saw id %q, want hook_gone", got)
	}
}

func TestGrpc_DeleteHook_MissingID(t *testing.T) {
	adapter := New(Config{Connector: &hookConnector{}, CancelReg: cancel.NewRegistry()})

	_, err := adapter.DeleteHook(context.Background(), &loomcyclepb.DeleteHookRequest{Id: ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestGrpc_NoConnector_RegisterHookUnavailable(t *testing.T) {
	adapter := New(Config{CancelReg: cancel.NewRegistry()}) // no Connector
	_, err := adapter.RegisterHook(context.Background(), &loomcyclepb.RegisterHookRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("code = %s, want Unavailable", status.Code(err))
	}
}

// TestGrpc_ListHooks_ReturnsHookSlice covers hookToProto's
// field-by-field conversion (time.Time → timestamppb, int → int32
// cast, string-typed Phase/FailMode pass-through). Without this
// test the typo "TimeoutMs vs TimeoutMS" or a swapped phase/fail_mode
// would silently ship.
func TestGrpc_ListHooks_ReturnsHookSlice(t *testing.T) {
	registered := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	hc := &hookConnector{listResp: connector.ListHooksResponse{
		Hooks: []*hooks.Hook{{
			ID:           "hook_a",
			Owner:        "jobs-search-web",
			Name:         "scan",
			Phase:        hooks.PhasePost,
			Agents:       []string{"*"},
			Tools:        []string{"WebFetch"},
			CallbackURL:  "https://e.test/h",
			FailMode:     hooks.FailClosed,
			TimeoutMs:    3000,
			RegisteredAt: registered,
		}},
	}}
	adapter := New(Config{Connector: hc, CancelReg: cancel.NewRegistry()})

	resp, err := adapter.ListHooks(context.Background(), &loomcyclepb.ListHooksRequest{})
	if err != nil {
		t.Fatalf("ListHooks: %v", err)
	}
	if len(resp.GetHooks()) != 1 {
		t.Fatalf("got %d hooks, want 1", len(resp.GetHooks()))
	}
	h := resp.GetHooks()[0]
	if h.GetId() != "hook_a" || h.GetOwner() != "jobs-search-web" || h.GetName() != "scan" {
		t.Errorf("identity fields: id=%q owner=%q name=%q", h.GetId(), h.GetOwner(), h.GetName())
	}
	if h.GetPhase() != "post" || h.GetFailMode() != "closed" {
		t.Errorf("phase/fail_mode: %q/%q", h.GetPhase(), h.GetFailMode())
	}
	if h.GetTimeoutMs() != 3000 {
		t.Errorf("timeout_ms = %d, want 3000", h.GetTimeoutMs())
	}
	if h.GetCallbackUrl() != "https://e.test/h" {
		t.Errorf("callback_url = %q", h.GetCallbackUrl())
	}
	if got := h.GetRegisteredAt().AsTime(); !got.Equal(registered) {
		t.Errorf("registered_at = %v, want %v", got, registered)
	}
}

func TestGrpc_NoConnector_ListHooksUnavailable(t *testing.T) {
	adapter := New(Config{CancelReg: cancel.NewRegistry()}) // no Connector
	_, err := adapter.ListHooks(context.Background(), &loomcyclepb.ListHooksRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("code = %s, want Unavailable", status.Code(err))
	}
}

// drain reads every Event from a Run/Continue stream until EOF or
// error. Used by the streaming tests to assert frame ordering +
// content.
func drain(t *testing.T, stream loomcyclepb.Loomcycle_RunClient) []*loomcyclepb.Event {
	t.Helper()
	var out []*loomcyclepb.Event
	for {
		ev, err := stream.Recv()
		if err != nil {
			// EOF on a successful stream is the natural terminator;
			// other errors fail the test.
			if err.Error() == "EOF" || strings.Contains(err.Error(), "EOF") {
				return out
			}
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, ev)
	}
}

// ---- Resolver re-probe RPC ----

// resolveProbeConnector embeds mockConnector and overrides ResolveProbe
// so the gRPC handler's dispatch + proto mapping can be exercised
// without a real *http.Server.
type resolveProbeConnector struct {
	mockConnector
	matrix connector.ResolverMatrix
	err    error
	calls  atomic.Int32
}

func (m *resolveProbeConnector) ResolveProbe(_ context.Context) (connector.ResolverMatrix, error) {
	m.calls.Add(1)
	return m.matrix, m.err
}

func TestGrpc_ResolveProbe_DelegatesAndMapsMatrix(t *testing.T) {
	rc := &resolveProbeConnector{matrix: connector.ResolverMatrix{
		Providers: map[string]connector.ResolverProviderAvailability{
			"mock": {
				Reachable: true,
				Models:    map[string]connector.ResolverModelStatus{"mock-generic": {Listed: true}},
			},
			"openai": {Excluded: true, LastError: "OPENAI_API_KEY not set"},
		},
	}}
	adapter := New(Config{Connector: rc, CancelReg: cancel.NewRegistry()})

	resp, err := adapter.ResolveProbe(context.Background(), &loomcyclepb.ResolveProbeRequest{})
	if err != nil {
		t.Fatalf("ResolveProbe: %v", err)
	}
	if rc.calls.Load() != 1 {
		t.Errorf("Connector.ResolveProbe called %d times, want 1", rc.calls.Load())
	}
	mock := resp.GetProviders()["mock"]
	if mock == nil || !mock.GetReachable() || !mock.GetModels()["mock-generic"].GetListed() {
		t.Errorf("mock provider not mapped through correctly: %+v", mock)
	}
	oa := resp.GetProviders()["openai"]
	if oa == nil || !oa.GetExcluded() || oa.GetLastError() != "OPENAI_API_KEY not set" {
		t.Errorf("openai exclusion not mapped through: %+v", oa)
	}
}

func TestGrpc_ResolveProbe_UnavailableMapsToUnavailable(t *testing.T) {
	rc := &resolveProbeConnector{err: connector.ErrResolveProbeUnavailable}
	adapter := New(Config{Connector: rc, CancelReg: cancel.NewRegistry()})

	_, err := adapter.ResolveProbe(context.Background(), &loomcyclepb.ResolveProbeRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("status code = %v, want Unavailable", status.Code(err))
	}
}
