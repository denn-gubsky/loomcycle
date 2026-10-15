package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// op=get shows the webhook's delivery counts as a status block: those the
// receiver wrote under the key it resolves the webhook by — its tenant, or for
// a shared row the yaml definition of the name, which wins there.
func TestWebhookDefTool_GetShowsTheDeliveryCounters(t *testing.T) {
	tool, ctx, _ := storedSecretFixture(t)
	if text, isErr := webhookCreate(t, tool, ctx, "w", `{"enabled":true,"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}}`); isErr {
		t.Fatal(text)
	}
	key := lookup.WebhookStateKey(lookup.WebhookOwner{TenantID: "acme"}, "w")
	at := time.Unix(1_700_000_000, 0).UTC()
	if err := tool.Store.WebhookCountersAdd(context.Background(), []store.WebhookCounterAdd{
		{WebhookKey: key, Outcome: "accepted", N: 5}, {WebhookKey: key, Outcome: "filtered", N: 2},
		{WebhookKey: lookup.WebhookStateKey(lookup.WebhookOwner{TenantID: "other"}, "w"), Outcome: "accepted", N: 99},
	}, at); err != nil {
		t.Fatal(err)
	}
	row, err := tool.Store.WebhookDefGetActive(context.Background(), "acme", "w")
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"get","def_id":"`+row.DefID+`"}`))
	if err != nil || res.IsError {
		t.Fatalf("%v %s", err, res.Text)
	}
	var got struct {
		Status struct {
			Counters  map[string]int64 `json:"counters"`
			UpdatedAt string           `json:"counters_updated_at"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Status.Counters, map[string]int64{"accepted": 5, "filtered": 2}) || got.Status.UpdatedAt != at.Format(time.RFC3339) {
		t.Errorf("status = %+v, want this tenant's counts only", got.Status)
	}
}

func TestWebhookOwnerOfRow_MatchesHowTheReceiverResolvesIt(t *testing.T) {
	tool, _, cleanup := webhookDefFixture(t)
	defer cleanup()
	for _, tc := range []struct {
		tenant, name string
		want         lookup.WebhookOwner
	}{
		{"acme", "gh-push", lookup.WebhookOwner{TenantID: "acme"}},
		{"", "gh-push", lookup.WebhookOwner{Static: true}},
		{"", "runtime-only", lookup.WebhookOwner{}},
	} {
		if got := lookup.WebhookOwnerOfRow(tool.Cfg, tc.tenant, tc.name); got != tc.want {
			t.Errorf("%q/%s: %+v, want %+v", tc.tenant, tc.name, got, tc.want)
		}
	}
}
