package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The parent_context length bound, on every surface that takes a caller's
// parent_context — gRPC and HTTP side by side, against the one real server, so
// a surface that skips the check cannot hide behind a fake connector.

// pcHarness serves one real HTTP server over both HTTP and gRPC.
type pcHarness struct {
	client loomcyclepb.LoomcycleClient
	url    string
}

func newPCHarness(t *testing.T) *pcHarness {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-parent-context.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(httpSrv.Mux())
	t.Cleanup(ts.Close)

	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})
	gs := googlegrpc.NewServer(
		googlegrpc.UnaryInterceptor(adapter.UnaryAuthInterceptor()),
		googlegrpc.StreamInterceptor(adapter.StreamAuthInterceptor()),
	)
	loomcyclepb.RegisterLoomcycleServer(gs, adapter)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &pcHarness{client: loomcyclepb.NewLoomcycleClient(conn), url: ts.URL}
}

// runStream is what Run and Continue both return.
type runStream interface {
	Recv() (*loomcyclepb.Event, error)
}

// drainRunStream reads a run stream to its end, returning the session id it
// announced and the stream's terminal error (nil on a clean end).
func drainRunStream(s runStream) (sessionID string, err error) {
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return sessionID, nil
		}
		if err != nil {
			return sessionID, err
		}
		if ev.GetType() == "session" {
			sessionID = ev.GetText()
		}
	}
}

const (
	pcRefused  = "refused"
	pcAccepted = "accepted"
)

// grpcOutcome classifies a gRPC call's error. Anything but a clean success or
// an InvalidArgument naming the field is a failure of the test: a refusal that
// surfaces as Internal is the wrong shape even though it refuses.
func grpcOutcome(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return pcAccepted
	}
	if status.Code(err) == codes.InvalidArgument && strings.Contains(err.Error(), "parent_context.function_key") {
		return pcRefused
	}
	t.Fatalf("error = %v, want nil or InvalidArgument naming parent_context.function_key", err)
	return ""
}

// httpOutcome is grpcOutcome for HTTP: 200 or a 400 naming the field.
func httpOutcome(t *testing.T, code int, body string) string {
	t.Helper()
	switch {
	case code == nethttp.StatusOK:
		return pcAccepted
	case code == nethttp.StatusBadRequest && strings.Contains(body, "parent_context.function_key"):
		return pcRefused
	}
	t.Fatalf("status = %d %s, want 200 or 400 naming parent_context.function_key", code, body)
	return ""
}

