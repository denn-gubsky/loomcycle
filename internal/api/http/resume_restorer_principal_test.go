package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// principalRecorder records the auth principal each of its calls saw.
type principalRecorder struct {
	mu   sync.Mutex
	seen []*auth.Principal // nil entry = no principal on ctx
}

func (r *principalRecorder) Name() string        { return "Recorder" }
func (r *principalRecorder) Description() string { return "records the caller's principal" }
func (r *principalRecorder) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (r *principalRecorder) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	var got *auth.Principal
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		got = &p
	}
	r.mu.Lock()
	r.seen = append(r.seen, got)
	r.mu.Unlock()
	return tools.Result{Text: "ok"}, nil
}

// restorerAdmin is the principal authMiddleware (or the gRPC interceptor)
// leaves on a restore request made with an operator admin token.
var restorerAdmin = auth.Principal{
	TenantID: "ops", Subject: "root-operator", Scopes: []string{auth.ScopeAdmin}, TokenSuffix: "abc123",
}

// resumedTenantRunTools resumes tenant acme's paused run through trigger and
// returns, by tool_use id, the results of the tools its resumed turn called:
// an AgentDef get of tenant globex's def, Context op=self and a Recorder.
func resumedTenantRunTools(t *testing.T, trigger func(t *testing.T, srv *Server, envelope []byte)) (map[string]providers.Event, *principalRecorder) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"resumer": {
				Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work",
				Tools:          []string{"AgentDef", "Context", "Recorder"},
				AgentDefScopes: []string{"any"},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	ctx := context.Background()

	// globex's def lives on the target, which is where the resumed run looks.
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	globexDef, err := st.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_globex_secret", Name: "globex-secret", Version: 1, Definition: json.RawMessage(`{"system_prompt":"globex only"}`), TenantID: "globex",
	})
	if err != nil {
		t.Fatalf("AgentDefCreate: %v", err)
	}

	getInput, _ := json.Marshal(map[string]any{"op": "get", "def_id": globexDef.DefID})
	prov := &scriptedProvider{
		scripts: [][]providers.Event{{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_get", Name: "AgentDef", Input: getInput}},
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_self", Name: "Context", Input: json.RawMessage(`{"op":"self"}`)}},
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_rec", Name: "Recorder", Input: json.RawMessage(`{}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
		}},
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}
	rec := &principalRecorder{}
	toolset := []tools.Tool{&builtin.AgentDef{Cfg: cfg, Store: st}, &builtin.Context{Cfg: cfg}, rec}
	srv := New(cfg, &stubResolver{p: prov}, toolset, concurrency.New(8, 8, time.Second), st)

	runID, envelope := capturePausedRun(t, store.RunIdentity{AgentID: "a_acme", UserID: "alice", TenantID: "acme", Model: "stub-model"}, "resumer", func(src store.Store, runID string) {
		b, _ := json.Marshal([]loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "look around"}}},
		})
		if err := src.AppendEvent(ctx, runID, "user_input", b); err != nil {
			t.Fatal(err)
		}
	})
	trigger(t, srv, envelope)

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := srv.store.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status == store.RunCompleted {
			break
		}
		if got.Status == store.RunFailed {
			t.Fatalf("resumed run failed: %s", got.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed run did not complete (status=%q)", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	run, _ := srv.store.GetRun(ctx, runID)
	events, err := srv.store.GetTranscript(ctx, run.SessionID)
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	results := map[string]providers.Event{}
	for _, e := range events {
		if e.RunID != runID || e.Type != "tool_result" {
			continue
		}
		var ev providers.Event
		if err := json.Unmarshal(e.Payload, &ev); err != nil {
			t.Fatalf("decode tool_result: %v", err)
		}
		if ev.ToolUse != nil {
			results[ev.ToolUse.ID] = ev
		}
	}
	for _, id := range []string{"tu_get", "tu_self", "tu_rec"} {
		if _, ok := results[id]; !ok {
			t.Fatalf("resumed turn has no tool_result for %s (have %d results)", id, len(results))
		}
	}
	return results, rec
}

// assertResumedAsTheRun is the one postcondition every resume trigger must
// meet: the run acts as tenant acme's run, never as the restoring operator.
func assertResumedAsTheRun(t *testing.T, results map[string]providers.Event, rec *principalRecorder) {
	t.Helper()
	rec.mu.Lock()
	for i, p := range rec.seen {
		if p != nil {
			t.Errorf("tool call %d ran under principal subject=%q tenant=%q scopes=%v; want none", i, p.Subject, p.TenantID, p.Scopes)
		}
	}
	rec.mu.Unlock()

	// Another tenant's def is the same opaque not-found a missing def is.
	get := results["tu_get"]
	if !strings.Contains(get.Text, "not found") || strings.Contains(get.Text, "globex only") {
		t.Errorf("AgentDef get of globex's def from acme's resumed run = %q; want opaque not-found", get.Text)
	}

	self := results["tu_self"]
	var out map[string]any
	if err := json.Unmarshal([]byte(self.Text), &out); err != nil {
		t.Fatalf("Context op=self output is not JSON: %v (%q)", err, self.Text)
	}
	if p, ok := out["principal"]; ok {
		t.Errorf("Context op=self reports principal %v; want none on a resumed run", p)
	}
	if strings.Contains(self.Text, "root-operator") {
		t.Errorf("Context op=self names the restoring admin: %s", self.Text)
	}
	if out["tenant_id"] != "acme" {
		t.Errorf("Context op=self tenant_id = %v, want acme", out["tenant_id"])
	}
}

func TestRestoreSnapshotHTTP_ResumedRunDoesNotActAsRestoringAdmin(t *testing.T) {
	results, rec := resumedTenantRunTools(t, func(t *testing.T, srv *Server, envelope []byte) {
		body, _ := json.Marshal(map[string]any{"json": json.RawMessage(envelope)})
		req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
		req.SetPathValue("id", "inline")
		req = req.WithContext(auth.WithPrincipal(req.Context(), restorerAdmin))
		w := httptest.NewRecorder()
		srv.handleRestoreSnapshot(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("restore status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp snapshotRestoreResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.PausedRunsResumed != 1 {
			t.Fatalf("paused runs resumed = %d, want 1 (warnings %v)", resp.PausedRunsResumed, resp.Warnings)
		}
	})
	assertResumedAsTheRun(t, results, rec)
}

// The gRPC RestoreSnapshot RPC and the MCP restore_snapshot tool both call the
// connector's RestoreSnapshot with the caller's context, principal included.
func TestRestoreSnapshotConnector_ResumedRunDoesNotActAsRestoringAdmin(t *testing.T) {
	results, rec := resumedTenantRunTools(t, func(t *testing.T, srv *Server, envelope []byte) {
		res, err := srv.RestoreSnapshot(auth.WithPrincipal(context.Background(), restorerAdmin), connector.RestoreSnapshotRequest{RawJSON: envelope})
		if err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if res.PausedRunsResumed != 1 {
			t.Fatalf("paused runs resumed = %d, want 1 (warnings %v)", res.PausedRunsResumed, res.Warnings)
		}
	})
	assertResumedAsTheRun(t, results, rec)
}

// A boot resume never had a principal; it must behave exactly as a restore now does.
func TestResumePausedRunsAtBoot_ResumedRunActsAsTheRun(t *testing.T) {
	results, rec := resumedTenantRunTools(t, func(t *testing.T, srv *Server, envelope []byte) {
		ctx := context.Background()
		if _, err := snapshot.Restore(ctx, srv.store, envelope, snapshot.RestoreOptions{}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if n, warnings := srv.ResumePausedRuns(ctx); n != 1 {
			t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings %v)", n, warnings)
		}
	})
	assertResumedAsTheRun(t, results, rec)
}
