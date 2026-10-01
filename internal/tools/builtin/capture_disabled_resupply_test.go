package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A def restored without its literal credentials is re-enabled by a fork (or
// a create on the same name) that re-supplies every stripped key. These tests
// pin that a re-supply counts only when the fire path will use it, that the
// result says what is still missing and why, and that re-supplying the
// stripped keys of a webhook keeps the parent's other credentials.

// allowEnvCredential puts name on the scheduler allowlist — which the webhook
// receiver's allowlist includes too — and sets it, so a user_credentials_from_env
// re-supply of it is one both fire paths resolve.
func allowEnvCredential(t *testing.T, cfg *config.Config, name string) {
	t.Helper()
	cfg.Env.SchedulerEnvAllowlist = append(cfg.Env.SchedulerEnvAllowlist, name)
	t.Setenv(name, "value-set-by-test")
}

// execResult runs one op and returns its decoded result.
func execResult(t *testing.T, exec func(context.Context, json.RawMessage) (resultText string, isErr bool), ctx context.Context, input string) map[string]any {
	t.Helper()
	text, isErr := exec(ctx, json.RawMessage(input))
	if isErr {
		t.Fatalf("%s: %s", input, text)
	}
	return decodeResult(t, text)
}

func scheduleExec(tool *ScheduleDef) func(context.Context, json.RawMessage) (string, bool) {
	return func(ctx context.Context, raw json.RawMessage) (string, bool) {
		res, _ := tool.Execute(ctx, raw)
		return res.Text, res.IsError
	}
}

func webhookExec(tool *WebhookDef) func(context.Context, json.RawMessage) (string, bool) {
	return func(ctx context.Context, raw json.RawMessage) (string, bool) {
		res, _ := tool.Execute(ctx, raw)
		return res.Text, res.IsError
	}
}

// resultList reads a []string result key ([]any after JSON decoding).
func resultList(out map[string]any, key string) []string {
	raw, _ := out[key].([]any)
	var got []string
	for _, v := range raw {
		s, _ := v.(string)
		got = append(got, s)
	}
	return got
}

// storedSchedule decodes the body and fire count of the def a result names.
func storedSchedule(t *testing.T, tool *ScheduleDef, out map[string]any) (mergedScheduleDef, int) {
	t.Helper()
	defID, _ := out["def_id"].(string)
	row, err := tool.Store.ScheduleDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get %s: %v", defID, err)
	}
	var def mergedScheduleDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	st, err := tool.Store.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("%s has no run state: %v", defID, err)
	}
	return def, st.FireCount
}

// storedWebhook decodes the body of the def a result names.
func storedWebhook(t *testing.T, tool *WebhookDef, out map[string]any) mergedWebhookDef {
	t.Helper()
	defID, _ := out["def_id"].(string)
	row, err := tool.Store.WebhookDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get %s: %v", defID, err)
	}
	var def mergedWebhookDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	return def
}

