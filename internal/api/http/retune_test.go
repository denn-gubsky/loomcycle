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
	"reflect"
	"sort"
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
		// A retune now produces TWO override events: the server's, recording what
		// the operator asked for, and the loop's, recording the routing the run
		// adopted. This test is about the second, and the routing pair is what
		// distinguishes them (see OverrideInfo.FromModel).
		if !strings.Contains(string(e.Payload), `"from_model"`) {
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

// The reported gap: a consumer could set a run's overrides and never read them
// back, so after a retune a panel could not confirm what the run holds — and
// could not recompute it either, because the merge is not a field-wise union.
func TestRetune_AnswersWithTheMergedRecord(t *testing.T) {
	_, ts, _, run := parkedRoutedRun(t)

	code, body := postRetune(t, ts, run.ID, `{"model":"model-b","max_tokens":4321}`)
	if code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(body))
	}
	var resp struct {
		RunID   string          `json:"run_id"`
		Retuned bool            `json:"retuned"`
		Config  runConfigRecord `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if !resp.Retuned || resp.RunID != run.ID {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Config.Routing == nil || resp.Config.Routing.Model != "model-b" {
		t.Errorf("config.routing = %+v, want model-b", resp.Config.Routing)
	}
	if resp.Config.Resources == nil || resp.Config.Resources.MaxTokens != 4321 {
		t.Errorf("config.resources = %+v, want max_tokens 4321", resp.Config.Resources)
	}
}

// The merge is NOT a field-wise union, which is exactly why echoing the request
// back would be wrong: naming a model CLEARS the provider, because a
// previously-chosen provider must not linger and contradict the new pin.
func TestRetune_TheAnsweredRecordShowsTheMergeTheCallerCannotRecompute(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)

	seed := runConfigRecord{Routing: &routingOverride{Provider: "stub"}}
	if err := srv.store.SetRunConfig(context.Background(), run.ID, seed.marshal()); err != nil {
		t.Fatal(err)
	}
	_, body := postRetune(t, ts, run.ID, `{"model":"model-b"}`)
	var resp struct {
		Config runConfigRecord `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if resp.Config.Routing == nil || resp.Config.Routing.Model != "model-b" {
		t.Fatalf("routing = %+v", resp.Config.Routing)
	}
	if resp.Config.Routing.Provider != "" {
		t.Errorf("provider = %q, want cleared — naming a model pins it, and a caller "+
			"echoing its own request back would show a provider the run no longer has",
			resp.Config.Routing.Provider)
	}
}

