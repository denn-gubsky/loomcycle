package webhook

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// These tests retire a webhook through the REAL sqlite store, whose
// WebhookDefSetRetired flips the row and leaves the active pointer in place —
// the state a stub store would have to guess at.

// agentRunner is storeRunner plus a record of which agent each run was for,
// so a test can tell which def a delivery resolved to.
type agentRunner struct {
	storeRunner
	agents []string
}

func (r *agentRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	r.mu.Lock()
	r.agents = append(r.agents, in.Agent)
	r.mu.Unlock()
	return r.storeRunner.RunOnce(ctx, in, cb)
}

// retireActive retires (or un-retires) the active version of tenant/name.
func retireActive(t *testing.T, st store.Store, tenant, name string, retired bool) {
	t.Helper()
	ctx := context.Background()
	row, err := st.WebhookDefGetActive(ctx, tenant, name)
	if err != nil {
		t.Fatalf("WebhookDefGetActive(%s/%s): %v", tenant, name, err)
	}
	if err := st.WebhookDefSetRetired(ctx, row.DefID, retired); err != nil {
		t.Fatalf("WebhookDefSetRetired(%s/%s, %v): %v", tenant, name, retired, err)
	}
}

func TestReceiver_RetiredWebhook_AnsweredLikeAnUnknownOne(t *testing.T) {
	st := openScopeStore(t)
	fr := &storeRunner{st: st}
	rec := newScopeReceiver(st, fr, nil)
	putWebhookDef(t, st, "acme", "gh", signedSpawnDef("acme"))
	putWebhookDef(t, st, "", "base", signedSpawnDef(""))

	for _, path := range []string{"/v1/_webhooks/acme/gh", "/v1/_webhooks/base"} {
		code, got := postSigned(t, rec, path, []byte(`{"n":1}`), "")
		assertFreshRun(t, path+" before retire", code, got)
	}
	unknownCode, unknown := postSigned(t, rec, "/v1/_webhooks/acme/never-registered", []byte(`{"n":2}`), "")
	if unknownCode != http.StatusNotFound {
		t.Fatalf("precondition: unknown webhook status = %d, want 404", unknownCode)
	}

	retireActive(t, st, "acme", "gh", true)
	retireActive(t, st, "", "base", true)
	for _, path := range []string{"/v1/_webhooks/acme/gh", "/v1/_webhooks/base"} {
		code, got := postSigned(t, rec, path, []byte(`{"n":2}`), "")
		if code != unknownCode || !reflect.DeepEqual(got, unknown) {
			t.Errorf("%s after retire: %d %v, want the unknown-webhook answer %d %v", path, code, got, unknownCode, unknown)
		}
	}
	if n := fr.callCount(); n != 2 {
		t.Errorf("runs started = %d, want 2: a retired webhook must not start one", n)
	}

	retireActive(t, st, "acme", "gh", false)
	retireActive(t, st, "", "base", false)
	for _, path := range []string{"/v1/_webhooks/acme/gh", "/v1/_webhooks/base"} {
		code, got := postSigned(t, rec, path, []byte(`{"n":3}`), "")
		assertFreshRun(t, path+" after un-retire", code, got)
	}
}

func TestReceiver_RetiredTenantOverride_ServedByTheStaticWebhook(t *testing.T) {
	st := openScopeStore(t)
	fr := &agentRunner{storeRunner: storeRunner{st: st}}
	static := scopeWebhook("")
	static.Agent = "static-agent"
	rec := newScopeReceiver(st, fr, map[string]config.Webhook{"gh": static})
	override := signedSpawnDef("acme")
	override["agent"] = "override-agent"
	putWebhookDef(t, st, "acme", "gh", override)

	code, got := postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"n":1}`), "")
	assertFreshRun(t, "override live", code, got)
	retireActive(t, st, "acme", "gh", true)
	code, got = postSigned(t, rec, "/v1/_webhooks/acme/gh", []byte(`{"n":2}`), "")
	assertFreshRun(t, "override retired", code, got)

	if want := []string{"override-agent", "static-agent"}; !reflect.DeepEqual(fr.agents, want) {
		t.Errorf("agents run = %v, want %v", fr.agents, want)
	}
}

// The admin dry-run must not promise a delivery the receiver would refuse.
func TestTriageTest_RetiredWebhook_NotAddressable(t *testing.T) {
	st := openScopeStore(t)
	rec := newScopeReceiver(st, &storeRunner{st: st}, nil)
	putWebhookDef(t, st, "", "base", signedSpawnDef(""))
	h := triageServer(t, rec, triageOpen)
	body := []byte(`{"n":1}`)
	hdr := http.Header{"X-Hub-Signature-256": {githubSig(scopeSecret, body)}}

	if w := triageDo(h, http.MethodPost, "/v1/_webhooks/base/test", "", body, hdr); w.Code != http.StatusOK {
		t.Fatalf("precondition: dry-run of a live webhook = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	retireActive(t, st, "", "base", true)
	if w := triageDo(h, http.MethodPost, "/v1/_webhooks/base/test", "", body, hdr); w.Code != http.StatusNotFound {
		t.Errorf("dry-run of a retired webhook = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
