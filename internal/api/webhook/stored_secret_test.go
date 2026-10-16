package webhook

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// credentialStore is a store with the server's credential engine over it.
// put stores a tenant-level credential restricted to no URL at all — how a
// verification secret is meant to be stored: it can be compared, never sent.
func credentialStore(t *testing.T) (store.Store, *credential.Engine, func(tenant, name, value string)) {
	t.Helper()
	st := openScopeStore(t)
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	put := func(tenant, name, value string) {
		t.Helper()
		nowhere := &[]string{}
		if _, err := engine.PutInline(context.Background(), credential.Identity{TenantID: tenant, Scope: "tenant", Name: name}, value, nil, nowhere, nil); err != nil {
			t.Fatal(err)
		}
	}
	return st, engine, put
}

func storedSecretDef(auth map[string]any) map[string]any {
	return map[string]any{"enabled": true, "delivery": "spawn", "agent": "x", "tenant_id": "acme", "auth": auth}
}

// postWith POSTs body to path signed GitHub-style with secret.
func postWith(rec *Receiver, path, secret string, body []byte) (int, string) {
	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(secret, body))
	w := post(rec, path, body, h)
	return w.Code, w.Body.String()
}

// A webhook whose secret is a stored credential verifies against it, read
// again once the receiver's copy lapses (storedSecretTTL): a rotation takes
// effect within that, and while one is under way a delivery signed with
// either secret is accepted. The credential is restricted to no URL, which
// refuses it to every sender; a comparison sends it nowhere, so it resolves
// here.
func TestReceiver_StoredSecretVerifiesAndRotatesWithoutARestart(t *testing.T) {
	st, engine, put := credentialStore(t)
	put("acme", "hook", "first-secret")
	putWebhookDef(t, st, "acme", "gh", storedSecretDef(map[string]any{
		"kind": "hmac", "header": "X-Hub-Signature-256", "secret": "$cred:hook"}))
	clk := newTestClock()
	rec := New(Deps{Cfg: &config.Config{}, Store: st, Runner: &fakeRunner{runID: "r"}, Credentials: engine, Now: clk.now})
	n := 0
	deliver := func(secret string) int {
		n++
		code, _ := postWith(rec, "/v1/_webhooks/acme/gh", secret, []byte(fmt.Sprintf(`{"n":%d}`, n)))
		return code
	}

	if code := deliver("first-secret"); code != http.StatusAccepted {
		t.Fatalf("signed with the stored secret: %d, want 202", code)
	}
	if code := deliver("not-the-secret"); code != http.StatusUnauthorized {
		t.Fatalf("signed with another secret: %d, want 401", code)
	}

	// Rotate: the old value moves to a second credential, named as the
	// previous secret, and the first takes a new value. No restart.
	put("acme", "hook-old", "first-secret")
	put("acme", "hook", "second-secret")
	putWebhookDef(t, st, "acme", "gh", storedSecretDef(map[string]any{
		"kind": "hmac", "header": "X-Hub-Signature-256", "secret": "$cred:hook", "previous_secret": "$cred:hook-old"}))
	clk.pastSecretTTL()
	for _, secret := range []string{"second-secret", "first-secret"} {
		if code := deliver(secret); code != http.StatusAccepted {
			t.Errorf("during the rotation, signed with %s: %d, want 202", secret, code)
		}
	}
	if code := deliver("not-the-secret"); code != http.StatusUnauthorized {
		t.Errorf("during the rotation, signed with neither: %d, want 401", code)
	}

	// The rotation ends by deleting the old credential: only the new secret
	// is accepted, and the definition still works.
	if _, err := engine.Delete(context.Background(), credential.Identity{TenantID: "acme", Scope: "tenant", Name: "hook-old"}); err != nil {
		t.Fatal(err)
	}
	clk.pastSecretTTL()
	if code := deliver("first-secret"); code != http.StatusUnauthorized {
		t.Errorf("after the old credential is deleted, signed with it: %d, want 401", code)
	}
	if code := deliver("second-secret"); code != http.StatusAccepted {
		t.Errorf("after the old credential is deleted, signed with the new one: %d, want 202", code)
	}
}

