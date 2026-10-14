package snapshot

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A webhook's stored verification secret, and that of a team's own webhook,
// is checked as the receiver reads it: a tenant-level credential of the
// definition's owning tenant — not its runs' tenant, not its agent's — and
// through the compare-only check, which finds a credential restricted to no
// URL that the keyability check would call missing.
func TestCredentialScan_AStoredWebhookSecretIsCheckedInTheOwnersTenant(t *testing.T) {
	ctx := context.Background()
	src, srcClose := newTestStore(t)
	defer srcClose()
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_s", TenantID: "acme", Name: "gh", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"delivery": "spawn", "agent": "intake", "enabled": true, "tenant_id": "runs-here",
			"auth": map[string]any{"kind": "hmac", "secret": "$cred:hook", "previous_secret": "$cred:hook-old"}}, true)
	if _, err := src.TeamDefCreate(ctx, store.TeamDefRow{DefID: "td_s", TenantID: "acme", Name: "crew", Version: 1, CreatedAt: triggerBase(),
		Definition: json.RawMessage(`{"local":{"webhooks":{"push":{"channel":"./events","auth":{"kind":"bearer","secret":"$cred:team-hook"}}}}}`)}); err != nil {
		t.Fatal(err)
	}
	snap := mustCapture(t, src)

	restore := func(held map[string]bool, wire bool) (RestoreResult, []string) {
		t.Helper()
		dst, dstClose := newTestStore(t)
		t.Cleanup(dstClose)
		var asked []string
		opts := (&fakeCredChecks{}).opts() // keyability: every credential missing
		if wire {
			opts.CredentialToCompare = func(_ context.Context, tenant, name string) bool {
				asked = append(asked, tenant+"/"+name)
				return held[tenant+"/"+name]
			}
		}
		return mustRestore(t, dst, snap, opts), asked
	}

	res, asked := restore(map[string]bool{"acme/hook": true}, true)
	for _, want := range [][]string{
		{"webhook_def acme/gh", "auth.previous_secret", "$cred:hook-old", `tenant "acme"`},
		{"team_def acme/crew", "local.webhooks.push.auth.secret", "$cred:team-hook", `tenant "acme"`},
	} {
		if !hasWarning(res, append([]string{"missing credential:"}, want...)...) {
			t.Errorf("no warning with %q; warnings:\n%s", want, strings.Join(res.Warnings, "\n"))
		}
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "references $cred:hook,") {
			t.Errorf("warned about a secret the owner's tenant holds: %s", w)
		}
	}
	sort.Strings(asked)
	if got := strings.Join(asked, " "); got != "acme/hook acme/hook-old acme/team-hook" {
		t.Errorf("asked %q, want each reference in the owning tenant acme", got)
	}

	// Not wired: the references are counted as not checked, not guessed at.
	res, _ = restore(nil, false)
	if !hasWarning(res, "missing-credential scan:", "credential reference(s)", "not checked") {
		t.Errorf("an unwired compare check did not say so: %v", res.Warnings)
	}
}
