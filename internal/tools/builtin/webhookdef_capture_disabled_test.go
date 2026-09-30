package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A webhook restored from a snapshot without its literal credentials comes back
// disabled, with a capture_disabled marker listing the stripped keys. These
// tests pin how a fork re-enables it: only when its OWN overlay re-supplies
// every listed key. Webhooks have no fire count, so nothing else carries over.

// plantCaptureDisabledWebhook writes a webhook def in the shape a snapshot
// restore leaves it: enabled:false, the marker, no literal values for the
// stripped keys, active. The parent already sources "telegram" from env.
func plantCaptureDisabledWebhook(t *testing.T, st store.Store, name string, stripped []string) string {
	t.Helper()
	ctx := context.Background()
	body, err := json.Marshal(map[string]any{
		"delivery": "spawn", "agent": "intake", "enabled": false,
		"auth":                      map[string]any{"kind": "hmac", "signing_secret_env": "LOOMCYCLE_WH_SECRET"},
		"user_credentials_from_env": map[string]string{"telegram": "LOOMCYCLE_TG"},
		"capture_disabled":          map[string]any{"stripped_credentials": stripped},
	})
	if err != nil {
		t.Fatal(err)
	}
	defID := "wh_restored_" + name
	if _, err := st.SnapshotRestoreWebhookDef(ctx, store.WebhookDefRow{
		DefID: defID, Name: name, Version: 1, Definition: body, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("plant def: %v", err)
	}
	if _, err := st.SnapshotRestoreWebhookDefActive(ctx, store.WebhookDefActiveEntry{Name: name, DefID: defID}); err != nil {
		t.Fatalf("plant active: %v", err)
	}
	return defID
}

// webhookForkResult runs a fork and returns the decoded result and new body.
func webhookForkResult(t *testing.T, tool *WebhookDef, ctx context.Context, input string) (map[string]any, mergedWebhookDef) {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(input))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	defID, _ := out["def_id"].(string)
	row, err := tool.Store.WebhookDefGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get fork %s: %v", defID, err)
	}
	var def mergedWebhookDef
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatalf("decode fork body: %v", err)
	}
	return out, def
}

// resolvedWebhook is what the receiver would see for the name.
func resolvedWebhook(t *testing.T, tool *WebhookDef, name string) config.Webhook {
	t.Helper()
	w, ok := lookup.Webhook(context.Background(), tool.Store, tool.Cfg, "", name)
	if !ok {
		t.Fatalf("webhook %q does not resolve", name)
	}
	return w
}

// V2c: a fork whose own overlay re-supplies every stripped key (one literal,
// one from env) and sets enabled:true clears the marker and is enabled.
func TestWebhookDefTool_ForkClearsCaptureDisabledOnFullResupply(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"jobs", "slack"})

	out, def := webhookForkResult(t, tool, ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,
		"user_credentials":{"jobs":"j-new"},"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}}}`)
	if def.CaptureDisabled != nil {
		t.Errorf("marker = %+v, want cleared (every stripped key re-supplied)", def.CaptureDisabled)
	}
	if !def.Enabled {
		t.Error("fork is disabled, want the overlay's enabled:true honoured")
	}
	if _, ok := out["disabled_until_credentials_supplied"]; ok {
		t.Errorf("result still lists missing keys: %v", out)
	}
	if w := resolvedWebhook(t, tool, "restored"); !w.Enabled || w.CaptureDisabled != nil {
		t.Errorf("receiver sees enabled=%v marker=%+v, want enabled and unmarked", w.Enabled, w.CaptureDisabled)
	}
}

