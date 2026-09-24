package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC DI D5 — configured runs over HTTP.

func configuredServer(t *testing.T, slots int) (*Server, *httptest.Server, *recordingProvider, *storesqlite.Store) {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: slots, MaxQueueDepth: 0, QueueTimeoutMS: 50},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.MaxConfiguredRunsPerUser = 2
	prov := &recordingProvider{}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "configured.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(slots, 0, 50*time.Millisecond), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, prov, st
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type createdRun struct {
	RunID     string          `json:"run_id"`
	AgentID   string          `json:"agent_id"`
	SessionID string          `json:"session_id"`
	Status    string          `json:"status"`
	Draft     json.RawMessage `json:"draft"`
}

func createDraft(t *testing.T, ts *httptest.Server, extra string) createdRun {
	t.Helper()
	code, body := do(t, "POST", ts.URL+"/v1/runs",
		`{"agent":"agent","user_id":"u1","start":false,"prompt":"draft me"`+extra+`}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var c createdRun
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// start:false creates a run that holds its request and runs nothing — no
// model call — and the single-run read shows it as configured, with its draft.
func TestConfiguredRun_CreateStoresTheRequestAndRunsNothing(t *testing.T) {
	srv, ts, prov, st := configuredServer(t, 4)
	c := createDraft(t, ts, `,"sampling":{"temperature":0.3}`)
	if c.Status != "configured" || c.RunID == "" || c.AgentID == "" || c.SessionID == "" {
		t.Fatalf("created = %+v", c)
	}
	if prov.last != nil {
		t.Error("the provider was called for a draft")
	}
	run, err := st.GetRun(context.Background(), c.RunID)
	if err != nil || run.Status != store.RunConfigured || run.UserID != "u1" {
		t.Fatalf("row = %+v (%v)", run, err)
	}
	var d map[string]any
	_ = json.Unmarshal(c.Draft, &d)
	if d["agent"] != "agent" || d["segments"] == nil || d["sampling"] == nil {
		t.Errorf("draft = %s, want the agent, the prompt as segments and the sampling", c.Draft)
	}
	for _, k := range []string{"prompt", "start", "user_id", "agent_id", "session_id", "tenant_id", "user_bearer", "user_credentials"} {
		if _, ok := d[k]; ok {
			t.Errorf("draft carries %q: %s", k, c.Draft)
		}
	}
	req := httptest.NewRequest("GET", "/v1/agents/"+c.AgentID, nil)
	req.SetPathValue("agent_id", c.AgentID)
	rec := httptest.NewRecorder()
	srv.handleGetAgent(rec, req)
	if !strings.Contains(rec.Body.String(), `"status":"configured"`) || !strings.Contains(rec.Body.String(), `"draft":`) {
		t.Errorf("GET /v1/agents/{id} = %s, want the draft on a configured run", rec.Body)
	}
}

// A draft never stores a secret and always starts in its own session.
func TestConfiguredRun_CreateRefusesSecretsAndASession(t *testing.T) {
	_, ts, _, _ := configuredServer(t, 4)
	for name, body := range map[string]string{
		"bearer":      `{"agent":"agent","start":false,"prompt":"x","user_bearer":"abcdefghijklmnopqrstuvwxyz"}`,
		"credentials": `{"agent":"agent","start":false,"prompt":"x","user_credentials":{"github":"ghp_x"}}`,
		"session":     `{"agent":"agent","start":false,"prompt":"x","session_id":"s_1"}`,
		"no input":    `{"agent":"agent","start":false}`,
	} {
		if code, b := do(t, "POST", ts.URL+"/v1/runs", body); code != http.StatusBadRequest {
			t.Errorf("%s: create = %d %s, want 400", name, code, b)
		}
	}
}

// A PATCH replaces the draft's fields (prompt sugar included, null removes a
// key) and is validated like a create; identity and secrets cannot be patched.
func TestConfiguredRun_PatchEditsTheDraftAndGuardsIdentity(t *testing.T) {
	_, ts, _, _ := configuredServer(t, 4)
	c := createDraft(t, ts, `,"sampling":{"temperature":0.3}`)
	u := ts.URL + "/v1/runs/" + c.RunID
	code, body := do(t, "PATCH", u, `{"prompt":"edited","sampling":null,"tool_choice":{"mode":"none"}}`)
	if code != 200 || !strings.Contains(body, "edited") || strings.Contains(body, "sampling") || !strings.Contains(body, `"tool_choice"`) {
		t.Fatalf("PATCH = %d %s", code, body)
	}
	for name, patch := range map[string]string{
		"agent":        `{"agent":"other"}`,
		"user":         `{"user_id":"u2"}`,
		"secret":       `{"user_bearer":"abcdefghijklmnopqrstuvwxyz"}`,
		"unknown key":  `{"not_a_field":1}`,
		"bad choice":   `{"tool_choice":{"mode":"required","until":"always"}}`,
		"empty prompt": `{"prompt":""}`,
		"no input":     `{"segments":null}`,
	} {
		if code, b := do(t, "PATCH", u, patch); code != http.StatusBadRequest {
			t.Errorf("%s: PATCH = %d %s, want 400", name, code, b)
		}
	}
	if code, _ := do(t, "PATCH", u, `{}`); code != http.StatusUnprocessableEntity {
		t.Errorf("empty PATCH = %d, want 422", code)
	}
}

// Start runs the draft as edited, in its OWN session and under its OWN run
// id, then the run is an ordinary finished run: its spec is the merged
// configuration, and it can be neither started again nor edited.
func TestConfiguredRun_StartRunsTheDraftInItsOwnRow(t *testing.T) {
	_, ts, prov, st := configuredServer(t, 4)
	c := createDraft(t, ts, "")
	u := ts.URL + "/v1/runs/" + c.RunID
	if code, b := do(t, "PATCH", u, `{"prompt":"edited prompt","sampling":{"temperature":0.4}}`); code != 200 {
		t.Fatalf("PATCH = %d %s", code, b)
	}
	code, body := do(t, "POST", u+"/start", "")
	if code != 200 || !strings.Contains(body, `"type":"done"`) || !strings.Contains(body, c.RunID) || !strings.Contains(body, c.SessionID) {
		t.Fatalf("start = %d %s, want an SSE stream of this run ending in done", code, body)
	}
	if prov.last == nil || prov.last.Temperature == nil || *prov.last.Temperature != 0.4 {
		t.Errorf("provider saw %+v, want the patched temperature", prov.last)
	}
	if !strings.Contains(firstUserTextOf(prov.last.Messages), "edited prompt") {
		t.Errorf("provider saw messages %+v, want the patched prompt", prov.last.Messages)
	}
	run, err := st.GetRun(context.Background(), c.RunID)
	if err != nil || run.Status != store.RunCompleted || run.SessionID != c.SessionID || run.AgentID != c.AgentID {
		t.Fatalf("row after start = %+v (%v)", run, err)
	}
	if rec, ok := decodeRunConfig(run.RunConfig); !ok || rec.Sampling == nil || *rec.Sampling.Temperature != 0.4 {
		t.Errorf("run_config = %s, want the merged sampling", run.RunConfig)
	}
	if code, _ := do(t, "POST", u+"/start", ""); code != http.StatusConflict {
		t.Errorf("second start = %d, want 409", code)
	}
	if code, _ := do(t, "PATCH", u, `{"prompt":"late"}`); code != http.StatusConflict {
		t.Errorf("PATCH after start = %d, want 409", code)
	}
	if code, _ := do(t, "DELETE", u, ""); code != http.StatusConflict {
		t.Errorf("DELETE after start = %d, want 409 (cancel is a live run's verb)", code)
	}
}

// An admission refusal at start is an ordinary status — and it leaves the
// draft as it was, to be started again once there is room.
func TestConfiguredRun_AStartRefusedAtAdmissionLeavesTheDraft(t *testing.T) {
	srv, ts, prov, st := configuredServer(t, 1)
	c := createDraft(t, ts, "")
	release, err := srv.sem.AcquireForUser(context.Background(), "", "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	code, body := do(t, "POST", ts.URL+"/v1/runs/"+c.RunID+"/start", "")
	if code != http.StatusTooManyRequests {
		t.Errorf("start with no slot = %d %s, want 429", code, body)
	}
	if run, _ := st.GetRun(context.Background(), c.RunID); run.Status != store.RunConfigured {
		t.Errorf("refused start left the run %q, want configured", run.Status)
	}
	if prov.last != nil {
		t.Error("the provider was called for a refused start")
	}
	release()
	if code, body := do(t, "POST", ts.URL+"/v1/runs/"+c.RunID+"/start", ""); code != 200 || !strings.Contains(body, `"type":"done"`) {
		t.Errorf("start once there is room = %d %s", code, body)
	}
}

// Discarding a draft takes its session; an unknown run is a 404.
func TestConfiguredRun_DeleteDiscardsTheDraftAndItsSession(t *testing.T) {
	_, ts, _, st := configuredServer(t, 4)
	c := createDraft(t, ts, "")
	if code, b := do(t, "DELETE", ts.URL+"/v1/runs/"+c.RunID, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", code, b)
	}
	var nf *store.ErrNotFound
	if _, err := st.GetSession(context.Background(), c.SessionID); !errors.As(err, &nf) {
		t.Errorf("session after delete: %v, want not found", err)
	}
	if code, _ := do(t, "DELETE", ts.URL+"/v1/runs/"+c.RunID, ""); code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", code)
	}
}

// The per-user cap bounds drafts, and a caller-chosen agent_id is reserved
// against live runs and other drafts.
func TestConfiguredRun_CapAndAgentIDReservation(t *testing.T) {
	_, ts, _, _ := configuredServer(t, 4)
	createDraft(t, ts, `,"agent_id":"a_mine"`)
	if code, b := do(t, "POST", ts.URL+"/v1/runs", `{"agent":"agent","user_id":"u1","start":false,"prompt":"x","agent_id":"a_mine"}`); code != http.StatusConflict {
		t.Errorf("duplicate agent_id = %d %s, want 409", code, b)
	}
	createDraft(t, ts, "")
	if code, b := do(t, "POST", ts.URL+"/v1/runs", `{"agent":"agent","user_id":"u1","start":false,"prompt":"x"}`); code != http.StatusTooManyRequests || !strings.Contains(b, "configured_run_cap") {
		t.Errorf("third draft (cap 2) = %d %s, want 429 configured_run_cap", code, b)
	}
	if code, _ := do(t, "POST", ts.URL+"/v1/runs", `{"agent":"agent","user_id":"u2","start":false,"prompt":"x"}`); code != http.StatusCreated {
		t.Errorf("another user's draft = %d, want 201 (the cap is per user)", code)
	}
}

// Another tenant's draft is the opaque 404 a missing one gets, on every
// draft route; and starting one keeps the identity it was created with,
// whoever starts it.
func TestConfiguredRun_TenantGateAndIdentityAtStart(t *testing.T) {
	srv, _, _, st := configuredServer(t, 4)
	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "acme", "agent", "alice")
	d := `{"agent":"agent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`
	run, err := st.CreateConfiguredRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_acme", TenantID: "acme", UserID: "alice", Isolated: true}, json.RawMessage(d))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{
		{"PATCH", "/v1/runs/" + run.ID}, {"DELETE", "/v1/runs/" + run.ID}, {"POST", "/v1/runs/" + run.ID + "/start"},
	} {
		body := io.Reader(nil)
		if tc.method == "PATCH" {
			body = strings.NewReader(`{"prompt":"x"}`)
		}
		// Straight to the handler with the other tenant's principal: the test
		// server's auth runs in open mode, where there is no tenant to cross.
		req := httptest.NewRequest(tc.method, tc.path, body).WithContext(tenantOperatorCtx("globex"))
		req.SetPathValue("run_id", run.ID)
		rec := httptest.NewRecorder()
		switch tc.method {
		case "PATCH":
			srv.handlePatchConfiguredRun(rec, req)
		case "DELETE":
			srv.handleDeleteConfiguredRun(rec, req)
		default:
			srv.handleStartConfiguredRun(rec, req)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s from another tenant = %d %s, want 404", tc.method, tc.path, rec.Code, rec.Body)
		}
	}

	// An admin starts it: the run stays acme/alice's and stays confined.
	admin := auth.WithPrincipal(ctx, auth.Principal{TenantID: "", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	err = srv.RunOnce(admin, runner.RunInput{
		Agent: "agent", ConfiguredRunID: run.ID, AgentID: run.AgentID, TenantID: run.TenantID, UserID: run.UserID,
		Isolated: run.Isolated,
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
	}, runner.RunCallbacks{})
	if err != nil {
		t.Fatalf("RunOnce start: %v", err)
	}
	got, _ := st.GetRun(ctx, run.ID)
	if got.TenantID != "acme" || got.UserID != "alice" || !got.Isolated {
		t.Errorf("started by an admin = tenant %q user %q isolated %v, want acme/alice/confined", got.TenantID, got.UserID, got.Isolated)
	}

	// The discriminating case. The row's tenant/user are fixed at create and
	// the start transition never rewrites them, so the ROW cannot show a start
	// that re-derived identity from the starter — the RUN would still execute
	// as the starter (memory scope, usage, fairness). The run-state bus carries
	// the run's effective identity: a start by a same-tenant operator (subject
	// "op") must publish alice's run as alice's.
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	sub := bus.Subscribe("alice")
	defer sub.Close()
	sess2, _ := st.CreateSession(ctx, "acme", "agent", "alice")
	run2, err := st.CreateConfiguredRun(ctx, sess2.ID, store.RunIdentity{AgentID: "a_acme2", TenantID: "acme", UserID: "alice"}, json.RawMessage(d))
	if err != nil {
		t.Fatal(err)
	}
	err = srv.RunOnce(tenantOperatorCtx("acme"), runner.RunInput{
		Agent: "agent", ConfiguredRunID: run2.ID, AgentID: run2.AgentID, TenantID: run2.TenantID, UserID: run2.UserID,
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
	}, runner.RunCallbacks{})
	if err != nil {
		t.Fatalf("RunOnce start by a tenant operator: %v", err)
	}
	select {
	case ev := <-sub.C:
		if ev.RunID != run2.ID || ev.TenantID != "acme" {
			t.Errorf("run-state event = %+v, want alice's run in acme", ev)
		}
	case <-time.After(2 * time.Second):
		t.Error("no run-state event reached alice: the started draft ran as the operator who started it")
	}
}

// Every field POST /v1/runs accepts is either carried into the draft or is one
// the draft deliberately leaves to the row / to start. A field added to
// runRequest and not to the draft shape fails here instead of being silently
// dropped from every configured run.
func TestRunDraft_CarriesEveryRequestField(t *testing.T) {
	notInDraft := map[string]bool{
		"prompt": true, "start": true, // normalised away / a verb
		"session_id": true, "tenant_id": true, "user_id": true, "agent_id": true, // the row's
		"user_bearer": true, "user_credentials": true, // supplied at start
	}
	draftKeys := jsonTags(reflect.TypeOf(runDraft{}))
	for k := range jsonTags(reflect.TypeOf(runRequest{})) {
		if notInDraft[k] {
			continue
		}
		if !draftKeys[k] {
			t.Errorf("runRequest field %q has no place in runDraft — a configured run would silently drop it", k)
		}
	}
}

func jsonTags(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Tag.Get("json") == "" {
			for k := range jsonTags(f.Type) {
				out[k] = true
			}
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// firstUserTextOf joins the text of the request's user messages.
func firstUserTextOf(msgs []providers.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Starting, editing and discarding a draft are run writes at runs:create — not
// the default-deny admin arm an unlisted mutating route falls to, which would
// lock a tenant token out of its own drafts. A deeper path is not caught by
// the PATCH / DELETE case.
func TestRequiredScopeFor_ConfiguredRunRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, path, want string
	}{
		{http.MethodPost, "/v1/runs/r_1/start", auth.ScopeRunsCreate},
		{http.MethodPatch, "/v1/runs/r_1", auth.ScopeRunsCreate},
		{http.MethodDelete, "/v1/runs/r_1", auth.ScopeRunsCreate},
		{http.MethodDelete, "/v1/runs/r_1/breakpoints", auth.ScopeAdmin},
		{http.MethodPost, "/v1/runs", auth.ScopeRunsCreate},
	} {
		if got := requiredScopeFor(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s scope = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// One expiry pass discards the drafts created before the cutoff, with their
// sessions, and leaves a newer one; a disabled TTL runs no sweeper at all.
func TestConfiguredRunSweeper_DiscardsExpiredDrafts(t *testing.T) {
	srv, ts, _, st := configuredServer(t, 4)
	old := createDraft(t, ts, "")
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(5 * time.Millisecond)
	fresh := createDraft(t, ts, "")
	srv.sweepConfiguredRuns(context.Background(), cutoff)
	var nf *store.ErrNotFound
	if _, err := st.GetRun(context.Background(), old.RunID); !errors.As(err, &nf) {
		t.Errorf("draft created before the cutoff: %v, want it discarded", err)
	}
	if _, err := st.GetSession(context.Background(), old.SessionID); !errors.As(err, &nf) {
		t.Errorf("its session: %v, want it discarded too", err)
	}
	if run, err := st.GetRun(context.Background(), fresh.RunID); err != nil || run.Status != store.RunConfigured {
		t.Errorf("newer draft = %+v (%v), want it kept", run, err)
	}

	srv.cfg().Env.ConfiguredRunTTL = 0
	done := make(chan struct{})
	go func() { srv.RunConfiguredRunSweeper(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("with the TTL off the sweeper should return at once, not tick forever")
	}
}

// A draft has no conversation yet, so there is nothing to compact.
func TestConfiguredRun_CompactingADraftIsRefused(t *testing.T) {
	_, ts, _, _ := configuredServer(t, 4)
	c := createDraft(t, ts, "")
	code, body := do(t, "POST", ts.URL+"/v1/runs/"+c.RunID+"/compact", `{}`)
	if code != http.StatusConflict || !strings.Contains(body, "run_not_configured") {
		t.Errorf("compact a draft = %d %s, want 409 run_not_configured", code, body)
	}
}

// A draft's parent_context lives on the run row — its start and its events
// take it from there — so a PATCH that changed only the draft's copy would be
// accepted, shown, and ignored. It is fixed at create like the other identity
// keys, and the draft keeps what it was created with.
func TestConfiguredRun_PatchRefusesParentContext(t *testing.T) {
	_, ts, _, _ := configuredServer(t, 4)
	c := createDraft(t, ts, `,"parent_context":{"function_key":"old"}`)
	u := ts.URL + "/v1/runs/" + c.RunID
	code, body := do(t, "PATCH", u, `{"parent_context":{"function_key":"new"}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "parent_context") {
		t.Errorf("PATCH parent_context = %d %s, want 400 naming parent_context", code, body)
	}
	if code, body := do(t, "PATCH", u, `{"prompt":"still editable"}`); code != 200 || !strings.Contains(body, `"function_key":"old"`) {
		t.Errorf("PATCH after the refusal = %d %s, want 200 with the original parent_context", code, body)
	}
}

// A draft reserves its agent_id at create; a run started afterwards with the
// same explicit id would share it, and the agent read — which answers with the
// newest row — would show that run in place of the draft. Every run start
// refuses the id with the draft create's wording, and the draft stays readable.
func TestConfiguredRun_RunStartsRefuseADraftsAgentID(t *testing.T) {
	srv, ts, prov, _ := configuredServer(t, 4)
	c := createDraft(t, ts, `,"agent_id":"a_reserved"`)
	const want = `agent_id \"a_reserved\" is already in use by a live run or another configured run`

	code, body := do(t, "POST", ts.URL+"/v1/runs", `{"agent":"agent","user_id":"u1","prompt":"x","agent_id":"a_reserved"}`)
	if code != http.StatusConflict || !strings.Contains(body, "agent_id_in_use") || !strings.Contains(body, want) {
		t.Errorf("POST /v1/runs with a draft's agent_id = %d %s, want 409 agent_id_in_use", code, body)
	}

	err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "agent", AgentID: "a_reserved",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
	}, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrAgentIDInUse) {
		t.Errorf("RunOnce (gRPC Run, MCP spawn_run) with a draft's agent_id = %v, want ErrAgentIDInUse", err)
	}

	// A continuation of an existing chat is a run start too.
	sess, serr := srv.store.CreateSession(context.Background(), "", "agent", "u1")
	if serr != nil {
		t.Fatal(serr)
	}
	code, body = do(t, "POST", ts.URL+"/v1/sessions/"+sess.ID+"/messages", `{"prompt":"x","agent_id":"a_reserved"}`)
	if code != http.StatusConflict || !strings.Contains(body, "agent_id_in_use") {
		t.Errorf("POST /v1/sessions/{id}/messages with a draft's agent_id = %d %s, want 409 agent_id_in_use", code, body)
	}

	if prov.last != nil {
		t.Error("a refused start reached the provider")
	}
	code, body = do(t, "GET", ts.URL+"/v1/agents/a_reserved", "")
	if code != 200 || !strings.Contains(body, c.RunID) || !strings.Contains(body, `"status":"configured"`) {
		t.Errorf("GET /v1/agents/a_reserved = %d %s, want the draft", code, body)
	}
}

// A start reads the draft, then waits for admission. A PATCH that lands in
// that window answered 200 while the run started with the pre-PATCH draft.
// The start is now conditional on the draft it read: it is refused with a
// 409 draft_changed, nothing runs, and the edited draft is what the next
// start runs.
func TestConfiguredRun_AStartRacedByAPatchIsRefusedAndKeepsTheEdit(t *testing.T) {
	srv, ts, prov, st := configuredServer(t, 4)
	c := createDraft(t, ts, "")
	in, err := srv.ConfiguredRunInput(context.Background(), c.RunID, connector.RunSecrets{})
	if err != nil {
		t.Fatalf("ConfiguredRunInput: %v", err)
	}
	// The PATCH lands while the start above is (notionally) queued.
	if code, b := do(t, "PATCH", ts.URL+"/v1/runs/"+c.RunID, `{"prompt":"edited while queued"}`); code != 200 {
		t.Fatalf("PATCH = %d %s", code, b)
	}
	err = srv.RunOnce(context.Background(), in, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrDraftChanged) {
		t.Fatalf("start built from the pre-PATCH draft = %v, want ErrDraftChanged", err)
	}
	rec := httptest.NewRecorder()
	writeRunOnceError(rec, err)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "draft_changed") {
		t.Errorf("HTTP mapping = %d %s, want 409 draft_changed", rec.Code, rec.Body)
	}
	if prov.last != nil {
		t.Error("the refused start reached the provider")
	}
	if run, _ := st.GetRun(context.Background(), c.RunID); run.Status != store.RunConfigured {
		t.Errorf("row after the refused start = %q, want configured", run.Status)
	}
	code, body := do(t, "POST", ts.URL+"/v1/runs/"+c.RunID+"/start", "")
	if code != 200 || !strings.Contains(body, `"type":"done"`) {
		t.Fatalf("start again = %d %s", code, body)
	}
	if !strings.Contains(firstUserTextOf(prov.last.Messages), "edited while queued") {
		t.Errorf("the run saw %+v, want the edited prompt", prov.last.Messages)
	}
}
