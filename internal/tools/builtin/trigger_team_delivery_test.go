package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A schedule and a webhook may deliver to a TEAM: the tick or the delivery
// starts a detached walk. These tests cover what the def tools decide — what a
// team delivery may carry, and that it survives create → get → fork → the
// read shapes the scheduler and the receiver use. Firing it is tested where it
// fires.

const teamScheduleOverlay = `"delivery":"team","team":"weekly-report","schedule":"0 6 * * 1","vars":{"repo":"loomcycle","user":"u-42"},"input":"the week's digest"`

const teamWebhookOverlay = `"enabled":true,"delivery":"team","team":"pr-review","vars":{"repo":"$.repository.full_name","pr":"$.pull_request.number"},"auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`

// storedDefinition returns the definition object of a create/fork/get result.
func storedDefinition(t *testing.T, tool tools.Tool, ctx context.Context, res tools.Result) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("write: %s", res.Text)
	}
	defID, _ := decodeResult(t, res.Text)["def_id"].(string)
	got, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+defID+`"}`))
	if got.IsError {
		t.Fatalf("get: %s", got.Text)
	}
	def, ok := decodeResult(t, got.Text)["definition"].(map[string]any)
	if !ok {
		t.Fatalf("no definition in %s", got.Text)
	}
	return def
}

func TestScheduleDefTool_CreateTeamDeliveryRoundTrips(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"weekly","overlay":{`+teamScheduleOverlay+`}}`))
	def := storedDefinition(t, tool, ctx, res)
	if def["delivery"] != "team" || def["team"] != "weekly-report" || def["input"] != "the week's digest" {
		t.Errorf("delivery/team/input did not round-trip: %v", def)
	}
	vars, _ := def["vars"].(map[string]any)
	if vars["repo"] != "loomcycle" || vars["user"] != "u-42" {
		t.Errorf("vars did not round-trip: %v", def["vars"])
	}
	if _, present := def["agent"]; present {
		t.Errorf("a team tick stored an agent: %v", def)
	}

	// A fork restating nothing keeps the target; one that sends vars replaces
	// the whole set.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"weekly","overlay":{"vars":{"repo":"other"}}}`))
	def = storedDefinition(t, tool, ctx, res)
	if def["delivery"] != "team" || def["team"] != "weekly-report" || def["input"] != "the week's digest" {
		t.Errorf("a fork lost the team target: %v", def)
	}
	vars, _ = def["vars"].(map[string]any)
	if len(vars) != 1 || vars["repo"] != "other" {
		t.Errorf("fork vars = %v, want only repo=other", def["vars"])
	}

	// The read shape the lookup hands the runtime carries the same target.
	sr, ok := lookup.Schedule(ctx, tool.Store, tool.Cfg, "", "weekly")
	if !ok {
		t.Fatal("lookup.Schedule(weekly): not found")
	}
	if sr.Delivery != "team" || sr.Team != "weekly-report" || sr.Vars["repo"] != "other" || sr.Input != "the week's digest" {
		t.Errorf("resolved schedule = %+v", sr)
	}
}

func TestScheduleDefTool_TeamDeliveryRefusesRunShapedFieldsAndBadTargets(t *testing.T) {
	const base = `"delivery":"team","team":"weekly-report","schedule":"0 6 * * 1"`
	cases := []struct {
		name    string
		overlay string
		want    string
	}{
		{"agent", base + `,"agent":"job-search-batch"`, "forbids agent"},
		{"channel", base + `,"channel":"c"`, "forbids channel"},
		{"prompt", base + `,"prompt":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]`, "forbids prompt"},
		{"on_complete", base + `,"on_complete":[{"kind":"memory.set","scope":"agent","key":"k"}]`, "forbids on_complete"},
		{"user_credentials", base + `,"user_credentials":{"jobs":"t"}`, "forbids credentials"},
		{"user_credentials_from_env", base + `,"user_credentials_from_env":{"jobs":"LOOMCYCLE_J"}`, "forbids credentials"},
		{"required_credentials", base + `,"required_credentials":["jobs"]`, "forbids credentials"},
		{"metadata", base + `,"metadata":{"batch":"nightly"}`, "forbids metadata"},
		{"no team", `"delivery":"team","schedule":"0 6 * * 1"`, "requires team"},
		{"team name with a slash", `"delivery":"team","team":"ops/weekly","schedule":"0 6 * * 1"`, "invalid character"},
		{"variable name with a dot", base + `,"vars":{"a.b":"x"}`, "must match"},
		{"value carrying a placeholder", base + `,"vars":{"repo":"{{thread.output}}"}`, "{{ or }}"},
		{"team on a run", `"agent":"job-search-batch","schedule":"0 6 * * 1","team":"weekly-report"`, "forbids team, vars and input"},
		{"vars on a channel tick", `"delivery":"channel","channel":"c","schedule":"0 6 * * 1","vars":{"a":"b"}`, "forbids team, vars and input"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, ctx, cleanup := scheduleDefFixture(t)
			defer cleanup()
			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-tick","overlay":{`+c.overlay+`}}`))
			if !res.IsError {
				t.Fatalf("create with %s succeeded; want a refusal", c.name)
			}
			if !strings.Contains(res.Text, c.want) {
				t.Errorf("refusal = %q, want it to mention %q", res.Text, c.want)
			}
		})
	}
}