// V2c: a partial re-supply stays disabled whatever the overlay says, and the
// marker, the result and the stored body list exactly the keys still missing.
func TestWebhookDefTool_ForkWithPartialResupplyStaysDisabled(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"jobs", "slack"})

	out, def := webhookForkResult(t, tool, ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,
		"user_credentials":{"jobs":"j-new"}}}`)
	if def.CaptureDisabled == nil || !reflect.DeepEqual(def.CaptureDisabled.StrippedCredentials, []string{"slack"}) {
		t.Fatalf("marker = %+v, want [slack] still missing", def.CaptureDisabled)
	}
	if def.Enabled {
		t.Error("fork is enabled although a stripped key is still missing")
	}
	if got, _ := out["disabled_until_credentials_supplied"].([]any); len(got) != 1 || got[0] != "slack" {
		t.Errorf("result lists %v, want [slack]", out["disabled_until_credentials_supplied"])
	}

	// The next fork supplies the last key and clears it.
	_, def = webhookForkResult(t, tool, ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,
		"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}}}`)
	if def.CaptureDisabled != nil || !def.Enabled {
		t.Errorf("second fork: marker=%+v enabled=%v, want cleared and enabled", def.CaptureDisabled, def.Enabled)
	}
}

// V2c: a key the PARENT already sources does not count as re-supplied — only
// the fork's own overlay does. And any fork of a marked def (here one that
// touches nothing credential-related) carries the marker forward.
func TestWebhookDefTool_ForkParentKeyDoesNotCountAsResupplied(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"telegram"})

	_, def := webhookForkResult(t, tool, ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,"body_size_limit_bytes":4096}}`)
	if def.CaptureDisabled == nil || !reflect.DeepEqual(def.CaptureDisabled.StrippedCredentials, []string{"telegram"}) {
		t.Fatalf("marker = %+v, want [telegram] kept (the parent's env source is not a re-supply)", def.CaptureDisabled)
	}
	if def.Enabled {
		t.Error("fork is enabled although the key was never re-supplied")
	}
}

// V2c: an overlay can neither set nor clear the marker.
func TestWebhookDefTool_OverlayCannotSetOrClearCaptureDisabled(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"fresh","overlay":{"enabled":true,"delivery":"spawn","agent":"a",
		"auth":{"signing_secret_env":"LOOMCYCLE_S"},"capture_disabled":{"stripped_credentials":["x"]}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	if w := resolvedWebhook(t, tool, "fresh"); w.CaptureDisabled != nil || !w.Enabled {
		t.Errorf("create with an overlay marker: marker=%+v enabled=%v, want the overlay's marker ignored", w.CaptureDisabled, w.Enabled)
	}

	plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"jobs"})
	_, def := webhookForkResult(t, tool, ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,"capture_disabled":null}}`)
	if def.CaptureDisabled == nil || def.Enabled {
		t.Errorf("fork with capture_disabled:null: marker=%+v enabled=%v, want the marker kept and disabled", def.CaptureDisabled, def.Enabled)
	}
	_, def = webhookForkResult(t, tool, ctx, `{"op":"fork","name":"restored","overlay":{"enabled":true,"capture_disabled":{"stripped_credentials":[]}}}`)
	if def.CaptureDisabled == nil || def.Enabled {
		t.Errorf("fork with an emptied marker: marker=%+v enabled=%v, want the marker kept and disabled", def.CaptureDisabled, def.Enabled)
	}
}

