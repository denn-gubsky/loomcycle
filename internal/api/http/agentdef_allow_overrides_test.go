package http

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An agent defined with allow_overrides: false runs on its definition only.
// The lock is on the agent, so it asks nothing about the caller: the tests
// here use the unrestricted token and the operator's own, the two callers a
// token's lock would never stop.

const legacyAdminBearer = "legacy-admin-secret"

func TestFixedAgent_EveryOverrideIsRefusedForEveryCaller(t *testing.T) {
	ts, _ := restrictedFixture(t)
	for _, bearer := range []string{freeBearer, legacyAdminBearer} {
		for _, field := range auth.LockableOverrides {
			code, body := bearerCall(t, ts, bearer, "POST", "/v1/runs", runBodyWith("fixed", field, overrideSamples[field]))
			if code != 403 || !strings.Contains(body, "override_not_allowed") || !strings.Contains(body, field) || !strings.Contains(body, "allow_overrides") {
				t.Errorf("%s: status %d, want 403 override_not_allowed naming it and the agent's setting: %s", field, code, body)
			}
		}
		if code, body := bearerCall(t, ts, bearer, "POST", "/v1/runs", runBody("fixed")); code != 200 {
			t.Errorf("a run with no override: status %d: %s", code, body)
		}
		// What only narrows, bounds or names the run is not an override.
		narrow := `{"agent":"fixed","tools":[],"max_wall_seconds":60,"metadata":{"k":"v"},"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
		if code, body := bearerCall(t, ts, bearer, "POST", "/v1/runs", narrow); code != 200 {
			t.Errorf("a run that only narrows: status %d: %s", code, body)
		}
		// The same overrides on an agent that allows them are still accepted.
		if code, body := bearerCall(t, ts, bearer, "POST", "/v1/runs", runBodyWith("other", "max_tokens", "64")); code != 200 {
			t.Errorf("an override on an agent that allows them: status %d: %s", code, body)
		}
	}
}

func TestFixedAgent_EveryOtherOverridePathIsRefused(t *testing.T) {
	s, ts, st := restrictedFixtureServer(t)
	ctx := t.Context()
	admin := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, runner.ErrOverrideNotAllowed) && (err == nil || !strings.Contains(err.Error(), "override not allowed")) {
			t.Errorf("%s: err = %v, want it refused as an override", what, err)
		}
	}

	sess, err := st.CreateSession(ctx, "acme", "fixed", "dev")
	if err != nil {
		t.Fatal(err)
	}
	msg := `{"max_tokens":64,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"more"}]}]}`
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/sessions/"+sess.ID+"/messages", msg); code != 403 || !strings.Contains(body, "override_not_allowed") {
		t.Errorf("a continuation with an override: status %d, want 403: %s", code, body)
	}

	batch := `{"spawns":[` + runBody("fixed") + `,` + runBodyWith("fixed", "model", `"stub-model"`) + `]}`
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs:batch", batch); code != 200 || strings.Count(body, "override not allowed") != 1 {
		t.Errorf("batch: status %d, want the one child with an override refused: %s", code, body)
	}

	// A run started off HTTP (gRPC, MCP, a schedule, a webhook).
	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	refused("RunOnce", s.RunOnce(admin, runner.RunInput{Agent: "fixed", Effort: "low", Segments: seg}, runner.RunCallbacks{}))
	if err := s.RunOnce(admin, runner.RunInput{Agent: "fixed", Segments: seg}, runner.RunCallbacks{}); err != nil {
		t.Errorf("RunOnce with no override: %v", err)
	}

	// A retune of a run of the agent.
	start := `{"agent":"fixed","agent_id":"a_fixed_retune","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", start); code != 200 {
		t.Fatalf("start: status %d: %s", code, body)
	}
	run, err := st.GetRunByAgentID(ctx, "a_fixed_retune")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.retuneRun(admin, run, &runOverridesWire{Model: "stub-model"})
	refused("a retune", err)

	// A draft: created with an override, edited to carry one.
	draft := func(agent, model string) runDraft {
		var d runDraft
		d.Agent, d.Model, d.Segments = agent, model, seg
		return d
	}
	_, err = s.CreateConfiguredRun(admin, draft("fixed", "stub-model"))
	refused("a draft created with an override", err)
	plain, err := s.CreateConfiguredRun(admin, draft("fixed", ""))
	if err != nil {
		t.Fatalf("a draft with no override: %v", err)
	}
	_, err = s.UpdateConfiguredRun(admin, plain.RunID, []byte(`{"model":"stub-model"}`))
	refused("an override edited into a draft", err)
	// A draft's agent cannot be changed, so an override written for an open
	// agent cannot be carried onto this one by an edit.
	if _, err := s.CreateConfiguredRun(admin, draft("other", "stub-model")); err != nil {
		t.Errorf("a draft of an open agent with an override: %v", err)
	}
}