// The stored secret is the definition's own tenant's. Another tenant's
// credential of the same name is never read, and a missing one answers 503
// naming the reference and never a value.
func TestReceiver_StoredSecretIsReadFromTheDefinitionsTenantOnly(t *testing.T) {
	st, engine, put := credentialStore(t)
	put("other", "hook", "other-tenants-secret")
	putWebhookDef(t, st, "acme", "gh", storedSecretDef(map[string]any{
		"kind": "hmac", "header": "X-Hub-Signature-256", "secret": "$cred:hook"}))
	rec := New(Deps{Cfg: &config.Config{}, Store: st, Runner: &fakeRunner{runID: "r"}, Credentials: engine})

	code, body := postWith(rec, "/v1/_webhooks/acme/gh", "other-tenants-secret", []byte(`{}`))
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"secret":"$cred:hook"`) {
		t.Fatalf("no such credential in the definition's tenant: %d %s, want 503 naming the reference", code, body)
	}
	if strings.Contains(body, "other-tenants-secret") {
		t.Fatalf("the 503 carries a secret value: %s", body)
	}

	// Without a credential engine the same definition is unresolvable too.
	bare := New(Deps{Cfg: &config.Config{}, Store: st, Runner: &fakeRunner{runID: "r"}})
	if code, _ := postWith(bare, "/v1/_webhooks/acme/gh", "anything", []byte(`{}`)); code != http.StatusServiceUnavailable {
		t.Errorf("no credential engine: %d, want 503", code)
	}
}

// A bearer webhook (GitLab's X-Gitlab-Token) takes a stored token and its
// previous one the same way.
func TestReceiver_StoredBearerTokenAndItsPreviousAreAccepted(t *testing.T) {
	st, engine, put := credentialStore(t)
	put("acme", "token", "new-token")
	put("acme", "token-old", "old-token")
	putWebhookDef(t, st, "acme", "gl", storedSecretDef(map[string]any{
		"kind": "bearer", "header": "X-Gitlab-Token", "secret": "$cred:token", "previous_secret": "$cred:token-old"}))
	rec := New(Deps{Cfg: &config.Config{}, Store: st, Runner: &fakeRunner{runID: "r"}, Credentials: engine})
	for i, tc := range []struct {
		token string
		want  int
	}{{"new-token", http.StatusAccepted}, {"old-token", http.StatusAccepted}, {"no-token", http.StatusUnauthorized}} {
		h := http.Header{}
		h.Set("X-Gitlab-Token", tc.token)
		if w := post(rec, "/v1/_webhooks/acme/gl", []byte(fmt.Sprintf(`{"n":%d}`, i)), h); w.Code != tc.want {
			t.Errorf("X-Gitlab-Token %s: %d, want %d", tc.token, w.Code, tc.want)
		}
	}
}

// A team's own webhook reads its stored secret from the team's tenant.
func TestTeamWebhook_StoredSecretIsTheTeamsTenants(t *testing.T) {
	_, engine, put := credentialStore(t)
	put("acme", "hook", "team-secret")
	auth := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", Secret: "$cred:hook"}
	th := &fakeTeamHooks{hooks: map[string]runner.TeamWebhook{
		"acme/review/push":  {Auth: auth, Channel: "./events"},
		"other/review/push": {Auth: auth, Channel: "./events"},
	}}
	rec := New(Deps{Cfg: &config.Config{}, Publisher: &fakePublisher{}, TeamWebhooks: th, Credentials: engine, Now: fixedClock(teamNow)})

	if code, body := postWith(rec, "/v1/_teams/acme/review/webhooks/push", "team-secret", []byte(`{"a":1}`)); code != http.StatusAccepted {
		t.Fatalf("the team's tenant holds the credential: %d %s, want 202", code, body)
	}
	if code, _ := postWith(rec, "/v1/_teams/other/review/webhooks/push", "team-secret", []byte(`{"a":2}`)); code != http.StatusServiceUnavailable {
		t.Errorf("a team in a tenant without the credential: %d, want 503", code)
	}
	if got := len(th.publishes()); got != 1 {
		t.Errorf("published %d, want 1", got)
	}
}

// An env-form secret may not name one of loomcycle's own secrets, even when
// the operator allowlists it and the request is signed with its value: a
// definition naming the operator bearer would make whoever holds that bearer
// a webhook sender, and would answer whether a guess is it.
func TestReceiver_AnEnvSecretCannotBeLoomcyclesOwn(t *testing.T) {
	const opToken = "operator-bearer-value"
	fr := &fakeRunner{runID: "r"}
	hook := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "LOOMCYCLE_AUTH_TOKEN"}}
	rec := New(Deps{
		Cfg:          &config.Config{Webhooks: map[string]config.Webhook{"w": hook}},
		Runner:       fr,
		EnvAllowlist: map[string]bool{"LOOMCYCLE_AUTH_TOKEN": true},
		Getenv:       mapGetenv(map[string]string{"LOOMCYCLE_AUTH_TOKEN": opToken}),
	})
	code, body := postWith(rec, "/v1/_webhooks/w", opToken, []byte(`{}`))
	if code != http.StatusServiceUnavailable || fr.wasCalled() {
		t.Fatalf("a delivery signed with the operator bearer: %d %s (run started: %t), want 503 and no run", code, body, fr.wasCalled())
	}
	warns := UnresolvableStaticSecrets(rec.cfg, rec.envAllowlist, rec.getenv)
	if len(warns) != 1 || !strings.Contains(warns[0], "loomcycle's own secrets") {
		t.Errorf("boot warnings %v, want one naming it as loomcycle's own secret", warns)
	}
}
