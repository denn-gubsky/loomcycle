package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const githubPushOverlay = `"provider":"github","events":["push"]`

// A webhook that lists events gets {provider, event, delivery_id} under the
// metadata key "webhook" from the receiver, so an author may not write that
// key there: a value they wrote would read as one the receiver vouched for.
// Without events the key is the author's, as before.
func TestWebhookDefTool_TheMetadataKeyWebhookIsTheReceiversWhenEventsAreListed(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	text, isErr := webhookCreate(t, tool, ctx, "evented", `{`+webhookBase+`,`+githubPushOverlay+`,"metadata":{"webhook":{"event":"push"}}}`)
	if !isErr || !strings.Contains(text, `metadata key "webhook"`) {
		t.Errorf("create with events and metadata.webhook: %s, want it refused", text)
	}
	if text, isErr := webhookCreate(t, tool, ctx, "plain", `{`+webhookBase+`,"metadata":{"webhook":"mine"}}`); isErr {
		t.Fatalf("without events the key is the author's: %s", text)
	}
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"plain","overlay":{`+githubPushOverlay+`}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, `metadata key "webhook"`) {
		t.Errorf("a fork adding events to a definition whose metadata has the key: %s, want it refused", res.Text)
	}
}

// get shows how the deliveries to the webhook ended (RFC EK §9): the counts of
// the name in the row's tenant, summed over its versions. A shared-tenant name
// the operator's yaml also declares is served by the yaml webhook, so those
// are its counts.
func TestWebhookDefTool_GetShowsTheWebhooksDeliveryCounts(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	text, isErr := webhookCreate(t, tool, ctx, "counted", `{`+webhookBase+`}`)
	if isErr {
		t.Fatalf("create: %s", text)
	}
	var created struct {
		DefID string `json:"def_id"`
	}
	if err := json.Unmarshal([]byte(text), &created); err != nil || created.DefID == "" {
		t.Fatalf("create answered %s", text)
	}
	row, err := tool.Store.WebhookDefGet(ctx, created.DefID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	add := func(static bool, outcome, reason string, n int64) {
		t.Helper()
		if err := tool.Store.WebhookCountsAdd(context.Background(), []store.WebhookCount{{
			TenantID: row.TenantID, Name: "counted", Static: static, Outcome: outcome, Reason: reason, Count: n, LastAt: at,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	add(false, store.WebhookOutcomeAccepted, "", 4)
	add(false, store.WebhookOutcomeRefused, "signature", 2)
	add(true, store.WebhookOutcomeAccepted, "", 100) // the yaml one's, if there were one

	deliveries := func() store.WebhookCountSummary {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+created.DefID+`"}`))
		if err != nil || res.IsError {
			t.Fatalf("get: %v %s", err, res.Text)
		}
		var got struct {
			Status struct {
				Deliveries store.WebhookCountSummary `json:"deliveries"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
			t.Fatalf("get answered %s", res.Text)
		}
		return got.Status.Deliveries
	}
	if got := deliveries(); got.Accepted != 4 || got.Refused["signature"] != 2 || got.LastAt == nil || !got.LastAt.Equal(at) {
		t.Errorf("deliveries = %+v, want accepted 4, refused signature 2, last_at %v", got, at)
	}

	// The fixture authors in the shared tenant: with a yaml webhook of the
	// same name, deliveries go to the yaml one, and so do the counts.
	if row.TenantID != "" {
		t.Fatalf("the fixture authored in tenant %q; this case needs the shared tenant", row.TenantID)
	}
	tool.Cfg.Webhooks = map[string]config.Webhook{"counted": {Enabled: true}}
	if got := deliveries(); got.Accepted != 100 {
		t.Errorf("a shared-tenant name the yaml also declares: accepted = %d, want the yaml webhook's 100", got.Accepted)
	}
}