// A yaml schedule with a team delivery is materialized at boot like any other,
// and its target reaches the stored definition the sweeper reads.
func TestScheduleDefTool_BootstrapStaticSchedules_TeamDelivery(t *testing.T) {
	tool, _, cleanup := scheduleDefFixture(t)
	defer cleanup()
	tool.Cfg.ScheduledRuns = map[string]config.ScheduledRun{
		"weekly": {
			Delivery: "team", Team: "weekly-report", Schedule: "0 6 * * 1", Enabled: true,
			Vars: map[string]string{"repo": "loomcycle"}, Input: "digest",
		},
	}
	bg := context.Background()
	if n, err := tool.BootstrapStaticSchedules(bg); err != nil || n != 1 {
		t.Fatalf("bootstrap = %d, %v; want 1 materialized", n, err)
	}
	row, err := tool.Store.ScheduleDefGetActive(bg, "", "weekly")
	if err != nil {
		t.Fatalf("no active version after bootstrap: %v", err)
	}
	var def mergedScheduleDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Delivery != "team" || def.Team != "weekly-report" || def.Vars["repo"] != "loomcycle" || def.Input != "digest" {
		t.Errorf("bootstrapped definition = %s", row.Definition)
	}
	if err := validateScheduleDef(def); err != nil {
		t.Errorf("the bootstrapped definition does not pass the tool's own validation: %v", err)
	}
	// A second boot finds the same content and re-materializes nothing.
	if n, err := tool.BootstrapStaticSchedules(bg); err != nil || n != 0 {
		t.Errorf("second bootstrap = %d, %v; want 0 (the content is unchanged)", n, err)
	}
}

func TestWebhookDefTool_CreateTeamDeliveryRoundTrips(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"pr","overlay":{`+teamWebhookOverlay+`}}`))
	def := storedDefinition(t, tool, ctx, res)
	if def["delivery"] != "team" || def["team"] != "pr-review" {
		t.Errorf("delivery/team did not round-trip: %v", def)
	}
	vars, _ := def["vars"].(map[string]any)
	if vars["repo"] != "$.repository.full_name" || vars["pr"] != "$.pull_request.number" {
		t.Errorf("vars did not round-trip: %v", def["vars"])
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"pr","overlay":{"vars":{"repo":"$.repo"}}}`))
	def = storedDefinition(t, tool, ctx, res)
	vars, _ = def["vars"].(map[string]any)
	if def["team"] != "pr-review" || len(vars) != 1 || vars["repo"] != "$.repo" {
		t.Errorf("fork = %v, want the team kept and only repo=$.repo", def)
	}

	// The shape the receiver resolves carries the same target.
	wd, ok := lookup.Webhook(ctx, tool.Store, tool.Cfg, "", "pr")
	if !ok {
		t.Fatal("lookup.Webhook(pr): not found")
	}
	if wd.Delivery != "team" || wd.Team != "pr-review" || wd.Vars["repo"] != "$.repo" {
		t.Errorf("resolved webhook = %+v", wd)
	}
}

func TestWebhookDefTool_TeamDeliveryRefusesRunShapedFieldsAndBadTargets(t *testing.T) {
	const base = `"delivery":"team","team":"pr-review","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`
	cases := []struct {
		name    string
		overlay string
		want    string
	}{
		{"agent", base + `,"agent":"intake"`, "forbids `agent`"},
		{"channel", base + `,"channel":"c"`, "forbids `channel`"},
		{"user_credentials", base + `,"user_credentials":{"gh":"t"}`, "forbids credentials"},
		{"user_credentials_from_env", base + `,"user_credentials_from_env":{"gh":"LOOMCYCLE_GH"}`, "forbids credentials"},
		{"metadata", base + `,"metadata":{"k":"v"}`, "forbids `metadata`"},
		{"on_complete", base + `,"on_complete":[{"kind":"memory.set","scope":"agent","key":"k"}]`, "forbids `on_complete`"},
		{"sync_response", base + `,"sync_response":{"enabled":true,"timeout_ms":1000}`, "forbids `sync_response`"},
		{"payload_mapping goal", base + `,"payload_mapping":{"goal":"$.title"}`, `forbids payload_mapping target "goal"`},
		{"payload_mapping credential", base + `,"payload_mapping":{"user_credentials.gh":"$.token"}`, "forbids payload_mapping target"},
		{"no team", `"delivery":"team","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`, "requires team"},
		{"team name with a colon", `"delivery":"team","team":"a:b","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`, "invalid character"},
		{"variable name with a dot", base + `,"vars":{"a.b":"$.x"}`, "must match"},
		{"value that is not a JSONPath", base + `,"vars":{"repo":"repository.full_name"}`, "not a supported JSONPath"},
		{"value with a wildcard", base + `,"vars":{"repo":"$.items[*].name"}`, "not a supported JSONPath"},
		{"vars on a spawn", `"delivery":"spawn","agent":"intake","vars":{"a":"$.a"},"auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`, "forbids team and vars"},
		{"vars on a channel", `"delivery":"channel","channel":"c","vars":{"a":"$.a"},"auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`, "forbids team and vars"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, ctx, cleanup := webhookDefFixture(t)
			defer cleanup()
			res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad-hook","overlay":{`+c.overlay+`}}`))
			if !res.IsError {
				t.Fatalf("create with %s succeeded; want a refusal", c.name)
			}
			if !strings.Contains(res.Text, c.want) {
				t.Errorf("refusal = %q, want it to mention %q", res.Text, c.want)
			}
		})
	}
}