func getRunConfig(t *testing.T, ts *httptest.Server, runID string) (int, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/runs/" + runID + "/config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// What a panel needs when it OPENS a chat, rather than only after it writes.
func TestRunConfig_IsReadableWithoutHavingJustWrittenIt(t *testing.T) {
	_, ts, _, run := parkedRoutedRun(t)

	// A run that was never retuned answers 200 with an empty config — "this run
	// overrides nothing" is an answer, not a 404.
	code, body := getRunConfig(t, ts, run.ID)
	if code != 200 {
		t.Fatalf("un-retuned run: %d %s", code, strings.TrimSpace(body))
	}
	if strings.Contains(body, `"routing"`) {
		t.Errorf("an un-retuned run reported routing: %s", body)
	}

	if c, b := postRetune(t, ts, run.ID, `{"max_iterations":40}`); c != 200 {
		t.Fatalf("retune: %d %s", c, strings.TrimSpace(b))
	}
	code, body = getRunConfig(t, ts, run.ID)
	if code != 200 {
		t.Fatalf("after retune: %d %s", code, strings.TrimSpace(body))
	}
	var resp struct {
		RunID  string          `json:"run_id"`
		Config runConfigRecord `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if resp.RunID != run.ID {
		t.Errorf("run_id = %q", resp.RunID)
	}
	if resp.Config.Resources == nil || resp.Config.Resources.MaxIterations != 40 {
		t.Errorf("config.resources = %+v, want max_iterations 40", resp.Config.Resources)
	}
}

// run_ids are not secrets, so the gate must not become an existence oracle.
func TestRunConfig_UnknownRunIsTheSame404AsAForbiddenOne(t *testing.T) {
	_, ts, _, _ := parkedRoutedRun(t)
	if code, _ := getRunConfig(t, ts, "r_does_not_exist"); code != 404 {
		t.Errorf("unknown run → %d, want 404", code)
	}
}

// A retune that moves NO model must still be legible on the transcript.
//
// The reported bug: OverrideInfo.Fields promised "the keys the request actually
// set, so a reader can see a budget or tuning change that moved no model at
// all", and the only site filling it in was the loop's — which is gated on
// routing having moved and hardcoded {"model"}. A retune of nothing but a budget
// produced no event whatsoever, so a consumer's branch for it could never run.
func TestRetune_ABudgetOnlyChangeIsRecordedOnTheTranscript(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)
	ctx := context.Background()

	if code, b := postRetune(t, ts, run.ID, `{"max_tokens":4321,"effort":"high"}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}

	evs, err := srv.store.GetRunEventsSince(ctx, run.ID, 0, 1000)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var got *providers.OverrideInfo
	for _, e := range evs {
		if e.Type != string(providers.EventOverride) {
			continue
		}
		var ev providers.Event
		if err := json.Unmarshal(e.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if ev.Override != nil && len(ev.Override.Fields) > 0 && ev.Override.FromModel == "" {
			got = ev.Override
		}
	}
	if got == nil {
		t.Fatal("a budget-only retune left NO override event on the transcript — the exact " +
			"gap reported: the change happened and the record does not say so")
	}
	want := []string{"effort", "max_tokens"}
	sort.Strings(got.Fields)
	if !reflect.DeepEqual(got.Fields, want) {
		t.Errorf("fields = %v, want %v — Fields must name what the request SET, not a "+
			"hardcoded routing key", got.Fields, want)
	}
	if got.Source != "operator" {
		t.Errorf("source = %q, want operator", got.Source)
	}
}

// setFields is what makes the event honest; a field added to the wire struct and
// not to it is a change that happens silently.
func TestRetune_SetFieldsNamesEveryOverrideTheBodyCarried(t *testing.T) {
	var w runOverridesWire
	body := `{"model":"m","provider":"p","tier":"t","effort":"high","max_tokens":1,
	          "max_iterations":2,"unbounded_iterations":false,"max_concurrent_children":3,
	          "retry_attempts":0,"memory_inject_max_tokens":0,"memory_index_max_bytes":0,
	          "inject_tool_guide":false,"interactive":true,"interruption":{"enabled":true}}`
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := w.setFields()
	sort.Strings(got)
	want := []string{
		"effort", "inject_tool_guide", "interactive", "interruption",
		"max_concurrent_children", "max_iterations", "max_tokens",
		"memory_index_max_bytes", "memory_inject_max_tokens", "model",
		"provider", "retry_attempts", "tier", "unbounded_iterations",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("setFields() = %v\nwant %v\n\nEvery override the wire struct carries must be "+
			"nameable, or a retune of it is invisible on the transcript.", got, want)
	}
	// The meaningful zeros are the ones a naive truthiness check drops.
	for _, z := range []string{"retry_attempts", "inject_tool_guide", "unbounded_iterations"} {
		found := false
		for _, f := range got {
			if f == z {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was set to its meaningful zero and setFields did not name it", z)
		}
	}
}

// A single retune of a ROUTING field produces two override events, and a
// consumer has to be able to tell them apart — the pair is the discriminator.
//
// This is the case the pre-existing routing test stopped covering the moment a
// second event appeared: it took the last match and got whichever was written
// later. Asserting the SHAPE of both is what stops that recurring.
func TestRetune_ARoutingChangeProducesBothEventsAndTheyAreDistinguishable(t *testing.T) {
	srv, ts, prov, run := parkedRoutedRun(t)

	if code, b := postInput(t, ts, run.ID, `{"text":"go","overrides":{"model":"model-b"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	prov.waitForRequests(t, 1)
	waitFor(t, "the routing event to reach the transcript", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), `"from_model"`)
	})

	events, err := srv.store.GetTranscript(context.Background(), run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var requested, applied int
	for _, e := range events {
		if e.RunID != run.ID || e.Type != string(providers.EventOverride) {
			continue
		}
		var ev providers.Event
		if err := json.Unmarshal(e.Payload, &ev); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if ev.Override == nil {
			t.Fatal("an override event persisted with no payload")
		}
		if ev.Override.FromModel == "" {
			requested++
			if len(ev.Override.Fields) == 0 {
				t.Error("the operator-request event named no fields, which is the only thing " +
					"it carries that the routing event does not")
			}
		} else {
			applied++
			if ev.Override.ToModel == "" {
				t.Error("the routing event carried a from with no to")
			}
		}
	}
	if requested != 1 {
		t.Errorf("operator-request events = %d, want 1", requested)
	}
	if applied != 1 {
		t.Errorf("routing-applied events = %d, want 1", applied)
	}
}
