package webhook

import (
	"context"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
)

// A delivery's run is counted under the webhook trigger: its ctx is built
// fresh from the delivery, so nothing upstream could have named it.
func TestDeliveryRunCtx_NamesTheWebhookTrigger(t *testing.T) {
	if got := lcotel.RunTriggerFromContext(deliveryRunCtx(context.Background())); got != lcotel.TriggerWebhook {
		t.Errorf("trigger = %q, want %q", got, lcotel.TriggerWebhook)
	}
}
