package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/scheduler"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// End to end, with nothing faked but the model: a trigger definition written
// through its real def tool, fired by the real sweeper or the real receiver,
// starting a real walk. What these assert is the claim a `delivery: team`
// makes — a walk of the named team starts, in the definition's tenant, with
// the variables the definition gives it.

// otherTenantsTeam is varsTeam with a prompt that says which tenant's copy
// ran, so a walk in the wrong tenant is visible in what the model was sent.
func otherTenantsTeam(tenant string) string {
	return strings.Replace(varsTeam, "Review ${var.repo}", "IN "+tenant+" ${var.repo}", 1)
}

// seedTeamEverywhereBut seeds a same-named team in two other tenants and the
// shared layer, each with its own prompt.
func seedTeamEverywhereBut(t *testing.T, st store.Store, name string) {
	t.Helper()
	seedTenantTeam(t, st, "globex", name, otherTenantsTeam("GLOBEX"))
	seedTenantTeam(t, st, "", name, otherTenantsTeam("SHARED"))
}

// acmeOperator is a tenant operator of acme: not an admin, so its triggers
// execute in acme and nowhere else.
func acmeOperator() context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_op", TenantID: "acme", UserID: "op"})
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})
}

// createDueTeamSchedule writes a team schedule as acme's operator and makes it
// due now.
func (h *triggerWalkHarness) createDueTeamSchedule(overlay string) string {
	h.t.Helper()
	h.srv.SetScheduleDefTool(&builtin.ScheduleDef{Store: h.st, Cfg: h.cfg})
	ctx := tools.WithScheduleDefPolicy(acmeOperator(), tools.ScheduleDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := h.srv.ScheduleDef(ctx, json.RawMessage(`{"op":"create","name":"weekly","overlay":{`+overlay+`}}`))
	if err != nil || res.IsError {
		h.t.Fatalf("create: %v %s", err, res.Text)
	}
	var created struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &created); err != nil || created.DefID == "" {
		h.t.Fatalf("create result %s: %v", res.Text, err)
	}
	if err := h.st.ScheduleRunStateSeed(context.Background(), created.DefID, time.Now().Add(-time.Minute)); err != nil {
		h.t.Fatal(err)
	}
	return created.DefID
}

// sweep runs the real sweeper against the server until the test ends.
func (h *triggerWalkHarness) sweep() {
	h.t.Helper()
	sched := scheduler.New(scheduler.Config{TickInterval: 20 * time.Millisecond}, h.st, h.srv, nil, nil, h.t.Logf)
	sched.SetTeamWalkStarter(h.srv)
	sched.Start(context.Background())
	h.t.Cleanup(sched.Stop)
}

func TestScheduleDef_ADueTeamScheduleStartsADetachedWalkInItsTenantWithItsVars(t *testing.T) {
	h := newTriggerWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "weekly-report", varsTeam)
	seedTeamEverywhereBut(t, h.st, "weekly-report")
	defID := h.createDueTeamSchedule(`"schedule":"0 6 * * 1","delivery":"team","team":"weekly-report","vars":{"repo":"loomcycle"},"input":"the digest","user_id":"u-42","max_fires":1`)

	h.sweep()

	var state store.ScheduleRunStateRow
	waitFor(t, "the schedule to record its fire", func() bool {
		got, err := h.st.ScheduleRunStateGet(context.Background(), defID)
		state = got
		return err == nil && got.LastStatus != ""
	})
	if state.LastStatus != "completed" || state.LastRunID == "" || state.FireCount != 1 {
		t.Fatalf("state = status %q run %q fire_count %d (%s), want completed with the walk's run id and one fire",
			state.LastStatus, state.LastRunID, state.FireCount, state.LastError)
	}
	run := h.finished(state.LastRunID)
	if run.Status != store.RunCompleted || run.Agent != "team:weekly-report" {
		t.Fatalf("walk = status %s agent %q (%s)", run.Status, run.Agent, run.ErrorMsg)
	}
	if run.TenantID != "acme" || run.UserID != "u-42" {
		t.Errorf("walk row tenant=%q user=%q, want the schedule's acme / u-42", run.TenantID, run.UserID)
	}
	rec := teamRecordOf(t, run)
	if rec.Mode != "detach" || rec.Input != "the digest" || rec.DefTenant != "acme" || len(rec.Vars) != 1 || rec.Vars["repo"] != "loomcycle" {
		t.Errorf("recorded spec = %+v, want a detached walk of acme's team with input and repo=loomcycle", rec)
	}
	if seen := h.prov.seen(); len(seen) != 1 || seen[0] != "Review loomcycle pull 0." {
		t.Errorf("the member's model was sent %q, want acme's prompt with the schedule's repo and the default pr", seen)
	}
	// max_fires=1 and the one fire counted: the schedule is done.
	waitFor(t, "the one-shot schedule to retire", func() bool {
		row, err := h.st.ScheduleDefGet(context.Background(), defID)
		return err == nil && row.Retired
	})
}