// A marked def with enabled:true in its body (a hand-edited store, say) still
// reaches the receiver marked, so the receiver's marker check is what decides.
func TestWebhookDef_MarkerSurvivesToReceiverShape(t *testing.T) {
	tool, _, cleanup := webhookDefFixture(t)
	defer cleanup()
	ctx := context.Background()
	body := json.RawMessage(`{"delivery":"spawn","agent":"a","enabled":true,"auth":{"signing_secret_env":"LOOMCYCLE_S"},
		"capture_disabled":{"stripped_credentials":["jobs"]}}`)
	if _, err := tool.Store.SnapshotRestoreWebhookDef(ctx, store.WebhookDefRow{DefID: "wh_m", Name: "m", Version: 1, Definition: body, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Store.SnapshotRestoreWebhookDefActive(ctx, store.WebhookDefActiveEntry{Name: "m", DefID: "wh_m"}); err != nil {
		t.Fatal(err)
	}
	w := resolvedWebhook(t, tool, "m")
	if w.CaptureDisabled == nil || !reflect.DeepEqual(w.CaptureDisabled.StrippedCredentials, []string{"jobs"}) {
		t.Errorf("config.Webhook marker = %+v, want [jobs]", w.CaptureDisabled)
	}
}

// TestMergedWebhookDef_DriftDetection_CaptureDisabledMarker pins the marker on
// all three webhook mirrors — the write shape, the substrate read shape and
// config.Webhook the receiver consumes — and its nested keys pairwise: the
// snapshot restore writes them as raw JSON, the fork reads them here, and the
// receiver reads them through config.Webhook.
func TestMergedWebhookDef_DriftDetection_CaptureDisabledMarker(t *testing.T) {
	for name, tags := range map[string]map[string]bool{
		"builtin.mergedWebhookDef":   a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedWebhookDef{})),
		"lookup.SubstrateWebhookDef": a2aBuiltinJSONTagsOf(reflect.TypeOf(lookup.SubstrateWebhookDef{})),
		"config.Webhook":             a2aBuiltinJSONTagsOf(reflect.TypeOf(config.Webhook{})),
	} {
		if !tags["capture_disabled"] {
			t.Errorf("%s has no capture_disabled field; a def restored without its credentials would lose its marker there", name)
		}
	}
	want := map[string]bool{"stripped_credentials": true}
	for name, tags := range map[string]map[string]bool{
		"builtin.mergedWebhookCaptureDisabled":   a2aBuiltinJSONTagsOf(reflect.TypeOf(mergedWebhookCaptureDisabled{})),
		"lookup.SubstrateWebhookCaptureDisabled": a2aBuiltinJSONTagsOf(reflect.TypeOf(lookup.SubstrateWebhookCaptureDisabled{})),
		"config.WebhookCaptureDisabled":          a2aBuiltinJSONTagsOf(reflect.TypeOf(config.WebhookCaptureDisabled{})),
	} {
		if !reflect.DeepEqual(tags, want) {
			t.Errorf("%s keys = %v, want %v", name, tags, want)
		}
	}
}

// A create on a name whose current def is capture-disabled writes a new
// version of it, so it follows the fork's rules: a partial supply stays
// disabled with the rest listed; a full supply clears the marker.
func TestWebhookDefTool_CreateOverCaptureDisabledPartialStaysDisabled(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"jobs", "slack"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"restored","overlay":{"enabled":true,"delivery":"spawn",
		"agent":"intake","auth":{"signing_secret_env":"LOOMCYCLE_WH_SECRET"},"user_credentials":{"jobs":"j-new"}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	if got, _ := decodeResult(t, res.Text)["disabled_until_credentials_supplied"].([]any); len(got) != 1 || got[0] != "slack" {
		t.Errorf("result lists %v, want [slack]", got)
	}
	w := resolvedWebhook(t, tool, "restored")
	if w.CaptureDisabled == nil || !reflect.DeepEqual(w.CaptureDisabled.StrippedCredentials, []string{"slack"}) || w.Enabled {
		t.Errorf("receiver sees marker=%+v enabled=%v; want [slack] still missing and disabled", w.CaptureDisabled, w.Enabled)
	}
}

func TestWebhookDefTool_CreateOverCaptureDisabledFullClears(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	plantCaptureDisabledWebhook(t, tool.Store, "restored", []string{"jobs", "slack"})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"restored","overlay":{"enabled":true,"delivery":"spawn",
		"agent":"intake","auth":{"signing_secret_env":"LOOMCYCLE_WH_SECRET"},
		"user_credentials":{"jobs":"j-new"},"user_credentials_from_env":{"slack":"LOOMCYCLE_SLACK"}}}`))
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	if w := resolvedWebhook(t, tool, "restored"); w.CaptureDisabled != nil || !w.Enabled {
		t.Errorf("receiver sees marker=%+v enabled=%v; want cleared and enabled", w.CaptureDisabled, w.Enabled)
	}
}
