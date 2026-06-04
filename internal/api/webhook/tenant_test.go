package webhook

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestWebhookBuildRunInput_TenantFromDefNotPayload pins the security
// invariant for the webhook tenant axis (RFC N follow-up): the tenant the
// spawned run executes as comes from the STATIC operator-authored def ONLY,
// NEVER from the inbound payload. The payload here carries tenant-ish fields
// (a mapped `tenant_id` target and a `run_metadata.tenant`), and NONE of them
// may override the def — otherwise an attacker who can shape the signed body
// could steer the run into another tenant's agents/skills/memory. An empty
// def tenant must resolve to "" (shared/default), not pick up the payload.
func TestWebhookBuildRunInput_TenantFromDefNotPayload(t *testing.T) {
	w := config.Webhook{Agent: "reviewer", TenantID: "acme"}
	proj := projectResult{Fields: map[string]string{
		"goal":                "review",
		"tenant_id":           "attacker", // not a real mapping target, but pin it can't leak
		"run_metadata.tenant": "attacker", // projected metadata is UNTRUSTED — must not become the tenant
	}}
	in := buildRunInput(w, proj, map[string]bool{}, func(string) string { return "" }, nil)
	if in.TenantID != "acme" {
		t.Errorf("def tenant must win and payload cannot set it: got %q, want %q", in.TenantID, "acme")
	}

	// Empty def tenant: payload tenant-ish fields still cannot inject a tenant.
	noTenant := config.Webhook{Agent: "reviewer"}
	got := buildRunInput(noTenant, proj, map[string]bool{}, func(string) string { return "" }, nil).TenantID
	if got != "" {
		t.Errorf("empty def tenant must stay \"\" (shared/default); payload must not inject %q", got)
	}
}