// A fork that re-supplies only the keys a snapshot stripped keeps every other
// credential the parent held — its $cred: reference and its other env source —
// in the stored body and in what the receiver resolves.
func TestWebhookDefTool_ForkReSupplyKeepsParentCredentials(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	allowEnvCredential(t, tool.Cfg, "LOOMCYCLE_SLACK")
	body, err := json.Marshal(map[string]any{
		"delivery": "spawn", "agent": "intake", "enabled": false,
		"auth":                      map[string]any{"kind": "hmac", "signing_secret_env": "LOOMCYCLE_WH_SECRET"},
		"user_credentials":          map[string]string{"gh": "$cred:gh"},
		"user_credentials_from_env": map[string]string{"telegram": "LOOMCYCLE_TG"},
		"capture_disabled":          map[string]any{"stripped_credentials": []string{"jobs", "slack"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bg := context.Background()
	if _, err := tool.Store.SnapshotRestoreWebhookDef(bg, store.WebhookDefRow{DefID: "wh_p", Name: "restored", Version: 1, Definition: body, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Store.SnapshotRestoreWebhookDefActive(bg, store.WebhookDefActiveEntry{Name: "restored", DefID: "wh_p"}); err != nil {
		t.Fatal(err)
	}

	out := execResult(t, webhookExec(tool), ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,
		"user_credentials":{"jobs":"j"},"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}}}`)
	wantUC := map[string]string{"gh": "$cred:gh", "jobs": "j"}
	wantEnv := map[string]string{"telegram": "LOOMCYCLE_TG", "slack": "LOOMCYCLE_SLACK"}

	def := storedWebhook(t, tool, out)
	if def.CaptureDisabled != nil || !def.Enabled {
		t.Fatalf("fork marker=%+v enabled=%v, want cleared and enabled", def.CaptureDisabled, def.Enabled)
	}
	if !reflect.DeepEqual(def.UserCredentials, wantUC) || !reflect.DeepEqual(def.UserCredentialsFromEnv, wantEnv) {
		t.Errorf("stored credentials = %v / %v, want %v / %v", def.UserCredentials, def.UserCredentialsFromEnv, wantUC, wantEnv)
	}
	w := resolvedWebhook(t, tool, "restored")
	if !reflect.DeepEqual(w.UserCredentials, wantUC) || !reflect.DeepEqual(w.UserCredentialsFromEnv, wantEnv) {
		t.Errorf("receiver credentials = %v / %v, want %v / %v", w.UserCredentials, w.UserCredentialsFromEnv, wantUC, wantEnv)
	}
}

// Any webhook fork — marked or not — merges the credential maps key by key, as
// a ScheduleDef fork does; an empty value removes a key.
func TestWebhookDefTool_ForkMergesCredentialMapsKeyByKey(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	exec := webhookExec(tool)
	execResult(t, exec, ctx, `{"op":"create","name":"plain","overlay":{"enabled":true,"delivery":"spawn","agent":"intake",
		"auth":{"signing_secret_env":"LOOMCYCLE_WH_SECRET"},
		"user_credentials":{"x":"1"},"user_credentials_from_env":{"a":"LOOMCYCLE_A"}}}`)

	def := storedWebhook(t, tool, execResult(t, exec, ctx, `{"op":"fork","name":"plain","overlay":{
		"user_credentials":{"y":"2"},"user_credentials_from_env":{"b":"LOOMCYCLE_B"}}}`))
	if want := map[string]string{"x": "1", "y": "2"}; !reflect.DeepEqual(def.UserCredentials, want) {
		t.Errorf("user_credentials = %v, want %v", def.UserCredentials, want)
	}
	if want := map[string]string{"a": "LOOMCYCLE_A", "b": "LOOMCYCLE_B"}; !reflect.DeepEqual(def.UserCredentialsFromEnv, want) {
		t.Errorf("user_credentials_from_env = %v, want %v", def.UserCredentialsFromEnv, want)
	}

	def = storedWebhook(t, tool, execResult(t, exec, ctx, `{"op":"fork","name":"plain","overlay":{
		"user_credentials":{"x":""},"user_credentials_from_env":{"a":""}}}`))
	if want := map[string]string{"y": "2"}; !reflect.DeepEqual(def.UserCredentials, want) {
		t.Errorf("after removing x: user_credentials = %v, want %v", def.UserCredentials, want)
	}
	if want := map[string]string{"b": "LOOMCYCLE_B"}; !reflect.DeepEqual(def.UserCredentialsFromEnv, want) {
		t.Errorf("after removing a: user_credentials_from_env = %v, want %v", def.UserCredentialsFromEnv, want)
	}
}

// resupplyCase is one way of re-supplying the stripped key "slack".
type resupplyCase struct {
	name        string
	allowlisted bool   // LOOMCYCLE_SLACK on the allowlist
	envValue    string // LOOMCYCLE_SLACK's value; "" = unset
	overlay     string // the credential part of the overlay
	wantCleared bool
	wantReason  string // substring of the unusable_credentials line; "" = none
}

var slackResupplyCases = []resupplyCase{
	{name: "env name not allowlisted", envValue: "secret-env-value", overlay: `"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}`,
		wantReason: "slack: env var LOOMCYCLE_SLACK is not in"},
	{name: "env name allowlisted but unset", allowlisted: true, overlay: `"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}`,
		wantReason: "slack: env var LOOMCYCLE_SLACK is not in"},
	{name: "env name allowlisted and set", allowlisted: true, envValue: "secret-env-value", overlay: `"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}`,
		wantCleared: true},
	{name: "whitespace literal", overlay: `"user_credentials":{"slack":"   "}`,
		wantReason: "slack: the user_credentials value is blank"},
}

func (c resupplyCase) setup(t *testing.T, cfg *config.Config) {
	t.Helper()
	if c.allowlisted {
		cfg.Env.SchedulerEnvAllowlist = []string{"LOOMCYCLE_SLACK"}
	}
	t.Setenv("LOOMCYCLE_SLACK", c.envValue)
}

// checkResult asserts the create/fork result reports exactly what the case
// leaves missing, and never the env variable's value.
func (c resupplyCase) checkResult(t *testing.T, out map[string]any) {
	t.Helper()
	missing := resultList(out, "disabled_until_credentials_supplied")
	reasons := resultList(out, "unusable_credentials")
	if c.wantCleared {
		if len(missing) != 0 || len(reasons) != 0 {
			t.Errorf("result lists missing=%v reasons=%v, want neither", missing, reasons)
		}
		return
	}
	if !reflect.DeepEqual(missing, []string{"slack"}) {
		t.Errorf("disabled_until_credentials_supplied = %v, want [slack]", missing)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], c.wantReason) {
		t.Errorf("unusable_credentials = %v, want one line containing %q", reasons, c.wantReason)
	}
	for _, r := range reasons {
		if c.envValue != "" && strings.Contains(r, c.envValue) {
			t.Errorf("reason %q reads out the env value", r)
		}
	}
}

// A schedule re-supply counts only when the scheduler will use it: an env name
// it would skip (not allowlisted, or unset) or a blank literal keeps the def
// disabled and marked, with the reason in the result, and the fire count
// carries over either way.
func TestScheduleDefTool_UnallowlistedEnvResupplyKeepsMarker(t *testing.T) {
	for _, c := range slackResupplyCases {
		t.Run(c.name, func(t *testing.T) {
			tool, ctx, cleanup := scheduleDefFixture(t)
			defer cleanup()
			c.setup(t, tool.Cfg)
			plantCaptureDisabled(t, tool.Store, "digest", []string{"slack"}, 3)

			out := execResult(t, scheduleExec(tool), ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,`+c.overlay+`}}`)
			def, count := storedSchedule(t, tool, out)
			if c.wantCleared {
				if def.CaptureDisabled != nil || !enabledOf(def) {
					t.Errorf("marker=%+v enabled=%v, want cleared and enabled", def.CaptureDisabled, enabledOf(def))
				}
			} else if def.CaptureDisabled == nil || !reflect.DeepEqual(def.CaptureDisabled.StrippedCredentials, []string{"slack"}) || enabledOf(def) {
				t.Errorf("marker=%+v enabled=%v, want [slack] kept and disabled", def.CaptureDisabled, enabledOf(def))
			}
			if count != 3 {
				t.Errorf("fire_count = %d, want 3 inherited", count)
			}
			c.checkResult(t, out)
		})
	}
}

// The same rule on a create over the marked name.
func TestScheduleDefTool_CreateWithUnusableResupplyKeepsMarker(t *testing.T) {
	for _, c := range slackResupplyCases {
		t.Run(c.name, func(t *testing.T) {
			tool, ctx, cleanup := scheduleDefFixture(t)
			defer cleanup()
			c.setup(t, tool.Cfg)
			plantCaptureDisabled(t, tool.Store, "digest", []string{"slack"}, 3)

			out := execResult(t, scheduleExec(tool), ctx, `{"op":"create","name":"digest","overlay":{"agent":"job-search-batch",
				"schedule":"0 6 * * *","enabled":true,`+c.overlay+`}}`)
			def, _ := storedSchedule(t, tool, out)
			if cleared := def.CaptureDisabled == nil && enabledOf(def); cleared != c.wantCleared {
				t.Errorf("marker=%+v enabled=%v, want cleared=%v", def.CaptureDisabled, enabledOf(def), c.wantCleared)
			}
			c.checkResult(t, out)
		})
	}
}

// The same rule for a webhook, against the receiver's allowlist.
func TestWebhookDefTool_UnallowlistedEnvResupplyKeepsMarker(t *testing.T) {
	for _, c := range slackResupplyCases {
		t.Run(c.name, func(t *testing.T) {
			tool, ctx, cleanup := webhookDefFixture(t)
			defer cleanup()
			c.setup(t, tool.Cfg)
			plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"slack"})

			out := execResult(t, webhookExec(tool), ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,`+c.overlay+`}}`)
			w := resolvedWebhook(t, tool, "restored")
			if c.wantCleared {
				if w.CaptureDisabled != nil || !w.Enabled {
					t.Errorf("receiver sees marker=%+v enabled=%v, want cleared and enabled", w.CaptureDisabled, w.Enabled)
				}
			} else if w.CaptureDisabled == nil || !reflect.DeepEqual(w.CaptureDisabled.StrippedCredentials, []string{"slack"}) || w.Enabled {
				t.Errorf("receiver sees marker=%+v enabled=%v, want [slack] kept and disabled", w.CaptureDisabled, w.Enabled)
			}
			c.checkResult(t, out)
		})
	}
}

func TestWebhookDefTool_CreateWithUnusableResupplyKeepsMarker(t *testing.T) {
	for _, c := range slackResupplyCases {
		t.Run(c.name, func(t *testing.T) {
			tool, ctx, cleanup := webhookDefFixture(t)
			defer cleanup()
			c.setup(t, tool.Cfg)
			plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"slack"})

			out := execResult(t, webhookExec(tool), ctx, `{"op":"create","name":"restored","overlay":{"enabled":true,"delivery":"spawn",
				"agent":"intake","auth":{"signing_secret_env":"LOOMCYCLE_WH_SECRET"},`+c.overlay+`}}`)
			w := resolvedWebhook(t, tool, "restored")
			if cleared := w.CaptureDisabled == nil && w.Enabled; cleared != c.wantCleared {
				t.Errorf("receiver sees marker=%+v enabled=%v, want cleared=%v", w.CaptureDisabled, w.Enabled, c.wantCleared)
			}
			c.checkResult(t, out)
		})
	}
}

// A ScheduleDef fork or create that leaves keys missing says which, as a
// WebhookDef's does; one that supplies them all does not.
func TestScheduleDefTool_PartialResupplyResultListsMissingKeys(t *testing.T) {
	tool, ctx, cleanup := scheduleDefFixture(t)
	defer cleanup()
	exec := scheduleExec(tool)
	plantCaptureDisabled(t, tool.Store, "digest", []string{"jobs", "slack", "telegram"}, 1)

	out := execResult(t, exec, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,"user_credentials":{"jobs":"j"}}}`)
	if got := resultList(out, "disabled_until_credentials_supplied"); !reflect.DeepEqual(got, []string{"slack", "telegram"}) {
		t.Errorf("fork result lists %v, want [slack telegram]", got)
	}
	if got := resultList(out, "unusable_credentials"); len(got) != 0 {
		t.Errorf("fork result gives reasons %v for keys it never named", got)
	}

	out = execResult(t, exec, ctx, `{"op":"create","name":"digest","overlay":{"agent":"job-search-batch","schedule":"0 6 * * *",
		"enabled":true,"user_credentials":{"slack":"s"}}}`)
	if got := resultList(out, "disabled_until_credentials_supplied"); !reflect.DeepEqual(got, []string{"telegram"}) {
		t.Errorf("create result lists %v, want [telegram]", got)
	}

	out = execResult(t, exec, ctx, `{"op":"fork","name":"digest","overlay":{"enabled":true,"user_credentials":{"telegram":"t"}}}`)
	if _, ok := out["disabled_until_credentials_supplied"]; ok {
		t.Errorf("a fork that supplied the last key still lists missing keys: %v", out)
	}
}
