package snapshot

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// plantOAuthWebhook writes a webhook whose credentials name an $oauth:
// credential.
func plantOAuthWebhook(t *testing.T, s store.Store) {
	t.Helper()
	plantWebhook(t, s, store.WebhookDefRow{DefID: "wh_o", TenantID: "acme", Name: "docs", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"delivery": "spawn", "agent": "intake", "enabled": true, "tenant_id": "acme",
			"auth":             map[string]any{"kind": "hmac", "signing_secret_env": "LOOMCYCLE_WH_SECRET"},
			"user_credentials": map[string]string{"docs": "Bearer $oauth:docs-server"},
		}, true)
}

func oauthScanOpts(state string, found bool, asked *[]string) RestoreOptions {
	return RestoreOptions{
		Validators: passValidators(),
		EnvSet:     func(string) bool { return true },
		OAuthCredentialState: func(_ context.Context, tenant, agent, user, name string) (string, bool) {
			*asked = append(*asked, tenant+"/"+agent+"/"+user+"/"+name)
			return state, found
		},
	}
}

func TestSnapshotCredScan_WarnsOAuthRefNotConnected(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantOAuthWebhook(t, src)
	var asked []string
	res := mustRestore(t, dst, mustCapture(t, src), oauthScanOpts("needs_reauth", true, &asked))
	if !hasWarning(res, "webhook_def acme/docs", "user_credentials.docs", "$oauth:docs-server", "not connected", "needs_reauth", "oauth_connect_start") {
		t.Errorf("no not-connected warning; warnings:\n%s", strings.Join(res.Warnings, "\n"))
	}
	if len(asked) != 1 || asked[0] != "acme/intake//docs-server" {
		t.Errorf("asked %v, want the def's tenant and agent", asked)
	}
	// A connected one is silent.
	dst2, dst2Close := newTestStore(t)
	defer dst2Close()
	res = mustRestore(t, dst2, mustCapture(t, src), oauthScanOpts("connected", true, &asked))
	for _, w := range res.Warnings {
		if strings.Contains(w, "$oauth:") || strings.Contains(w, "missing-credential scan") {
			t.Errorf("warned about a connected credential: %s", w)
		}
	}
}

func TestSnapshotCredScan_WarnsOAuthRefMissing(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantOAuthWebhook(t, src)
	var asked []string
	res := mustRestore(t, dst, mustCapture(t, src), oauthScanOpts("", false, &asked))
	if !hasWarning(res, "missing credential:", "webhook_def acme/docs", "user_credentials.docs", "$oauth:docs-server") {
		t.Errorf("no missing warning; warnings:\n%s", strings.Join(res.Warnings, "\n"))
	}
	// Unwired: counted as not checked, never asked of the $cred: check.
	dst2, dst2Close := newTestStore(t)
	defer dst2Close()
	res = mustRestore(t, dst2, mustCapture(t, src), RestoreOptions{
		Validators: passValidators(), EnvSet: func(string) bool { return true },
		CredentialExists: func(context.Context, string, string, string, string) bool {
			t.Error("an $oauth: reference was asked of the $cred: check")
			return true
		},
	})
	if !hasWarning(res, "missing-credential scan", "not checked") {
		t.Errorf("an unchecked $oauth: reference was not counted; warnings:\n%s", strings.Join(res.Warnings, "\n"))
	}
}

// A user_bearer that names an $oauth: credential is refused at run time
// whatever the target holds; the scan says so instead of checking it.
func TestSnapshotCredScan_OAuthUserBearerIsReportedRefused(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_b", TenantID: "acme", Name: "bearer", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"delivery": "spawn", "agent": "intake", "enabled": true, "tenant_id": "acme",
			"auth":        map[string]any{"kind": "hmac", "signing_secret_env": "LOOMCYCLE_WH_SECRET"},
			"user_bearer": "$oauth:docs-server",
		}, true)
	var asked []string
	res := mustRestore(t, dst, mustCapture(t, src), oauthScanOpts("connected", true, &asked))
	if !hasWarning(res, "refused credential:", "webhook_def acme/bearer", "user_bearer", "$oauth:docs-server") {
		t.Errorf("no refused warning; warnings:\n%s", strings.Join(res.Warnings, "\n"))
	}
	if len(asked) != 0 {
		t.Errorf("a refused reference was checked: %v", asked)
	}
}