// A parent agent's per-spawn override is an override too.
func TestFixedAgent_AParentsPerSpawnOverrideIsRefused(t *testing.T) {
	spawn := func(input string) []providers.Event {
		return []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(input)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1}},
		}
	}
	toolResult := func(body string) string {
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, `"type":"tool_result"`) {
				return line
			}
		}
		return ""
	}

	ts, _ := restrictedFixture(t, spawn(`{"name":"fixed","prompt":"go","compaction":{"enabled":false}}`))
	_, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runBody("launcher"))
	if res := toolResult(body); !strings.Contains(res, "override not allowed") || !strings.Contains(res, "compaction") {
		t.Errorf("a spawn of the fixed agent with a compaction override was not refused: %s", body)
	}

	ts, _ = restrictedFixture(t, spawn(`{"name":"fixed","prompt":"go"}`))
	_, body = bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runBody("launcher"))
	if res := toolResult(body); res == "" || strings.Contains(res, "override not allowed") || strings.Contains(res, `"is_error":true`) {
		t.Errorf("a plain spawn of the fixed agent failed: %s", body)
	}
}

// A spawn can pin its child to one version of the agent. The lock that
// decides is that version's: a lock that held only for the active version
// would be opened by naming another.
func TestFixedAgent_APinnedVersionsOwnLockDecides(t *testing.T) {
	lockedDef := strings.TrimSuffix(workerDef("prompt locked pin", "Narrow"), "}") + `,"allow_overrides":false}`
	if got := applyAgentDefOverlay(config.AgentDef{}, []byte(lockedDef)); got.AllowOverrides == nil || *got.AllowOverrides {
		t.Fatalf("a pinned version's lock was dropped by the merge: %v", got.AllowOverrides)
	}
	locked := false
	if got := applyAgentDefOverlay(config.AgentDef{AllowOverrides: &locked}, []byte(workerDef("p"))); got.AllowOverrides == nil || *got.AllowOverrides {
		t.Errorf("a version that says nothing removed the base's lock: %v", got.AllowOverrides)
	}

	f := pinTenantFixture(t)
	row := store.AgentDefRow{DefID: "def_worker_locked", Name: "worker", Version: 2, TenantID: "acme", Definition: []byte(lockedDef), CreatedAt: time.Now()}
	if _, err := f.st.AgentDefCreate(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	spawn := func(defID string, override bool) error {
		ctx := tools.WithRunIdentity(t.Context(), tools.RunIdentityValue{UserID: "alice", AgentID: "a_acme_parent", TenantID: "acme"})
		if override {
			off := false
			ctx = tools.WithCompactionOverride(ctx, &config.Compaction{Enabled: &off})
		}
		_, _, _, err := f.srv.runSubAgent(ctx, "worker", "", "hi", defID)
		return err
	}
	if err := spawn("def_worker_locked", true); !errors.Is(err, runner.ErrOverrideNotAllowed) {
		t.Errorf("an override on a spawn pinned to the locked version: err = %v, want it refused", err)
	}
	if err := spawn("def_worker_locked", false); err != nil {
		t.Errorf("a plain spawn pinned to the locked version: %v", err)
	}
	if err := spawn("def_worker_acme", true); err != nil {
		t.Errorf("an override on a spawn pinned to an open version: %v", err)
	}

	// A resident child is a run driven interactively, which is one of the
	// locked settings.
	ctx := tools.WithRunIdentity(t.Context(), tools.RunIdentityValue{UserID: "alice", AgentID: "a_acme_parent", TenantID: "acme"})
	if _, err := f.srv.prepareSubRun(ctx, "worker", "", "hi", "def_worker_locked", true, func(providers.Event) {}); !errors.Is(err, runner.ErrOverrideNotAllowed) {
		t.Errorf("a resident spawn of the locked version: err = %v, want it refused", err)
	}
}

// A child of the same definition inherits its parent's overrides. Not once
// the definition is locked: what was legal when the parent started is not
// for a run that starts now.
func TestFixedAgent_ALockedDefinitionInheritsNoOverrides(t *testing.T) {
	srv := routingServer(t)
	parentRec := runConfigRecord{Routing: &routingOverride{Model: "model-b"}}
	ctx := tools.WithRunOverrides(t.Context(), tools.RunOverridesValue{Record: parentRec.marshal(), AgentName: "router"})

	locked := false
	got, cfg := srv.inheritOverridesForChild(ctx, config.AgentDef{Tier: "middle", AllowOverrides: &locked}, runConfigRecord{}, "", "router")
	if got.Model != "" || cfg.Routing != nil {
		t.Errorf("a locked child inherited its parent's model: def.Model=%q record=%+v", got.Model, cfg.Routing)
	}
	if open, _ := srv.inheritOverridesForChild(ctx, config.AgentDef{Tier: "middle"}, runConfigRecord{}, "", "router"); open.Model != "model-b" {
		t.Errorf("an open child no longer inherits: model = %q", open.Model)
	}
}