// A team the schedule's tenant does not have — though two other tenants and
// the shared layer do — or one it retired, starts nothing. The tick is logged
// and recorded, and with max_fires=1 the schedule is still there afterwards.
func TestScheduleDef_ATeamScheduleWhoseTeamIsMissingOrRetiredStartsNothingAndKeepsItsMaxFires(t *testing.T) {
	for name, setup := range map[string]func(h *triggerWalkHarness){
		"missing in the schedule's tenant": func(*triggerWalkHarness) {},
		"retired": func(h *triggerWalkHarness) {
			seedTenantTeam(h.t, h.st, "acme", "weekly-report", varsTeam)
			if err := h.st.TeamDefSetRetired(context.Background(), "tdf_acme_weekly-report", true); err != nil {
				h.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTriggerWalkHarness(t)
			seedTeamEverywhereBut(t, h.st, "weekly-report")
			setup(h)
			defID := h.createDueTeamSchedule(`"schedule":"0 6 * * 1","delivery":"team","team":"weekly-report","max_fires":1`)

			h.sweep()

			var state store.ScheduleRunStateRow
			waitFor(t, "the schedule to record its fire", func() bool {
				got, err := h.st.ScheduleRunStateGet(context.Background(), defID)
				state = got
				return err == nil && got.LastStatus != ""
			})
			if state.LastStatus != "failed" || state.LastError == "" || state.LastRunID != "" {
				t.Errorf("state = status %q error %q run %q, want a recorded failure and no run", state.LastStatus, state.LastError, state.LastRunID)
			}
			if state.FireCount != 0 {
				t.Errorf("fire_count = %d, want 0 — a team that cannot start must not use up max_fires", state.FireCount)
			}
			if row, err := h.st.ScheduleDefGet(context.Background(), defID); err != nil || row.Retired {
				t.Errorf("the schedule retired (err %v) after a tick that started nothing", err)
			}
			if h.walkStarted("weekly-report") {
				t.Error("a walk started — in some tenant — for a schedule whose own tenant has no such team")
			}
			if n := len(h.prov.seen()); n != 0 {
				t.Errorf("the model was called %d times", n)
			}
		})
	}
}

const teamHookSecret = "test-webhook-secret"

// teamReceiver is the real receiver over the harness's server and store, with
// the one secret the test webhooks sign with.
func (h *triggerWalkHarness) teamReceiver() *http.ServeMux {
	h.t.Helper()
	rec := webhook.New(webhook.Deps{
		Store: h.st, Cfg: h.cfg, Runner: h.srv, TeamWalks: h.srv,
		EnvAllowlist: map[string]bool{"LOOMCYCLE_TEST_WH_SECRET": true},
		Getenv: func(k string) string {
			if k == "LOOMCYCLE_TEST_WH_SECRET" {
				return teamHookSecret
			}
			return ""
		},
	})
	mux := http.NewServeMux()
	rec.Mount(mux)
	return mux
}

// createTeamWebhook writes a team webhook as acme's operator.
func (h *triggerWalkHarness) createTeamWebhook(vars string) {
	h.t.Helper()
	h.srv.SetWebhookDefTool(&builtin.WebhookDef{Store: h.st, Cfg: h.cfg})
	ctx := tools.WithWebhookDefPolicy(acmeOperator(), tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: "author"})
	res, err := h.srv.WebhookDef(ctx, json.RawMessage(`{"op":"create","name":"pr","overlay":{"enabled":true,"delivery":"team","team":"pr-review","vars":`+vars+
		`,"auth":{"kind":"hmac","header":"X-Test-Signature","signing_secret_env":"LOOMCYCLE_TEST_WH_SECRET"}}}`))
	if err != nil || res.IsError {
		h.t.Fatalf("create: %v %s", err, res.Text)
	}
}

