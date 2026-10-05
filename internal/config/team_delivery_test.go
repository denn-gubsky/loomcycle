package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A yaml schedule that starts a team walk needs a team and a cadence, and
// carries its variables as literals.
func TestScheduleDelivery_TeamTickLoadsWithVarsAndInput(t *testing.T) {
	cfg, err := loadScheduleYAML(t, `
  weekly:
    delivery: team
    team: weekly-report
    vars: { repo: loomcycle, user: u-42 }
    input: "the week's digest"
    schedule: "0 6 * * 1"
    enabled: true
`)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	got := cfg.ScheduledRuns["weekly"]
	if got.Delivery != "team" || got.Team != "weekly-report" || got.Input != "the week's digest" {
		t.Errorf("delivery/team/input did not load: %+v", got)
	}
	if got.Vars["repo"] != "loomcycle" || got.Vars["user"] != "u-42" {
		t.Errorf("vars did not load: %v", got.Vars)
	}
}

// Every field that configures an agent run is refused on a team tick, as on a
// channel tick: nothing would read it.
func TestScheduleDelivery_TeamTickRefusesRunShapedFields(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"agent", "    agent: worker\n", "forbids `agent`"},
		{"channel", "    channel: wave-in\n", "forbids `channel`"},
		{"prompt", "    prompt: [{role: user, content: [{type: trusted-text, text: go}]}]\n", "forbids `prompt`"},
		{"on_complete", "    on_complete: [{kind: memory.set, scope: agent, key: k}]\n", "forbids `on_complete`"},
		{"required_credentials", "    required_credentials: [jobs]\n", "forbids credentials"},
		{"user_credentials_from_env", "    user_credentials_from_env: {jobs: LOOMCYCLE_JOBS_TOKEN}\n", "forbids credentials"},
		{"metadata", "    metadata: {batch: nightly}\n", "forbids `metadata`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadScheduleYAML(t, "  tick:\n    delivery: team\n    team: weekly-report\n    schedule: \"0 3 * * *\"\n"+c.extra)
			if err == nil {
				t.Fatalf("delivery=team with %s loaded; want a refusal", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// What a team tick can be held to when it is written: the team's name, each
// variable's name, and each literal value.
func TestScheduleDelivery_TeamTickChecksTargetAndVars(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no team", "    delivery: team\n", "requires team"},
		{"team name with a slash", "    delivery: team\n    team: ops/weekly\n", "invalid character"},
		{"variable name with a dot", "    delivery: team\n    team: weekly\n    vars: {\"a.b\": x}\n", "must match"},
		{"value carrying a placeholder", "    delivery: team\n    team: weekly\n    vars: {repo: \"{{thread.output}}\"}\n", "{{ or }}"},
		{"value over the size bound", "    delivery: team\n    team: weekly\n    vars: {repo: \"" + strings.Repeat("x", 4097) + "\"}\n", "more than the maximum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadScheduleYAML(t, "  tick:\n    schedule: \"0 3 * * *\"\n"+c.body)
			if err == nil {
				t.Fatalf("%s loaded; want a refusal", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// team, vars and input belong to a team tick only: on a run or a channel tick
// they would be settings nothing reads.
func TestScheduleDelivery_TeamFieldsAreRefusedOnOtherDeliveries(t *testing.T) {
	cases := []struct{ name, body string }{
		{"team on a run", "    agent: worker\n    team: weekly\n"},
		{"vars on a run", "    agent: worker\n    vars: {repo: x}\n"},
		{"input on a run", "    agent: worker\n    input: hello\n"},
		{"team on a channel tick", "    delivery: channel\n    channel: wave-in\n    team: weekly\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadScheduleYAML(t, "  tick:\n    schedule: \"0 3 * * *\"\n"+c.body)
			if err == nil || !strings.Contains(err.Error(), "forbids `team`, `vars` and `input`") {
				t.Fatalf("error = %v, want a refusal of the team fields", err)
			}
		})
	}
}

func TestValidateStaticWebhook_TeamDelivery(t *testing.T) {
	team := func(mut func(*Webhook)) Webhook {
		w := Webhook{Delivery: "team", Team: "pr-review", Vars: map[string]string{"repo": "$.repository.full_name", "pr": "$.pull_request.number"}}
		if mut != nil {
			mut(&w)
		}
		return w
	}
	cases := []struct {
		name    string
		wh      Webhook
		wantErr string // substring; "" = no error
	}{
		{"team ok", team(nil), ""},
		{"team with a user_id mapping", team(func(w *Webhook) { w.PayloadMapping = map[string]string{"user_id": "$.sender.login"} }), ""},
		{"no team", Webhook{Delivery: "team"}, "requires team"},
		{"team name with a colon", team(func(w *Webhook) { w.Team = "a:b" }), "invalid character"},
		{"variable name with a dot", team(func(w *Webhook) { w.Vars = map[string]string{"a.b": "$.x"} }), "must match"},
		{"value that is not a JSONPath", team(func(w *Webhook) { w.Vars = map[string]string{"repo": "repository.full_name"} }), "not a supported JSONPath"},
		{"value with a wildcard", team(func(w *Webhook) { w.Vars = map[string]string{"repo": "$.items[*].name"} }), "not a supported JSONPath"},
		{"agent", team(func(w *Webhook) { w.Agent = "a" }), "forbids `agent`"},
		{"channel", team(func(w *Webhook) { w.Channel = "c" }), "forbids `channel`"},
		{"user_tier", team(func(w *Webhook) { w.UserTier = "high" }), "forbids `user_tier`"},
		{"user_credentials", team(func(w *Webhook) { w.UserCredentials = map[string]string{"k": "v"} }), "forbids credentials"},
		{"user_credentials_from_env", team(func(w *Webhook) { w.UserCredentialsFromEnv = map[string]string{"k": "LOOMCYCLE_K"} }), "forbids credentials"},
		{"metadata", team(func(w *Webhook) { w.Metadata = map[string]any{"k": "v"} }), "forbids `metadata`"},
		{"on_complete", team(func(w *Webhook) { w.OnComplete = []ScheduledRunHook{{Kind: "memory.set", Scope: "agent", Key: "k"}} }), "forbids `on_complete`"},
		{"sync_response", team(func(w *Webhook) { w.SyncResponse = WebhookSyncResponse{Enabled: true, TimeoutMs: 1000} }), "forbids `sync_response`"},
		{"payload_mapping goal", team(func(w *Webhook) { w.PayloadMapping = map[string]string{"goal": "$.title"} }), `forbids payload_mapping target "goal"`},
		{"payload_mapping credential", team(func(w *Webhook) { w.PayloadMapping = map[string]string{"user_credentials.gh": "$.token"} }), "forbids payload_mapping target"},
		{"team on a spawn", Webhook{Delivery: "spawn", Agent: "a", Team: "pr-review"}, "forbids `team` and `vars`"},
		{"vars on a channel", Webhook{Delivery: "channel", Channel: "c", Vars: map[string]string{"a": "$.a"}}, "forbids `team` and `vars`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStaticWebhook("wh", tc.wh)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
			}
		})
	}
}

// The yaml keys reach the struct, and the validator is wired into Load.
func TestLoad_WebhookTeamDelivery(t *testing.T) {
	load := func(t *testing.T, webhook string) (*Config, error) {
		t.Helper()
		yamlPath := filepath.Join(t.TempDir(), "c.yaml")
		body := "defaults: { provider: anthropic, model: x }\nwebhooks:\n" + webhook
		if err := os.WriteFile(yamlPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(yamlPath)
	}
	cfg, err := load(t, `
  pr:
    enabled: true
    delivery: team
    team: pr-review
    vars: { repo: "$.repository.full_name", pr: "$.pull_request.number" }
    auth: { kind: hmac, signing_secret_env: LOOMCYCLE_PR_SECRET }
`)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	got := cfg.Webhooks["pr"]
	if got.Delivery != "team" || got.Team != "pr-review" || got.Vars["pr"] != "$.pull_request.number" {
		t.Errorf("delivery/team/vars did not load: %+v", got)
	}

	if _, err := load(t, `
  pr:
    enabled: true
    delivery: team
    team: pr-review
    sync_response: { enabled: true, timeout_ms: 1000 }
    auth: { kind: hmac, signing_secret_env: LOOMCYCLE_PR_SECRET }
`); err == nil || !strings.Contains(err.Error(), "forbids `sync_response`") {
		t.Fatalf("Load err = %v, want a sync_response refusal", err)
	}
}