// A user_id mapping is the one payload_mapping target a walk has a use for.
func TestWebhookDefTool_TeamDeliveryAcceptsUserIDMapping(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"pr","overlay":{`+teamWebhookOverlay+`,"payload_mapping":{"user_id":"$.sender.login"}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
}

// A fork that names one delivery target clears the others, so a webhook can be
// flipped between an agent run and a team walk without a stale field tripping
// the validator.
func TestWebhookDefTool_ForkFlipsBetweenSpawnAndTeam(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"flip","overlay":{"enabled":true,"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}}}`)); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"flip","overlay":{"delivery":"team","team":"pr-review","vars":{"pr":"$.number"}}}`))
	def := storedDefinition(t, tool, ctx, res)
	if _, present := def["agent"]; present || def["team"] != "pr-review" {
		t.Errorf("spawn → team fork = %v, want the agent cleared and the team set", def)
	}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"flip","overlay":{"delivery":"spawn","agent":"intake"}}`))
	def = storedDefinition(t, tool, ctx, res)
	_, hasTeam := def["team"]
	_, hasVars := def["vars"]
	if hasTeam || hasVars || def["agent"] != "intake" {
		t.Errorf("team → spawn fork = %v, want team and vars cleared and the agent set", def)
	}
}

func TestWebhookDefTool_BootstrapStaticWebhooks_TeamDelivery(t *testing.T) {
	tool, _, cleanup := webhookDefFixture(t)
	defer cleanup()
	tool.Cfg.Webhooks = map[string]config.Webhook{
		"pr": {
			Enabled: true, Delivery: "team", Team: "pr-review",
			Vars: map[string]string{"repo": "$.repository.full_name"},
			Auth: config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "LOOMCYCLE_S"},
		},
	}
	bg := context.Background()
	if n, err := tool.BootstrapStaticWebhooks(bg); err != nil || n != 1 {
		t.Fatalf("bootstrap = %d, %v; want 1 materialized", n, err)
	}
	row, err := tool.Store.WebhookDefGetActive(bg, "", "pr")
	if err != nil {
		t.Fatalf("no active version after bootstrap: %v", err)
	}
	if err := ValidateWebhookDefBody(row.Definition); err != nil {
		t.Errorf("the bootstrapped definition does not pass the tool's own validation: %v", err)
	}
	var def mergedWebhookDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Delivery != "team" || def.Team != "pr-review" || def.Vars["repo"] != "$.repository.full_name" {
		t.Errorf("bootstrapped definition = %s", row.Definition)
	}
}

// The execution-tenant guard is on the def, not on a delivery: a tenant
// operator's team trigger may not name another tenant either, so a walk it
// starts can only run — and look its team up — in the author's own tenant.
func TestTriggerDefs_NonAdminTeamDeliveryNamingAnotherTenantIsRefusedAndStoresNothing(t *testing.T) {
	for _, h := range triggerHarnesses(t) {
		h.overlay = teamWebhookOverlay
		if h.kind == "ScheduleDef" {
			h.overlay = teamScheduleOverlay
		}
		requireRefusedAsValidation(t, h.kind, h.call(h.as("acme", false), "create", "grab", "globex"))
		if got, ok := h.execTenant(t, "acme", "grab"); ok {
			t.Errorf("%s: a refused create stored a version executing in %q", h.kind, got)
		}
		// Left out, the execution tenant is the author's own.
		if res := h.call(h.as("acme", false), "create", "mine", ""); res.IsError {
			t.Fatalf("%s: create: %s", h.kind, res.Text)
		}
		if got, _ := h.execTenant(t, "acme", "mine"); got != "acme" {
			t.Errorf("%s: a team trigger with no tenant_id executes in %q, want acme", h.kind, got)
		}
	}
}

// The schemas are hand-written JSON inside Go strings, and the team examples
// in them carry escaped quotes.
func TestTriggerDefs_InputSchemasAreValidJSON(t *testing.T) {
	for name, schema := range map[string]string{"ScheduleDef": scheduleDefInputSchema, "WebhookDef": webhookDefInputSchema} {
		if !json.Valid([]byte(schema)) {
			t.Errorf("%s input schema is not valid JSON", name)
		}
	}
}