func httpDo(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := nethttp.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// batchChildCompleted fails the test unless the lone child of an accepted
// batch ran: an over-long field reported as a failed child in a 200 envelope is
// neither a refusal nor an acceptance.
func batchChildCompleted(t *testing.T, childStatus, childErr string) {
	t.Helper()
	if childStatus != "completed" {
		t.Fatalf("batch child status = %q (%s), want completed", childStatus, childErr)
	}
}

func pcJSON(functionKey string) string {
	b, _ := json.Marshal(map[string]string{"root_agent_run_id": "r_root", "function_key": functionKey})
	return string(b)
}

const pcSegmentsJSON = `"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]`

// TestParentContext_OverLongFieldRefusedOnEveryTransport: an over-long
// function_key is refused with the transport's client-error shape — gRPC
// InvalidArgument, HTTP 400 — on every surface that accepts a caller's
// parent_context, and a well-formed one is accepted there. gRPC Run, Continue,
// SpawnRunBatch and POST /v1/runs:batch went unchecked; the rest are controls
// that already refused.
func TestParentContext_OverLongFieldRefusedOnEveryTransport(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// call sends functionKey through the surface and classifies the answer.
		call func(t *testing.T, h *pcHarness, functionKey string) string
	}{
		{"gRPC Run", func(t *testing.T, h *pcHarness, fk string) string {
			stream, err := h.client.Run(ctx, &loomcyclepb.RunRequest{
				Agent: "agent", Segments: userSegment("hi"),
				ParentContext: &loomcyclepb.ParentContext{RootAgentRunId: "r_root", FunctionKey: fk},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = drainRunStream(stream)
			return grpcOutcome(t, err)
		}},
		{"gRPC Continue", func(t *testing.T, h *pcHarness, fk string) string {
			seed, err := h.client.Run(ctx, &loomcyclepb.RunRequest{Agent: "agent", Segments: userSegment("seed")})
			if err != nil {
				t.Fatal(err)
			}
			sessionID, err := drainRunStream(seed)
			if err != nil || sessionID == "" {
				t.Fatalf("seed run: session %q, %v", sessionID, err)
			}
			stream, err := h.client.Continue(ctx, &loomcyclepb.ContinueRequest{
				SessionId: sessionID, Segments: userSegment("again"),
				ParentContext: &loomcyclepb.ParentContext{RootAgentRunId: "r_root", FunctionKey: fk},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = drainRunStream(stream)
			return grpcOutcome(t, err)
		}},
		{"gRPC SpawnRunBatch", func(t *testing.T, h *pcHarness, fk string) string {
			res, err := h.client.SpawnRunBatch(ctx, &loomcyclepb.BatchSpawnRequest{Spawns: []*loomcyclepb.RunRequest{{
				Agent: "agent", Segments: userSegment("hi"),
				ParentContext: &loomcyclepb.ParentContext{RootAgentRunId: "r_root", FunctionKey: fk},
			}}})
			outcome := grpcOutcome(t, err)
			if outcome == pcAccepted {
				if len(res.GetResults()) != 1 {
					t.Fatalf("results = %+v, want one", res.GetResults())
				}
				batchChildCompleted(t, res.GetResults()[0].GetStatus(), res.GetResults()[0].GetError())
			}
			return outcome
		}},
		{"POST /v1/runs:batch", func(t *testing.T, h *pcHarness, fk string) string {
			code, body := httpDo(t, h.url+"/v1/runs:batch",
				`{"spawns":[{"agent":"agent","parent_context":`+pcJSON(fk)+`,`+pcSegmentsJSON+`}]}`)
			outcome := httpOutcome(t, code, body)
			if outcome == pcAccepted {
				var res struct {
					Results []struct{ Status, Error string } `json:"results"`
				}
				if err := json.Unmarshal([]byte(body), &res); err != nil || len(res.Results) != 1 {
					t.Fatalf("envelope = %s (%v), want one result", body, err)
				}
				batchChildCompleted(t, res.Results[0].Status, res.Results[0].Error)
			}
			return outcome
		}},
		// Controls: these validated before the fix.
		{"POST /v1/runs", func(t *testing.T, h *pcHarness, fk string) string {
			code, body := httpDo(t, h.url+"/v1/runs", `{"agent":"agent","parent_context":`+pcJSON(fk)+`,`+pcSegmentsJSON+`}`)
			return httpOutcome(t, code, body)
		}},
		{"POST /v1/sessions/{id}/messages", func(t *testing.T, h *pcHarness, fk string) string {
			// Seeded over gRPC: the session the continuation needs, whichever
			// transport made it.
			seed, err := h.client.Run(ctx, &loomcyclepb.RunRequest{Agent: "agent", Segments: userSegment("seed")})
			if err != nil {
				t.Fatal(err)
			}
			sessionID, err := drainRunStream(seed)
			if err != nil || sessionID == "" {
				t.Fatalf("seed run: session %q, %v", sessionID, err)
			}
			code, body := httpDo(t, h.url+"/v1/sessions/"+sessionID+"/messages",
				`{"parent_context":`+pcJSON(fk)+`,`+pcSegmentsJSON+`}`)
			return httpOutcome(t, code, body)
		}},
		{"gRPC CreateConfiguredRun", func(t *testing.T, h *pcHarness, fk string) string {
			_, err := h.client.CreateConfiguredRun(ctx, &loomcyclepb.RunRequest{
				Agent: "agent", Segments: userSegment("hi"),
				ParentContext: &loomcyclepb.ParentContext{RootAgentRunId: "r_root", FunctionKey: fk},
			})
			return grpcOutcome(t, err)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPCHarness(t)
			if got := tc.call(t, h, strings.Repeat("k", 257)); got != pcRefused {
				t.Errorf("a 257-byte function_key was %s, want %s", got, pcRefused)
			}
			if got := tc.call(t, h, strings.Repeat("k", 256)); got != pcAccepted {
				t.Errorf("a 256-byte function_key was %s, want %s", got, pcAccepted)
			}
		})
	}
}
