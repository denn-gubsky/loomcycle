package builtin

import "testing"

// F24: static yaml webhooks materialize into webhook_defs at boot — parity with
// agents/skills/schedules — so they are listable/forkable without a prior fork.
// Idempotent + fork-respecting.
func TestWebhookDef_BootstrapStaticWebhooks(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()

	// First pass seeds the one static webhook (gh-push) from the fixture cfg.
	n, err := tool.BootstrapStaticWebhooks(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if n != 1 {
		t.Fatalf("seeded %d, want 1", n)
	}
	row, err := tool.Store.WebhookDefGetActive(ctx, "", "gh-push")
	if err != nil {
		t.Fatalf("get active after bootstrap: %v", err)
	}
	if !row.BootstrappedFromStatic {
		t.Errorf("BootstrappedFromStatic = false, want true")
	}

	// Idempotent: a name that already has an active version is left untouched.
	n2, err := tool.BootstrapStaticWebhooks(ctx)
	if err != nil {
		t.Fatalf("bootstrap (2nd pass): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second bootstrap seeded %d, want 0 (idempotent)", n2)
	}
}
