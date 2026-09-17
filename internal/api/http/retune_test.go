package http

import (
	"context"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/steer"
)

func postInput(t *testing.T, ts *httptest.Server, runID, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs/"+runID+"/input", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post input: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// parkedRoutedRun stands up a TIERED interactive run that is genuinely parked,
// and returns it ready to be retuned.
//
// It goes through pause + ResumePausedRuns rather than POST /v1/runs, for two
// reasons. The honest one: an interactive run's SSE stream stays open while it
// parks, so a test that posts and reads the body to completion HANGS — which is
// exactly what the first version of these tests did. The better one: a restored
// parked chat is the case an operator actually retunes, so this is the shape
// worth testing.
func parkedRoutedRun(t *testing.T) (*Server, *httptest.Server, *recordingScriptedProvider, store.Run) {
	t.Helper()
	srv, ts, prov, st := routedServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))

	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "router", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_retune", UserID: "alice", Model: "model-a", Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A conversation that ends on an ASSISTANT turn — the shape of a chat
	// sitting idle waiting for its operator.
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "hello"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{
		Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	waitFor(t, "the restored chat to park", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_input")
	})
	if got := prov.requests(); len(got) != 0 {
		t.Fatalf("the parked run called the provider %d time(s) before any operator turn", len(got))
	}
	return srv, ts, prov, run
}

// RFC DC V7, and THE CROSSING for this phase.
func TestRetune_ParkedRunAdoptsItOnTheNextTurn(t *testing.T) {
	_, ts, prov, run := parkedRoutedRun(t)

	// Retune and speak in one request — the shape an operator's terminal sends
	// when they change the model and continue the conversation.
	code, body := postInput(t, ts, run.ID, `{"text":"now with the other model","overrides":{"model":"model-b"}}`)
	if code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(body))
	}

	got := prov.waitForRequests(t, 1)[0]
	if got.Model != "model-b" {
		t.Errorf("the turn after the retune ran on %q, want model-b — the parked run kept the "+
			"routing it resolved at start", got.Model)
	}
}

// The row must say what the run is actually on, or a restart silently undoes
// the retune: resume restores the model runs.model names (RFC DD Gap 5).
func TestRetune_UpdatesTheRunRowSoARestartCannotUndoIt(t *testing.T) {
	srv, ts, prov, run := parkedRoutedRun(t)

	if code, b := postInput(t, ts, run.ID, `{"text":"go","overrides":{"model":"model-b"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	prov.waitForRequests(t, 1)

	waitFor(t, "runs.model to record the adopted model", func() bool {
		r, err := srv.store.GetRun(context.Background(), run.ID)
		return err == nil && r.Model == "model-b"
	})
}

// MERGE, not replace. Changing the model must not silently drop what the run
// was started with — an override is state with a lifetime (D4).
func TestRetune_MergesRatherThanReplaces(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)
	ctx := context.Background()

	// A run that already carries a budget from its start.
	seed := runConfigRecord{Resources: &resourceOverride{MaxTokens: 4321}}
	if err := srv.store.SetRunConfig(ctx, run.ID, seed.marshal()); err != nil {
		t.Fatal(err)
	}

	if code, b := postInput(t, ts, run.ID, `{"text":"go on","overrides":{"model":"model-b"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}

	rec, ok := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig)
	if !ok {
		t.Fatal("the run lost its configuration record entirely")
	}
	if rec.Routing == nil || rec.Routing.Model != "model-b" {
		t.Errorf("routing = %+v, want model-b", rec.Routing)
	}
	if rec.Resources == nil || rec.Resources.MaxTokens != 4321 {
		t.Errorf("max_tokens = %+v, want the run-start value 4321 kept — retuning the model "+
			"must not silently drop what else the run was started with", rec.Resources)
	}
}

// Refused at the moment it is sent, not on the next turn, and the stored record
// is left alone — so a record is always one the run could actually adopt.
func TestRetune_InvalidOverrideIsRefusedAndChangesNothing(t *testing.T) {
	srv, ts, prov, run := parkedRoutedRun(t)

	before := mustGetRun(t, srv.store, run.ID).RunConfig
	code, body := postInput(t, ts, run.ID, `{"text":"go","overrides":{"model":"model-nowhere"}}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d (%s), want 400", code, strings.TrimSpace(body))
	}
	if after := mustGetRun(t, srv.store, run.ID).RunConfig; string(after) != string(before) {
		t.Errorf("a refused retune changed the stored record: %s → %s", before, after)
	}
	// And it did not wake the run either: a rejected request must not deliver
	// its text.
	if got := prov.requests(); len(got) != 0 {
		t.Errorf("the run took a turn (%d provider calls) despite the 400", len(got))
	}
}

// A run nobody may steer is a run nobody may retune, and it fails the SAME
// opaque way — a retune that could tell "not yours" from "does not exist" would
// make this endpoint an existence oracle for other tenants' run_ids.
func TestRetune_UnknownRunIsOpaque(t *testing.T) {
	srv, ts, _, _ := routedServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))

	code, _ := postInput(t, ts, "r_doesnotexist", `{"text":"go","overrides":{"model":"model-b"}}`)
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — the same answer a steer for an unknown run gets", code)
	}
}
