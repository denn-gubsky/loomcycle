package snapshot

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// V4b: the target derives which credentials the restored definitions need
// and does not have — from the reference text in the bodies, through two
// injected yes/no checks — and names the definition and the reference.

// fakeCredChecks answers from fixed sets and records what it was asked.
type fakeCredChecks struct {
	env   map[string]bool
	creds map[string]bool // tenant/agent/user/name
	asked []string
}

func (f *fakeCredChecks) opts() RestoreOptions {
	return RestoreOptions{
		Validators: passValidators(),
		EnvSet:     func(name string) bool { return f.env[name] },
		CredentialExists: func(_ context.Context, tenant, agent, user, name string) bool {
			key := tenant + "/" + agent + "/" + user + "/" + name
			f.asked = append(f.asked, key)
			return f.creds[key]
		},
	}
}

// plantScanFixture writes a webhook and a schedule that each name env vars
// and $cred: references, and a server card that names a signing env.
func plantScanFixture(t *testing.T, s store.Store) {
	t.Helper()
	plantWebhook(t, s, store.WebhookDefRow{DefID: "wh_1", TenantID: "acme", Name: "gh", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"delivery": "spawn", "agent": "intake", "enabled": true, "tenant_id": "acme",
			"auth":                      map[string]any{"kind": "hmac", "signing_secret_env": "LOOMCYCLE_WH_SECRET"},
			"user_credentials_from_env": map[string]string{"telegram": "LOOMCYCLE_TG"},
			"user_credentials":          map[string]string{"jobs": "$cred:jobs-token"},
		}, true)
	plantSchedules(t, s, plantedSchedule{
		row: store.ScheduleDefRow{DefID: "sd_1", TenantID: "acme", Name: "digest", Version: 1, CreatedAt: schedBase()},
		body: map[string]any{"agent": "digester", "user_id": "alice", "tenant_id": "acme", "schedule": "0 6 * * *",
			"user_credentials_from_env": map[string]string{"slack": "LOOMCYCLE_SLACK"},
			"user_credentials":          map[string]string{"gh": "Bearer $ghapp:gh-app", "api": "${LOOMCYCLE_API}"},
		},
		active: true, next: schedBase(),
	})
	plantA2ACard(t, s, store.A2AServerCardDefRow{DefID: "ascd_1", TenantID: "acme", Name: "card", Version: 1, CreatedAt: triggerBase()},
		map[string]any{"name": "card", "exposed_agents": []any{map[string]any{"agent_name": "a"}}, "sign_with_key_env": "LOOMCYCLE_CARD_KEY"}, true)
}

func TestCredentialScan_NamesEachMissingReferenceOnWebhooksSchedulesAndCards(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantScanFixture(t, src)

	checks := &fakeCredChecks{}
	res := mustRestore(t, dst, mustCapture(t, src), checks.opts())
	for _, want := range [][]string{
		{"webhook_def acme/gh", "auth.signing_secret_env", "LOOMCYCLE_WH_SECRET", "not set on this host"},
		{"webhook_def acme/gh", "user_credentials_from_env.telegram", "LOOMCYCLE_TG"},
		{"webhook_def acme/gh", "user_credentials.jobs", "$cred:jobs-token", `tenant "acme"`},
		{"schedule_def acme/digest", "user_credentials_from_env.slack", "LOOMCYCLE_SLACK"},
		{"schedule_def acme/digest", "user_credentials.gh", "$ghapp:gh-app"},
		{"schedule_def acme/digest", "user_credentials.api", "LOOMCYCLE_API"},
		{"a2a_server_card_def acme/card", "sign_with_key_env", "LOOMCYCLE_CARD_KEY"},
	} {
		if !hasWarning(res, append([]string{"missing credential:"}, want...)...) {
			t.Errorf("no missing-credential warning with %q; warnings:\n%s", want, strings.Join(res.Warnings, "\n"))
		}
	}
	// The credential check is asked in the scope a run of the def resolves:
	// the webhook's execution tenant and agent, the schedule's agent and user.
	for _, key := range []string{"acme/intake//jobs-token", "acme/digester/alice/gh-app"} {
		found := false
		for _, a := range checks.asked {
			if a == key {
				found = true
			}
		}
		if !found {
			t.Errorf("CredentialExists was not asked %q (asked %v)", key, checks.asked)
		}
	}
}