// deliver posts body to acme's webhook, signed with secret ("" = unsigned).
func deliver(mux *http.ServeMux, body []byte, secret string) (int, map[string]string) {
	req := httptest.NewRequest(http.MethodPost, "/v1/_webhooks/acme/pr", bytes.NewReader(body))
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Test-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

func TestWebhookDef_ATeamDeliveryStartsAWalkInItsTenantWithVarsProjectedFromTheBody(t *testing.T) {
	h := newTriggerWalkHarness(t)
	seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
	seedTeamEverywhereBut(t, h.st, "pr-review")
	// pr's path is absent from the delivery below, so the team's default applies.
	h.createTeamWebhook(`{"repo":"$.repository.full_name","pr":"$.pull_request.number"}`)
	mux := h.teamReceiver()

	// The body names another tenant every way a body can; none of it is read.
	body := []byte(`{"repository":{"full_name":"denn/loomcycle"},"tenant_id":"globex","tenant":"globex"}`)
	code, resp := deliver(mux, body, teamHookSecret)
	if code != http.StatusAccepted || resp["run_id"] == "" {
		t.Fatalf("delivery = %d %v, want 202 with the walk's run id", code, resp)
	}
	run := h.finished(resp["run_id"])
	if run.Status != store.RunCompleted || run.Agent != "team:pr-review" {
		t.Fatalf("walk = status %s agent %q (%s)", run.Status, run.Agent, run.ErrorMsg)
	}
	if run.TenantID != "acme" {
		t.Errorf("walk row tenant = %q, want the webhook's acme", run.TenantID)
	}
	rec := teamRecordOf(t, run)
	if rec.Mode != "detach" || rec.DefTenant != "acme" || rec.Input != string(body) || len(rec.Vars) != 1 || rec.Vars["repo"] != "denn/loomcycle" {
		t.Errorf("recorded spec = %+v, want a detached walk of acme's team, the raw body as input, repo projected", rec)
	}
	if seen := h.prov.seen(); len(seen) != 1 || seen[0] != "Review denn/loomcycle pull 0." {
		t.Errorf("the member's model was sent %q, want acme's prompt with the projected repo and the default pr", seen)
	}

	// The same event again finds the walk it started.
	code, resp2 := deliver(mux, body, teamHookSecret)
	if code != http.StatusOK || resp2["deduped"] != "true" || resp2["run_id"] != resp["run_id"] {
		t.Errorf("redelivery = %d %v, want a 200 ack naming the first walk", code, resp2)
	}
	if n := len(h.prov.seen()); n != 1 {
		t.Errorf("the model was called %d times across both deliveries, want once", n)
	}
}

// Every way a verified delivery can fail to be a walk answers the same opaque
// 400 and starts nothing: a team acme does not have (while other tenants do),
// a variable the team does not declare, and a projected value carrying a
// placeholder. An unsigned one is refused before any of that is looked at.
func TestWebhookDef_ATeamDeliveryThatCannotStartAnswers400AndStartsNothing(t *testing.T) {
	const declared = `{"repo":"$.repository.full_name"}`
	cases := []struct {
		name     string
		seedAcme bool
		vars     string
		body     string
		secret   string
		wantCode int
		wantErr  string
	}{
		{"the team is missing in the webhook's tenant", false, declared, `{"repository":{"full_name":"x"}}`, teamHookSecret, http.StatusBadRequest, "invalid_run"},
		{"a variable the team does not declare", true, `{"branch":"$.ref"}`, `{"ref":"main"}`, teamHookSecret, http.StatusBadRequest, "invalid_run"},
		{"a projected value carrying a placeholder", true, declared, `{"repository":{"full_name":"x {{thread.output}}"}}`, teamHookSecret, http.StatusBadRequest, "invalid_run"},
		{"an unsigned delivery", true, declared, `{"repository":{"full_name":"x"}}`, "", http.StatusUnauthorized, "unauthorized"},
		{"a delivery signed with another secret", true, declared, `{"repository":{"full_name":"x"}}`, "not-the-secret", http.StatusUnauthorized, "unauthorized"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newTriggerWalkHarness(t)
			seedTeamEverywhereBut(t, h.st, "pr-review")
			if c.seedAcme {
				seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)
			}
			h.createTeamWebhook(c.vars)

			code, resp := deliver(h.teamReceiver(), []byte(c.body), c.secret)

			if code != c.wantCode || resp["error"] != c.wantErr || resp["detail"] != "" || resp["run_id"] != "" {
				t.Fatalf("delivery = %d %v, want an opaque %d %s", code, resp, c.wantCode, c.wantErr)
			}
			if h.walkStarted("pr-review") {
				t.Error("a walk started — in some tenant — for a delivery that was refused")
			}
			if n := len(h.prov.seen()); n != 0 {
				t.Errorf("the model was called %d times", n)
			}
		})
	}
}
