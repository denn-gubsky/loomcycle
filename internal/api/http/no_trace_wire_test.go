package http

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The mode reaches a run from four places, and the strictest wins: the
// request, the agent's definition, the caller's token, the parent run. These
// tests are about the first three as a caller sets them; the parent is in
// no_trace_test.go.

func canaryAnswer() *scriptedProvider {
	return &scriptedProvider{defaultS: []providers.Event{{Type: providers.EventText, Text: "answer " + canary}, spend("end_turn", 2)}}
}

func canaryBody(agent, agentID, extra string) string {
	return `{"agent":"` + agent + `","agent_id":"` + agentID + `","user_id":"alice"` + extra +
		`,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"q ` + canary + `"}]}]}`
}

func TestNoTraceWire_TheRequestFieldMakesANoTraceRun(t *testing.T) {
	srv, dbPath := makeServer(t, canaryAnswer(), noTraceConfig())
	code, body := postStrictRun(t, srv, canaryBody("child", "a_req", `,"retention":"none"`))
	if code != 200 || !strings.Contains(body, "answer "+canary) {
		t.Fatalf("status %d, or the caller did not get its answer: %s", code, body)
	}
	if run := runOfAgent(t, srv, "a_req"); run.Retention != store.RetentionNone {
		t.Errorf("retention = %q, want none", run.Retention)
	}
	if onDisk(t, srv, dbPath) {
		t.Error("the canary is on disk after a run that asked for retention none")
	}
	// The run's own response says what it is.
	if got := runToAgentResponse(runOfAgent(t, srv, "a_req"), false); got.Retention != store.RetentionNone {
		t.Errorf("the run's response carries retention %q", got.Retention)
	}

	// "default" and an absent field are the ordinary mode; anything else is
	// refused, so a misspelled "none" cannot become a run that stores it all.
	for _, extra := range []string{`,"retention":"default"`, ``} {
		id := "a_def" + string(rune('0'+len(extra)%7))
		if code, body := postStrictRun(t, srv, canaryBody("child", id, extra)); code != 200 {
			t.Fatalf("%q: status %d: %s", extra, code, body)
		}
		if run := runOfAgent(t, srv, id); run.Retention != "" {
			t.Errorf("%q: retention = %q, want the default", extra, run.Retention)
		}
	}
	for _, bad := range []string{"nonee", "NONE", "off"} {
		if code, body := postStrictRun(t, srv, canaryBody("child", "a_bad", `,"retention":"`+bad+`"`)); code != 400 || !strings.Contains(body, "retention") {
			t.Errorf("retention %q: status %d, want 400: %s", bad, code, body)
		}
		err := srv.RunOnce(context.Background(), runner.RunInput{Agent: "child", Retention: bad,
			Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}}}, runner.RunCallbacks{})
		if !errors.Is(err, runner.ErrInvalidArgument) {
			t.Errorf("RunOnce retention %q: err = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

// The connector is what gRPC, MCP and a batch reach a run through.
func TestNoTraceWire_TheConnectorAndABatchChildCarryIt(t *testing.T) {
	srv, dbPath := makeServer(t, canaryAnswer(), noTraceConfig())
	segs := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "q " + canary}}}}
	res, err := srv.SpawnRun(context.Background(), connector.SpawnRunRequest{Agent: "child", AgentID: "a_conn", UserID: "alice", Segments: segs, Retention: "none"})
	if err != nil {
		t.Fatal(err)
	}
	// The caller gets its answer; the stored run does not hold it.
	if !strings.Contains(res.FinalText, canary) {
		t.Errorf("SpawnRun's answer = %q, want the run's answer", res.FinalText)
	}
	batch, err := srv.SpawnRunBatch(context.Background(), connector.BatchSpawnRequest{Spawns: []connector.SpawnRunRequest{
		{Agent: "child", AgentID: "a_b0", UserID: "alice", Segments: segs, Retention: "none"},
		{Agent: "child", AgentID: "a_b1", UserID: "alice", Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "plain"}}}}},
	}})
	if err != nil || len(batch.Results) != 2 {
		t.Fatalf("batch: %v %+v", err, batch)
	}
	for id, want := range map[string]string{"a_conn": store.RetentionNone, "a_b0": store.RetentionNone, "a_b1": ""} {
		if run := runOfAgent(t, srv, id); run.Retention != want {
			t.Errorf("%s: retention = %q, want %q", id, run.Retention, want)
		}
	}
	// a_b1's prompt was "plain" and its answer carries the canary: it is an
	// ordinary run, so the canary IS on disk — from it alone.
	events, _ := srv.store.GetTranscript(context.Background(), runOfAgent(t, srv, "a_b0").SessionID)
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), canary) {
			t.Errorf("the no-trace batch child stored content: %s", ev.Payload)
		}
	}
	_ = dbPath
}

