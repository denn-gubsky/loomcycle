package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func snapshotEvent(system, input string) providers.Event {
	return providers.Event{Type: providers.EventPromptSnapshot, PromptSnapshot: &providers.PromptSnapshotInfo{
		System: []providers.ContentBlock{{Type: "text", Text: system}},
		Input:  []providers.ContentBlock{{Type: "text", Text: input}},
	}}
}

// The snapshot is a store-side record: persisted for GET /prompt, never put on
// the live stream — with a store or without one.
func TestRecordingEmit_PersistsThePromptSnapshotButNeverForwardsIt(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	run := seedTenantRun(t, srv.store, "acme", "u1", "a_snap")

	var forwarded []providers.EventType
	emit := srv.makeRecordingEmit(context.Background(), run.ID, tools.RunIdentityValue{}, run.SessionID,
		func(ev providers.Event) { forwarded = append(forwarded, ev.Type) })
	emit(snapshotEvent("sys", "go"))
	if len(forwarded) != 0 {
		t.Errorf("the snapshot reached the live stream: %v", forwarded)
	}
	evs, err := srv.store.GetRunEventsSince(context.Background(), run.ID, 0, 50)
	if err != nil {
		t.Fatalf("GetRunEventsSince: %v", err)
	}
	stored := false
	for _, ev := range evs {
		if ev.Type == string(providers.EventPromptSnapshot) {
			stored = true
		}
	}
	if !stored {
		t.Error("the snapshot was not persisted")
	}

	storeless := &Server{}
	var leaked int
	storeless.makeRecordingEmit(context.Background(), "", tools.RunIdentityValue{}, "",
		func(providers.Event) { leaked++ })(snapshotEvent("sys", "go"))
	if leaked != 0 {
		t.Error("without a store the snapshot was forwarded to the live stream")
	}
}

func getPrompt(t *testing.T, srv *Server, ctx context.Context, runID string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/runs/"+runID+"/prompt", nil)
	req.SetPathValue("run_id", runID)
	rec := httptest.NewRecorder()
	srv.handleGetRunPrompt(rec, req.WithContext(ctx))
	return rec.Code, rec.Body.String()
}

// GET /v1/runs/{id}/prompt answers with the FIRST snapshot (a resumed run
// records another; the first is what the run was started with), is tenant-gated
// like every run read, and tells "no recorded prompt" apart from "no such run".
func TestHandleGetRunPrompt(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	run := seedTenantRun(t, srv.store, "acme", "u1", "a_prompt")
	emit := srv.makeRecordingEmit(context.Background(), run.ID, tools.RunIdentityValue{}, run.SessionID, func(providers.Event) {})

	code, body := getPrompt(t, srv, tenantOperatorCtx("acme"), run.ID)
	if code != 404 || !strings.Contains(body, "no_prompt_snapshot") {
		t.Errorf("before any model call = %d %s, want 404 no_prompt_snapshot", code, body)
	}

	emit(snapshotEvent("You are a reviewer.", "review PR 7"))
	emit(snapshotEvent("resumed system", "resumed input"))
	code, body = getPrompt(t, srv, tenantOperatorCtx("acme"), run.ID)
	if code != 200 {
		t.Fatalf("GET /prompt = %d %s", code, body)
	}
	var out struct {
		System []providers.ContentBlock `json:"system"`
		Input  []providers.ContentBlock `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.System) != 1 || out.System[0].Text != "You are a reviewer." || out.Input[0].Text != "review PR 7" {
		t.Errorf("prompt = %+v, want the FIRST snapshot", out)
	}

	if code, body := getPrompt(t, srv, tenantOperatorCtx("other"), run.ID); code != 404 || strings.Contains(body, "reviewer") {
		t.Errorf("another tenant's read = %d %s, want an opaque 404", code, body)
	}
}
