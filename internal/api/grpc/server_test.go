package grpc

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
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

// ---- Run / Continue stubs (PR 1: Unimplemented; PR 2 wires them) ----

func TestRun_Unimplemented(t *testing.T) {
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
	if !strings.Contains(err.Error(), "PR 2") {
		t.Errorf("error doesn't reference PR 2: %v", err)
	}
}

func TestContinue_Unimplemented(t *testing.T) {
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