func TestNoTraceWire_ATokenMakesEveryRunItStartsNoTrace(t *testing.T) {
	answer := []providers.Event{{Type: providers.EventText, Text: "answer " + canary}, spend("end_turn", 2)}
	s, ts, st := restrictedFixtureServer(t, answer, answer)
	ctx := t.Context()

	// The request asks for the default; the token decides.
	body := `{"agent":"other","agent_id":"a_tok","retention":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"q ` + canary + `"}]}]}`
	if code, resp := bearerCall(t, ts, quietBearer, "POST", "/v1/runs", body); code != 200 || !strings.Contains(resp, "answer "+canary) {
		t.Fatalf("status %d: %s", code, resp)
	}
	run, err := st.GetRunByAgentID(ctx, "a_tok")
	if err != nil {
		t.Fatal(err)
	}
	if run.Retention != store.RetentionNone {
		t.Errorf("a run started by a retention-none token has retention %q", run.Retention)
	}
	events, _ := st.GetTranscript(ctx, run.SessionID)
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), canary) {
			t.Errorf("it stored content: %s", ev.Payload)
		}
	}
	// The same request with an ordinary token is an ordinary run.
	plain := strings.Replace(body, "a_tok", "a_tok_plain", 1)
	if code, resp := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", plain); code != 200 {
		t.Fatalf("ordinary token: status %d: %s", code, resp)
	}
	if got, _ := st.GetRunByAgentID(ctx, "a_tok_plain"); got.Retention != "" {
		t.Errorf("an ordinary token's run has retention %q", got.Retention)
	}

	// What such a token cannot do: store a prompt as a draft, start a walk,
	// continue a chat.
	draft := `{"agent":"other","start":false,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, resp := bearerCall(t, ts, quietBearer, "POST", "/v1/runs", draft); code != 409 || !strings.Contains(resp, "retention_none_unsupported") {
		t.Errorf("a draft: status %d, want 409 retention_none_unsupported: %s", code, resp)
	}
	quiet := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant},
		Restrictions: auth.Restrictions{Retention: auth.RetentionNone}})
	// It cannot write its words into what is stored by another road either:
	// an edit of somebody's draft, a message into somebody's ordinary run.
	free := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant}})
	var d runDraft
	d.Agent = "other"
	d.Segments = []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	theirs, err := s.CreateConfiguredRun(free, d)
	if err != nil {
		t.Fatalf("an ordinary token's draft: %v", err)
	}
	if _, err := s.UpdateConfiguredRun(quiet, theirs.RunID, []byte(`{"metadata":{"k":"`+canary+`"}}`)); err == nil || !strings.Contains(err.Error(), "retention_none_unsupported") {
		t.Errorf("a draft edit: err = %v, want it refused", err)
	}
	if _, err := s.CreateConfiguredRun(quiet, d); err == nil || !strings.Contains(err.Error(), "retention_none_unsupported") {
		t.Errorf("a draft created through the connector: err = %v, want it refused", err)
	}
	s.SetSteerRegistry(steer.NewRegistry(8))
	if _, err := s.SteerRun(quiet, run.ID, "a message "+canary, "api"); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("a message from a no-trace token: err = %v, want ErrRetentionUnsupported", err)
	}
	// Nor an answer to a question, nor feedback on a held answer: both are
	// stored beside what they respond to.
	if _, err := s.ResolveInterrupt(quiet, run.ID, "i_any", "", "an answer "+canary, "api", ""); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("an interrupt answer from a no-trace token: err = %v, want ErrRetentionUnsupported", err)
	}
	if _, err := s.ReviewRun(quiet, run.ID, "revise", "feedback "+canary, "api"); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("review feedback from a no-trace token: err = %v, want ErrRetentionUnsupported", err)
	}
	if _, err := s.admitTeamRun(quiet); !errors.Is(err, runner.ErrRetentionUnsupported) {
		t.Errorf("a team walk: err = %v, want ErrRetentionUnsupported", err)
	}
	sess, err := st.CreateSession(ctx, "acme", "other", "dev")
	if err != nil {
		t.Fatal(err)
	}
	msg := `{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"more"}]}]}`
	if code, resp := bearerCall(t, ts, quietBearer, "POST", "/v1/sessions/"+sess.ID+"/messages", msg); code != 409 || !strings.Contains(resp, "retention_none_unsupported") {
		t.Errorf("a continuation: status %d, want 409: %s", code, resp)
	}
}

// The mode is settable where a token is minted, and a stored record this
// build cannot read is refused rather than read as "no setting".
func TestNoTraceWire_MintingATokenWithTheMode(t *testing.T) {
	var req mintUserTokenRequest
	req.Retention = "none"
	if r, err := req.restrictions(); err != nil || r.Retention != auth.RetentionNone {
		t.Errorf("mint with retention none: %+v, %v", r, err)
	}
	req.Retention = "default"
	if r, err := req.restrictions(); err != nil || r.Retention != "" || !r.IsZero() {
		t.Errorf("mint with retention default: %+v, %v; want no restriction", r, err)
	}
	req.Retention = "nonee"
	if _, err := req.restrictions(); err == nil {
		t.Error("a misspelled retention was accepted at mint")
	}

	stored, err := auth.MarshalRestrictions(auth.Restrictions{Retention: auth.RetentionNone})
	if err != nil {
		t.Fatal(err)
	}
	if back, err := auth.ParseRestrictions(stored); err != nil || back.Retention != auth.RetentionNone {
		t.Errorf("round trip: %+v, %v", back, err)
	}
	if _, err := auth.ParseRestrictions(`{"retention":"none-ish"}`); err == nil {
		t.Error("a stored record with an unknown retention was read as usable")
	}
	if (auth.Restrictions{Retention: auth.RetentionNone}).IsZero() {
		t.Error("a retention-only record reads as no restriction: it would not be stored")
	}
}

// An agent defined with retention: none. The config refuses a value it does
// not know.
func TestNoTraceWire_TheAgentDefinitionField(t *testing.T) {
	cfg := noTraceConfig()
	def := cfg.Agents["child"]
	def.Retention = "none"
	cfg.Agents["child"] = def
	srv, dbPath := makeServer(t, canaryAnswer(), cfg)
	if code, body := postStrictRun(t, srv, canaryBody("child", "a_def", `,"retention":"default"`)); code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if run := runOfAgent(t, srv, "a_def"); run.Retention != store.RetentionNone {
		t.Errorf("a run of a retention-none agent has retention %q; a request cannot loosen it", run.Retention)
	}
	if onDisk(t, srv, dbPath) {
		t.Error("the canary is on disk")
	}
	if got := applyAgentDefOverlay(config.AgentDef{}, []byte(`{"retention":"none"}`)); got.Retention != store.RetentionNone {
		t.Errorf("a pinned version's mode was dropped: %q", got.Retention)
	}
	if got := applyAgentDefOverlay(config.AgentDef{Retention: store.RetentionNone}, []byte(`{"retention":"default"}`)); got.Retention != store.RetentionNone {
		t.Errorf("a pinned version loosened its base: %q", got.Retention)
	}
}
