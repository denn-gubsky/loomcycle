package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A static agent (the operator's yaml, presets, bundles) has no versions, and a
// registered agent or a tenant's AgentDef of the same name resolves ahead of it
// for that tenant. A run records that it started on a static agent, and a
// resume reads the static definition again rather than whatever the name
// resolves to by then.

// recordedStatic is whether the run recorded that it started on a static agent,
// failing when it recorded no agent version at all.
func recordedStatic(t *testing.T, run store.Run) bool {
	t.Helper()
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.AgentVersion == nil {
		t.Fatalf("run %s recorded no agent version: %s", run.ID, run.RunConfig)
	}
	return rec.AgentVersion.Static
}

func TestRunStart_RecordsThatItStartedOnAStaticAgent(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"yaml-agent": {Model: "stub-model", Tools: []string{}, SystemPrompt: "static"},
		"parent":     {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:parent."},
		"child":      {Model: "stub-model", Tools: []string{}, SystemPrompt: "role:child."},
	}
	prov := &roleProvider{
		roles:   []string{"parent", "child"},
		scripts: map[string][][]providers.Event{"parent": {toolCallTurn("tu_spawn", "Agent", `{"name":"child","prompt":"hi"}`)}},
		calls:   map[string]int{},
	}
	srv, st, ts := spawnCeilingServer(t, cfg, prov, nil)
	ctx := context.Background()

	first := postAgentRun(t, st, ts, "yaml-agent")
	if !recordedStatic(t, first) {
		t.Errorf("POST /v1/runs of a static agent did not record it as static")
	}
	// RunOnce is the start path of gRPC, MCP spawn_run, webhooks, schedules and A2A.
	if err := srv.RunOnce(ctx, runner.RunInput{
		Agent: "yaml-agent", AgentID: "a_once_static",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
	}, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if once, err := st.GetRunByAgentID(ctx, "a_once_static"); err != nil {
		t.Fatal(err)
	} else if !recordedStatic(t, once) {
		t.Errorf("RunOnce of a static agent did not record it as static")
	}
	if code, body := do(t, "POST", ts.URL+"/v1/sessions/"+first.SessionID+"/messages", `{"prompt":"more","agent_id":"a_cont_static"}`); code != http.StatusOK {
		t.Fatalf("continuation: %d %s", code, body)
	}
	if cont, err := st.GetRunByAgentID(ctx, "a_cont_static"); err != nil {
		t.Fatal(err)
	} else if !recordedStatic(t, cont) {
		t.Errorf("a continuation of a static agent did not record it as static")
	}
	// A sub-run (the Agent tool, and a team walk's members) is a run start too.
	if child := spawnLiveChild(t, st, ts, "a_parent_static", ""); !recordedStatic(t, child) {
		t.Errorf("a sub-run of a static agent did not record it as static")
	}

	// Not every definition with no version is static.
	putAgentDef(t, st, "def_worker_v1", "worker", 1, workerDef("v1"), true)
	if recordedStatic(t, postAgentRun(t, st, ts, "worker")) {
		t.Errorf("a run of an AgentDef version recorded it as static")
	}
	if _, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
		Name: "registered", SystemPrompt: "r", Tools: []string{"Narrow"}, Provider: "scripted", Model: "stub-model",
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if recordedStatic(t, postAgentRun(t, st, ts, "registered")) {
		t.Errorf("a run of a registered agent recorded it as static")
	}
}

// staticWorkerConfig declares "worker" as a static agent with v1's prompt and
// tools — the definition every test here starts on.
func staticWorkerConfig() *config.Config {
	cfg := versionConfig()
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{"Narrow"}, SystemPrompt: "prompt v1"},
	}
	return cfg
}

// postTenantAgentRun starts one run of agent in tenant and returns its row once
// it is done. A shadow applies to the tenant it was written in: for the shared
// tenant the static tier resolves first.
func postTenantAgentRun(t *testing.T, st store.Store, ts *httptest.Server, tenant, agent string) store.Run {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"`+agent+`","tenant_id":"`+tenant+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session frame in SSE body:\n%s", body)
	}
	run := onlyRun(t, st, sessionID)
	if run.TenantID != tenant {
		t.Fatalf("fixture drifted: the run is in tenant %q, want %q", run.TenantID, tenant)
	}
	return run
}

