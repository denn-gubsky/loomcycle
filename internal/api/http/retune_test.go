package http

import (
	"context"
	"encoding/json"
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

// RFC DC V9 on the HTTP side: the retune reaches the TRANSCRIPT as a typed
// event, so a reader can explain why the answers changed after turn N.
//
// Asserted on the persisted JSON — the consumer's view — rather than on the
// struct the loop built, because a missing json tag or an un-persisted event
// type would leave every Go-side test green and every reader with nothing.
func TestRetune_EmitsATypedOverrideEventOnTheTranscript(t *testing.T) {
	srv, ts, prov, run := parkedRoutedRun(t)

	if code, b := postInput(t, ts, run.ID, `{"text":"go","overrides":{"model":"model-b"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	prov.waitForRequests(t, 1)

	waitFor(t, "the override event to reach the transcript", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), `"override"`)
	})

	events, err := srv.store.GetTranscript(context.Background(), run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.RunID != run.ID || e.Type != string(providers.EventOverride) {
			continue
		}
		var ev providers.Event
		if err := json.Unmarshal(e.Payload, &ev); err != nil {
			t.Fatalf("the persisted override event is not a providers.Event: %v", err)
		}
		if ev.Override == nil {
			t.Fatal("the override event persisted with no payload — a reader sees that " +
				"something changed and not what")
		}
		// The pair is "provider/model". This fixture's stub reports one provider
		// id for every candidate, so the MODEL half is what carries the signal
		// here; asserting the provider half would only pin the stub.
		if !strings.HasSuffix(ev.Override.ToModel, "/model-b") {
			t.Errorf("to_model = %q, want it to end /model-b", ev.Override.ToModel)
		}
		if !strings.HasSuffix(ev.Override.FromModel, "/model-a") {
			t.Errorf("from_model = %q, want it to end /model-a — without the FROM the reader "+
				"cannot tell what changed", ev.Override.FromModel)
		}
		if ev.Override.FromModel == ev.Override.ToModel {
			t.Error("from and to are identical, so the event reports a change that did not happen")
		}
		if ev.Override.Source != "operator" {
			t.Errorf("source = %q, want operator", ev.Override.Source)
		}
		found = true
	}
	if !found {
		t.Error("no override event on the transcript")
	}
}

// It must NOT be an EventProviderFallback. A fallback means the runtime moved
// the run because something failed; this means a person chose to. A consumer
// that cannot tell them apart renders a deliberate retune as an outage.
func TestRetune_IsNotReportedAsAProviderFallback(t *testing.T) {
	srv, ts, prov, run := parkedRoutedRun(t)

	if code, b := postInput(t, ts, run.ID, `{"text":"go","overrides":{"model":"model-b"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	prov.waitForRequests(t, 1)
	waitFor(t, "the override event", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), `"override"`)
	})

	if got := runTranscriptText(t, srv.store, run.SessionID, run.ID); strings.Contains(got, "provider_fallback") {
		t.Error("the retune was reported as a provider fallback; a reader would see an " +
			"outage where an operator made a choice")
	}
}

func postRetune(t *testing.T, ts *httptest.Server, runID, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs/"+runID+"/retune", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post retune: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The endpoint exists because `text` is required on /input, so retuning a parked
// chat meant writing a message the operator never wanted to send. Reported by a
// consumer that wanted exactly this and could not express it.
func TestRetuneEndpoint_ChangesTheRunWithoutSendingATurn(t *testing.T) {
	srv, ts, prov, run := parkedRoutedRun(t)
	ctx := context.Background()

	before := len(prov.requests())

	if code, b := postRetune(t, ts, run.ID, `{"model":"model-b"}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}

	rec, ok := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig)
	if !ok || rec.Routing == nil || rec.Routing.Model != "model-b" {
		t.Fatalf("the retune did not reach the run's configuration: %+v", rec)
	}
	// The whole point: no turn was delivered, so the run is still parked and the
	// transcript carries no message the operator did not write.
	if got := len(prov.requests()); got != before {
		t.Errorf("the provider saw %d requests, was %d — a retune woke the run and spent a "+
			"turn, which is the behaviour this endpoint exists to avoid", got, before)
	}
	_ = ctx
}

// An empty body is a caller mistake — misspelled or mis-nested override names —
// and answering 200 would report success for a call that changed nothing.
func TestRetuneEndpoint_RefusesABodyWithNoOverrides(t *testing.T) {
	_, ts, _, run := parkedRoutedRun(t)
	for _, body := range []string{`{}`, `{"overrides":{"model":"model-b"}}`} {
		code, b := postRetune(t, ts, run.ID, body)
		if code != 422 {
			t.Errorf("body %s → %d %s, want 422", body, code, strings.TrimSpace(b))
		}
	}
}

// The crossing: a retune writes the record, and the callback the loop holds must
// read that record back as its park decision. Both halves are unit-tested and
// both pass while the value never travels between them — the failure class this
// project keeps meeting.
//
// Driven through /retune, which is the whole point: taking hold of a run should
// not require putting a message in its transcript that the operator never wanted
// to send. (The same fields ride /input's `overrides` for the atomic case — they
// share runOverridesWire.)
func TestRetune_PromotingARunIsVisibleToTheLoopsParkDecision(t *testing.T) {
	srv, ts, prov, run := parkedRoutedRun(t)
	ctx := context.Background()

	// The callback a run-start site installs, for a run that started PLAIN.
	decide := srv.interactiveNowFn(run.ID, false)
	if decide(ctx) {
		t.Fatal("a run that started non-interactive already reads as interactive")
	}

	before := len(prov.requests())
	if code, b := postRetune(t, ts, run.ID, `{"interactive":true}`); code != 200 {
		t.Fatalf("promote: %d %s", code, strings.TrimSpace(b))
	}
	// No turn was spent taking hold of it.
	if got := len(prov.requests()); got != before {
		t.Errorf("promoting the run cost %d extra provider request(s)", got-before)
	}

	if !decide(ctx) {
		t.Error("after the retune the loop still reads NOT interactive — the promotion " +
			"reached the store and not the decision, so the run would finish instead of parking")
	}

	// And back, or `interactive` is a one-way door: a run started interactive
	// could never be released.
	if code, b := postRetune(t, ts, run.ID, `{"interactive":false}`); code != 200 {
		t.Fatalf("demote: %d %s", code, strings.TrimSpace(b))
	}
	waitFor(t, "the loop to read the demotion", func() bool { return !decide(ctx) })
}

// The callback must FAIL TO THE START-TIME ANSWER. A read that did not work must
// not change whether a run terminates.
func TestRetune_AnUnreadableRunKeepsWhatItStartedAs(t *testing.T) {
	srv, _, _, _ := parkedRoutedRun(t)
	ctx := context.Background()

	for _, started := range []bool{true, false} {
		if got := srv.interactiveNowFn("r_does_not_exist", started)(ctx); got != started {
			t.Errorf("an unreadable run answered %v, want the start-time %v — a failed read "+
				"must never flip a run between parking and finishing", got, started)
		}
	}
}

// Same refusal the /input retune gives, at the same moment — the shared half is
// s.retuneRun, so the two routes cannot disagree about what a retune permits.
func TestRetuneEndpoint_RefusesAnOverrideTheDefinitionForbids(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)

	code, _ := postRetune(t, ts, run.ID, `{"model":"model-the-agent-cannot-use"}`)
	if code == 200 {
		t.Fatal("a model outside the definition's allowlist was accepted")
	}
	if rec, ok := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig); ok && rec.Routing != nil {
		t.Errorf("a refused retune still wrote routing %+v — the stored record must stay one "+
			"the run could actually adopt", rec.Routing)
	}
}

// run_ids are returned to callers and shown in the UI, so the tenant gate must
// not become an existence oracle: unknown and forbidden answer alike.
func TestRetuneEndpoint_UnknownRunIsTheSame404AsAForbiddenOne(t *testing.T) {
	_, ts, _, _ := parkedRoutedRun(t)
	if code, _ := postRetune(t, ts, "r_does_not_exist", `{"model":"model-b"}`); code != 404 {
		t.Errorf("unknown run → %d, want 404", code)
	}
}

// Promoting must not disturb what else the run carries.
func TestRetune_PromotingKeepsTheRestOfTheConfiguration(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)
	ctx := context.Background()

	seed := runConfigRecord{Resources: &resourceOverride{MaxTokens: 4321}}
	if err := srv.store.SetRunConfig(ctx, run.ID, seed.marshal()); err != nil {
		t.Fatal(err)
	}
	if code, b := postRetune(t, ts, run.ID, `{"interactive":true}`); code != 200 {
		t.Fatalf("promote: %d %s", code, strings.TrimSpace(b))
	}

	rec, ok := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig)
	if !ok || rec.Interactive == nil || !*rec.Interactive {
		t.Fatalf("interactive did not persist: %+v", rec)
	}
	if rec.Resources == nil || rec.Resources.MaxTokens != 4321 {
		t.Errorf("max_tokens = %+v, want the run's own 4321 kept", rec.Resources)
	}
}

// An agent whose definition does not enable interruptions can be granted them
// for one run — the operator watching it go wrong could not previously let it
// ask a question, because that was frozen into the definition before the run.
func TestRetune_InterruptionCanBeEnabledForOneRun(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)

	body := `{"interruption":{"enabled":true,"max_pending":2}}`
	if code, b := postRetune(t, ts, run.ID, body); code != 200 {
		t.Fatalf("enable interruption: %d %s", code, strings.TrimSpace(b))
	}

	rec, ok := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig)
	if !ok || rec.Interruption == nil || !rec.Interruption.Enabled {
		t.Fatalf("interruption did not persist as enabled: %+v", rec)
	}
	if rec.Interruption.MaxPending != 2 {
		t.Errorf("max_pending = %d, want 2", rec.Interruption.MaxPending)
	}
}

// isZero is hand-written now: the struct holds a pointer to a slice-bearing type,
// so the `*w == runOverridesWire{}` it used to be no longer compiles. A body
// naming only a NEW field must not read as empty.
func TestRetune_ABodyNamingOnlyTheNewFieldsIsNotEmpty(t *testing.T) {
	for _, body := range []string{`{"interactive":true}`, `{"interruption":{"enabled":true}}`} {
		var w runOverridesWire
		if err := json.Unmarshal([]byte(body), &w); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		if w.isZero() {
			t.Errorf("%s read as an empty override set — a real call would be dropped", body)
		}
	}
}

// A storeless server has nowhere to have recorded a promotion — and the callback
// runs at EVERY turn boundary, so a missing nil check is a panic on every run
// rather than a missing feature. The suite caught this; review did not.
func TestRetune_AStorelessServerDoesNotPanicAtTheBoundary(t *testing.T) {
	var s *Server
	for _, started := range []bool{true, false} {
		if got := s.interactiveNowFn("r_1", started)(context.Background()); got != started {
			t.Errorf("nil server answered %v, want the start-time %v", got, started)
		}
	}
	empty := &Server{}
	if got := empty.interactiveNowFn("r_1", true)(context.Background()); !got {
		t.Error("a server with no store answered false for a run that started interactive")
	}
}