func TestCredentialScan_SilentWhenEveryReferenceResolves(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantScanFixture(t, src)

	checks := &fakeCredChecks{
		env: map[string]bool{"LOOMCYCLE_WH_SECRET": true, "LOOMCYCLE_TG": true, "LOOMCYCLE_SLACK": true,
			"LOOMCYCLE_API": true, "LOOMCYCLE_CARD_KEY": true},
		creds: map[string]bool{"acme/intake//jobs-token": true, "acme/digester/alice/gh-app": true},
	}
	res := mustRestore(t, dst, mustCapture(t, src), checks.opts())
	for _, w := range res.Warnings {
		if strings.Contains(w, "missing credential") || strings.Contains(w, "missing-credential scan") {
			t.Errorf("warned although every reference resolves: %s", w)
		}
	}
}

// A lineage warns once per definition name, not once per version; a retired
// version and a def the target already had are not scanned.
func TestCredentialScan_OneWarningPerNameAndOnlyForWhatItRestored(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	body := map[string]any{"delivery": "spawn", "agent": "a", "enabled": true,
		"auth": map[string]any{"signing_secret_env": "LOOMCYCLE_WH_SECRET"}}
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_v1", Name: "gh", Version: 1, CreatedAt: triggerBase()}, body, false)
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_v2", Name: "gh", Version: 2, ParentDefID: "wh_v1", CreatedAt: triggerBase()}, body, true)
	retired := map[string]any{"delivery": "spawn", "agent": "a", "auth": map[string]any{"signing_secret_env": "LOOMCYCLE_RETIRED"}}
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_old", Name: "old", Version: 1, CreatedAt: triggerBase(), Retired: true}, retired, false)
	live := map[string]any{"delivery": "spawn", "agent": "a", "auth": map[string]any{"signing_secret_env": "LOOMCYCLE_LIVE"}}
	plantWebhook(t, src, store.WebhookDefRow{DefID: "wh_live", Name: "live", Version: 1, CreatedAt: triggerBase()}, live, false)
	plantWebhook(t, dst, store.WebhookDefRow{DefID: "wh_live", Name: "live", Version: 1, CreatedAt: triggerBase()}, live, false)

	res := mustRestore(t, dst, mustCapture(t, src), (&fakeCredChecks{}).opts())
	n := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, "LOOMCYCLE_WH_SECRET") {
			n++
		}
		if strings.Contains(w, "LOOMCYCLE_RETIRED") || strings.Contains(w, "LOOMCYCLE_LIVE") {
			t.Errorf("scanned a retired or already-present def: %s", w)
		}
	}
	if n != 1 {
		t.Errorf("%d warnings for the two versions of one webhook, want 1: %v", n, res.Warnings)
	}
}

// Unwired checks are not guessed at: the scan says how many references it
// did not check, and names none of them.
func TestCredentialScan_UnwiredChecksSayTheyDidNotCheck(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	plantScanFixture(t, src)
	res := mustRestore(t, dst, mustCapture(t, src), RestoreOptions{Validators: passValidators()})
	if !hasWarning(res, "missing-credential scan: 5 env var reference(s)", "not checked") ||
		!hasWarning(res, "missing-credential scan: 2 credential reference(s)", "not checked") {
		t.Errorf("no not-checked warnings with the counts: %v", res.Warnings)
	}
	if hasWarning(res, "missing credential:") {
		t.Errorf("an unwired check still named a reference: %v", res.Warnings)
	}
}