// shadowWithRegistration writes a registration of "worker" in tenant with v2's
// prompt and the wider tool list. RegisterAgent refuses a name the static
// config holds, so a registration that shadows one was made while a reload had
// removed it; the row is written directly here, as that leaves it.
func shadowWithRegistration(t *testing.T, st store.Store, tenant string) {
	t.Helper()
	def, err := json.Marshal(config.AgentDef{
		Provider: "scripted", Model: "stub-model", SystemPrompt: "prompt v2", Tools: []string{"Narrow", "Wider"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DynamicAgentUpsert(context.Background(), store.DynamicAgent{
		Name: "worker", Definition: def, CreatedAt: time.Now(), TenantID: tenant,
	}); err != nil {
		t.Fatalf("DynamicAgentUpsert: %v", err)
	}
}

// shadowWithAgentDef forks "worker" in tenant, as AgentDef fork of a static
// agent does: an active tenant version with v2's prompt and the wider tools.
func shadowWithAgentDef(t *testing.T, st store.Store, tenant string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_" + tenant + "_worker", Name: "worker", Version: 1, TenantID: tenant,
		Definition: []byte(workerDef("prompt v2", "Narrow", "Wider")), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AgentDefSetActive(ctx, tenant, "worker", "def_"+tenant+"_worker", ""); err != nil {
		t.Fatal(err)
	}
}

func TestResumedRun_OfAStaticAgentIgnoresAShadowThatAppearedSince(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shadow func(t *testing.T, st store.Store, tenant string)
		// source is how the name resolves in the tenant once shadowed.
		source func(d config.AgentDef) bool
	}{
		{"a registered agent", shadowWithRegistration, func(d config.AgentDef) bool { return d.RegisteredSHA256 != "" }},
		{"an AgentDef version", shadowWithAgentDef, func(d config.AgentDef) bool { return d.DefID != "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Live: one turn that ends. A resume on the shadow would call Wider.
			f := newVersionFixture(t, staticWorkerConfig(), endTurn(), toolCallTurn("tu_w", "Wider", `{}`), endTurn())
			run := postTenantAgentRun(t, f.st, f.ts, "acme", "worker")
			if live := offered(f.prov.waitForRequests(t, 1)[0]); hasTool(live, "Wider") || !hasTool(live, "Narrow") {
				t.Fatalf("fixture drifted: the live run was offered %v", live)
			}

			tc.shadow(t, f.st, "acme")
			if d, ok := f.srv.lookupAgent(context.Background(), "acme", "worker"); !ok || d.Static || !tc.source(d) {
				t.Fatalf("fixture drifted: worker in acme does not resolve to the shadow (%+v, %v)", d, ok)
			}

			resumeAndFinish(t, f.srv, run)
			assertResumedOnV1(t, f, 1)
		})
	}
}

// A static agent removed since has nothing to resume on, and a definition that
// took its name since is not a substitute: the run fails, saying why, and the
// provider is never called.
func TestResumedRun_WhoseStaticAgentWasRemovedFailsWithoutRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		// reuse is what happens to the name once the static agent is gone.
		reuse func(t *testing.T, f *versionFixture)
	}{
		{"removed", func(*testing.T, *versionFixture) {}},
		// With the static agent gone, RegisterAgent accepts its name again.
		{"removed and its name registered", func(t *testing.T, f *versionFixture) {
			registerWorker(t, f, "prompt v2", "Narrow", "Wider")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Live: one turn that ends. A resume that ran would call Wider.
			f := newVersionFixture(t, staticWorkerConfig(), endTurn(), toolCallTurn("tu_w", "Wider", `{}`), endTurn())
			run := postAgentRun(t, f.st, f.ts, "worker")
			f.prov.waitForRequests(t, 1)

			// A config reload that drops the agent swaps the config holder.
			f.srv.cfgHolder.Store(versionConfig())
			tc.reuse(t, f)
			parkForResume(t, f.srv, run.ID)

			// The row finished once already (live), so a failure cannot be read
			// off its status; the resume pass says why it refused.
			n, warnings := f.srv.ResumePausedRuns(context.Background())
			if n != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "static agent the run started on was removed") {
				t.Fatalf("ResumePausedRuns = %d, %v; want 0 re-dispatched and one warning that the static agent was removed", n, warnings)
			}
			if n := len(f.prov.requests()); n != 1 {
				t.Errorf("the provider saw %d calls, want only the live one", n)
			}
			if n := f.wider.calls.Load(); n != 0 {
				t.Errorf("Wider ran %d time(s)", n)
			}
		})
	}
}

// A run recorded before the static marker existed cannot tell a static start
// from a registered one, and resumes by name as it did — here onto the
// registration that has shadowed the static agent since.
func TestResumedRun_OfAStaticAgentWithNoRecordedMarkerResumesByName(t *testing.T) {
	f := newVersionFixture(t, staticWorkerConfig(), toolCallTurn("tu_w", "Wider", `{}`), endTurn())
	ctx := context.Background()
	sess, err := f.st.CreateSession(ctx, "acme", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	rec := runConfigRecord{AgentVersion: &agentVersionRecord{}} // a pre-marker record
	run, err := f.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_premarker", UserID: "alice", TenantID: "acme", Model: "stub-model", RunConfig: rec.marshal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	shadowWithRegistration(t, f.st, "acme")

	resumeAndFinish(t, f.srv, run)
	if sys := systemText(f.prov.waitForRequests(t, 1)[0]); !strings.Contains(sys, "prompt v2") {
		t.Errorf("a pre-marker run resumed with system prompt %q, want the registration it resolves to by name", sys)
	}
	if got, _ := f.st.GetRun(ctx, run.ID); got.Status != store.RunCompleted {
		t.Errorf("a pre-marker run ended %q (%s), want completed", got.Status, got.ErrorMsg)
	}
}
