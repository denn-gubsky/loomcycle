package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// fakeTeamHooks serves the team webhooks it holds, keyed tenant/team/name,
// and records each publish.
type fakeTeamHooks struct {
	mu        sync.Mutex
	hooks     map[string]runner.TeamWebhook
	published []teamPublish
	resolved  []string
	keys      [][]string // the dedup keys of every Publish call
	err       error      // returned by Publish when set
}

type teamPublish struct {
	key, userID string
	body        string
}

func (f *fakeTeamHooks) ResolveTeamWebhook(_ context.Context, tenant, team, name string) (runner.TeamWebhook, bool) {
	key := tenant + "/" + team + "/" + name
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, key)
	h, ok := f.hooks[key]
	if !ok {
		return runner.TeamWebhook{}, false
	}
	h.Publish = func(_ context.Context, userID string, keys []string, body json.RawMessage) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys = append(f.keys, keys)
		if f.err != nil {
			return f.err
		}
		f.published = append(f.published, teamPublish{key: key, userID: userID, body: string(body)})
		return nil
	}
	return h, true
}

func (f *fakeTeamHooks) publishes() []teamPublish {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]teamPublish(nil), f.published...)
}

var teamNow = time.Unix(1_700_000_000, 0)

// newLocalHookReceiver serves the team webhooks given plus the global webhooks
// given, with WH_SECRET="shhh" set and allowlisted.
func newLocalHookReceiver(t *testing.T, hooks map[string]runner.TeamWebhook, global map[string]config.Webhook) (*Receiver, *fakeTeamHooks, *fakePublisher) {
	t.Helper()
	th := &fakeTeamHooks{hooks: hooks}
	fp := &fakePublisher{}
	rec := New(Deps{
		Cfg:          &config.Config{Webhooks: global},
		Publisher:    fp,
		TeamWebhooks: th,
		EnvAllowlist: map[string]bool{"WH_SECRET": true},
		Now:          fixedClock(teamNow),
		Getenv:       mapGetenv(map[string]string{"WH_SECRET": "shhh"}),
	})
	return rec, th, fp
}

func ghHook() runner.TeamWebhook {
	return runner.TeamWebhook{
		Auth:    config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery"},
		Channel: "./events",
	}
}

func post(rec *Receiver, path string, body []byte, hdr http.Header) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	rec.Mount(mux)
	req := httptest.NewRequest(http.MethodPost, path, bytesReader(body))
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func signed(body []byte, delivery string) http.Header {
	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig("shhh", body))
	if delivery != "" {
		h.Set("X-Delivery", delivery)
	}
	return h
}

func TestTeamWebhook_SignedDeliveryPublishesTheRawBodyOnce(t *testing.T) {
	rec, th, fp := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"acme/triage/github": ghHook()}, nil)
	body := []byte(`{"pr": 7}`)
	w := post(rec, "/v1/_teams/acme/triage/webhooks/github", body, signed(body, "d-1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["webhook_name"] != "github" || resp["delivery_id"] != "d-1" || resp["channel"] != "./events" {
		t.Errorf("response = %v", resp)
	}
	got := th.publishes()
	if len(got) != 1 || got[0].key != "acme/triage/github" || got[0].body != `{"pr": 7}` || got[0].userID != "" {
		t.Fatalf("publishes = %+v, want the raw body once", got)
	}
	if fp.called {
		t.Error("a team's webhook published through the global channel publisher")
	}
}

// The bare route is the shared "" tenant's; the prefixed one names a tenant,
// matched exactly.
func TestTeamWebhook_RouteTenantIsMatchedExactly(t *testing.T) {
	rec, th, _ := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"/triage/github": ghHook()}, nil)
	body := []byte(`{}`)
	if w := post(rec, "/v1/_teams/triage/webhooks/github", body, signed(body, "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("the shared tenant's team at the bare route: %d %s", w.Code, w.Body.String())
	}
	if w := post(rec, "/v1/_teams/acme/triage/webhooks/github", body, signed(body, "d-2")); w.Code != http.StatusNotFound {
		t.Fatalf("another tenant's route must not reach the shared team's webhook: %d", w.Code)
	}
	if got := th.resolved; len(got) != 2 || got[0] != "/triage/github" || got[1] != "acme/triage/github" {
		t.Errorf("resolved %v", got)
	}
}

// Unknown team, unknown webhook, no walk running, retired: the resolver says
// no for all, and the answer is byte-identical to a WebhookDef that does not
// exist — before the body is read or the signature checked.
func TestTeamWebhook_UnresolvedIsByteIdenticalToAnUnknownWebhookDef(t *testing.T) {
	rec, th, _ := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"acme/triage/github": ghHook()}, nil)
	body := []byte(`{"pr": 7}`)
	want := post(rec, "/v1/_webhooks/acme/nosuch", body, signed(body, ""))
	if want.Code != http.StatusNotFound {
		t.Fatalf("an unknown WebhookDef: %d", want.Code)
	}
	for _, path := range []string{
		"/v1/_teams/acme/nosuch/webhooks/github",
		"/v1/_teams/acme/triage/webhooks/nosuch",
		"/v1/_teams/globex/triage/webhooks/github",
		"/v1/_teams/triage/webhooks/github",
	} {
		for _, hdr := range []http.Header{signed(body, ""), {}} {
			w := post(rec, path, body, hdr)
			if w.Code != want.Code || w.Body.String() != want.Body.String() || w.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
				t.Errorf("%s: %d %q, want %d %q", path, w.Code, w.Body.String(), want.Code, want.Body.String())
			}
		}
	}
	if len(th.publishes()) != 0 {
		t.Error("an unresolved webhook published")
	}
}

// Unsigned and badly signed deliveries get exactly what a WebhookDef's do.
func TestTeamWebhook_BadSignatureIsAnsweredAsAWebhookDefs(t *testing.T) {
	global := map[string]config.Webhook{"gh": {Enabled: true, Delivery: "channel", Channel: "c",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}}}
	rec, th, _ := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"/triage/github": ghHook()}, global)
	body := []byte(`{"pr": 7}`)
	badSig := http.Header{}
	badSig.Set("X-Hub-Signature-256", githubSig("not-the-secret", body))
	tampered := signed([]byte(`{"pr": 8}`), "")
	for what, hdr := range map[string]http.Header{"unsigned": {}, "wrong secret": badSig, "tampered": tampered} {
		want := post(rec, "/v1/_webhooks/gh", body, hdr)
		got := post(rec, "/v1/_teams/triage/webhooks/github", body, hdr)
		if got.Code != http.StatusUnauthorized || got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("%s: team %d %q, WebhookDef %d %q", what, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
	}
	if len(th.publishes()) != 0 {
		t.Error("a badly signed delivery published")
	}
}

// A secret the receiver will not resolve (not allowlisted, not LOOMCYCLE_*) is
// the operator's 503 naming the env var, as for a runtime WebhookDef; none
// auth is refused unless the operator allows unauthenticated webhooks.
func TestTeamWebhook_SecretAndNoneFollowTheReceiversRules(t *testing.T) {
	unlisted := ghHook()
	unlisted.Auth.SigningSecretEnv = "GITHUB_HOOK_SECRET"
	none := runner.TeamWebhook{Auth: config.WebhookAuth{Kind: "none"}, Channel: "./events"}
	rec, th, _ := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"/t/unlisted": unlisted, "/t/open": none}, nil)
	body := []byte(`{}`)
	w := post(rec, "/v1/_teams/t/webhooks/unlisted", body, signed(body, ""))
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "{\"error\":\"secret_unresolvable\",\"secret_env\":\"GITHUB_HOOK_SECRET\"}\n" {
		t.Errorf("an unallowlisted secret: %d %s", w.Code, w.Body.String())
	}
	if w := post(rec, "/v1/_teams/t/webhooks/open", body, nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("auth none without the operator's opt-in: %d %s", w.Code, w.Body.String())
	}
	rec.allowUnauthenticated = true
	if w := post(rec, "/v1/_teams/t/webhooks/open", body, nil); w.Code != http.StatusAccepted {
		t.Errorf("auth none with the operator's opt-in: %d %s", w.Code, w.Body.String())
	}
	if n := len(th.publishes()); n != 1 {
		t.Errorf("%d publish(es), want only the opted-in none delivery", n)
	}
}

// A redelivery to a team's webhook is an idempotent ack; the same delivery to
// another team's webhook of the same name, or to a WebhookDef of that name,
// is its own.
func TestTeamWebhook_DedupsWithinTheTeamOnly(t *testing.T) {
	global := map[string]config.Webhook{"github": {Enabled: true, Delivery: "channel", Channel: "c",
		Auth: config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery"}}}
	rec, th, fp := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"/triage/github": ghHook(), "/deploy/github": ghHook()}, global)
	body := []byte(`{"pr": 7}`)
	hdr := signed(body, "d-1")
	if w := post(rec, "/v1/_webhooks/github", body, hdr); w.Code != http.StatusAccepted || !fp.called {
		t.Fatalf("the WebhookDef's delivery: %d %s", w.Code, w.Body.String())
	}
	if w := post(rec, "/v1/_teams/triage/webhooks/github", body, hdr); w.Code != http.StatusAccepted {
		t.Fatalf("the same delivery to a team's webhook of the same name is its own: %d %s", w.Code, w.Body.String())
	}
	w := post(rec, "/v1/_teams/triage/webhooks/github", body, hdr)
	if w.Code != http.StatusOK || w.Body.String() != "{\"deduped\":\"true\",\"delivery_id\":\"d-1\",\"webhook_name\":\"github\"}\n" {
		t.Fatalf("a redelivery to the team's webhook: %d %s", w.Code, w.Body.String())
	}
	if w := post(rec, "/v1/_teams/deploy/webhooks/github", body, hdr); w.Code != http.StatusAccepted {
		t.Fatalf("another team's webhook of the same name is its own: %d %s", w.Code, w.Body.String())
	}
	got := th.publishes()
	if len(got) != 2 || got[0].key != "/triage/github" || got[1].key != "/deploy/github" {
		t.Errorf("publishes = %+v, want one per team", got)
	}
	if teamWebhookKey("", "triage", "github") == webhookKey(lookup.WebhookOwner{}, "github") {
		t.Error("a team webhook's key equals a WebhookDef's")
	}
}

// The user the payload names is the publish's; a body that is not JSON is
// refused before anything publishes; a user-scoped channel's webhook with no
// user in the body is a 400, a webhook whose walk just ended a 404.
func TestTeamWebhook_UserMappingAndPublishRefusals(t *testing.T) {
	mapped := ghHook()
	mapped.PayloadMapping = map[string]string{"user_id": "$.sender.login"}
	rec, th, _ := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"/t/github": mapped}, nil)
	body := []byte(`{"sender":{"login":"octo"}}`)
	if w := post(rec, "/v1/_teams/t/webhooks/github", body, signed(body, "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := th.publishes(); len(got) != 1 || got[0].userID != "octo" {
		t.Errorf("publishes = %+v, want it attributed to the mapped user", got)
	}
	notJSON := []byte(`not json`)
	if w := post(rec, "/v1/_teams/t/webhooks/github", notJSON, signed(notJSON, "d-2")); w.Code != http.StatusBadRequest {
		t.Errorf("a body that is not JSON: %d", w.Code)
	}
	for err, want := range map[error]int{
		runner.ErrTeamWebhookNeedsUser: http.StatusBadRequest,
		runner.ErrTeamWebhookGone:      http.StatusNotFound,
		context.DeadlineExceeded:       http.StatusServiceUnavailable,
	} {
		th.mu.Lock()
		th.err = err
		th.mu.Unlock()
		b := []byte(`{"n":"` + err.Error() + `"}`)
		w := post(rec, "/v1/_teams/t/webhooks/github", b, signed(b, err.Error()))
		if w.Code != want {
			t.Errorf("%v: %d %s, want %d", err, w.Code, w.Body.String(), want)
		}
		// Refused, so a retry is not a replay.
		th.mu.Lock()
		th.err = nil
		th.mu.Unlock()
		if w := post(rec, "/v1/_teams/t/webhooks/github", b, signed(b, err.Error())); w.Code != http.StatusAccepted {
			t.Errorf("%v: the retry after a refusal: %d %s", err, w.Code, w.Body.String())
		}
	}
}

// Without a resolver the team routes are not mounted at all.
func TestTeamWebhook_NoRouteWithoutAResolver(t *testing.T) {
	rec := New(Deps{Cfg: &config.Config{}})
	body := []byte(`{}`)
	if w := post(rec, "/v1/_teams/t/webhooks/github", body, nil); w.Code != http.StatusNotFound || w.Body.String() == "{\"error\":\"unknown_webhook\"}\n" {
		t.Errorf("a receiver with no team resolver answers the team route: %d %s", w.Code, w.Body.String())
	}
}

// A delivery the resolver reports already accepted elsewhere is the replay
// guard's idempotent ack.
func TestTeamWebhook_ADuplicateFromThePublishIsAnIdempotentAck(t *testing.T) {
	rec, th, _ := newLocalHookReceiver(t, map[string]runner.TeamWebhook{"acme/triage/github": ghHook()}, nil)
	th.err = runner.ErrTeamWebhookDuplicate
	body := []byte(`{"pr": 7}`)
	w := post(rec, "/v1/_teams/acme/triage/webhooks/github", body, signed(body, "d-1"))
	if w.Code != http.StatusOK || w.Body.String() != "{\"deduped\":\"true\",\"delivery_id\":\"d-1\",\"webhook_name\":\"github\"}\n" {
		t.Errorf("a duplicate: %d %s, want the idempotent ack", w.Code, w.Body.String())
	}
}

// The keys handed to the durable store are ones the signature covers — the
// sender's id only where the signature expires — scoped to the team webhook.
func TestTeamWebhook_DurableKeysAreTheSignedIdentities(t *testing.T) {
	key := teamWebhookKey("acme", "triage", "github")
	body := []byte(`{"pr": 7}`)
	hdr := func(sig string) func(string) string {
		return func(h string) string {
			switch h {
			case "X-Hub-Signature-256":
				return sig
			case "X-Delivery":
				return "d-1"
			}
			return ""
		}
	}
	auth := ghHook().Auth
	for what, tc := range map[string]struct {
		sig  string
		want []string
	}{
		"github sha256=": {githubSig("shhh", body), []string{dedupKey(key, bodyDeliveryID(body))}},
		"stripe t=,v1=":  {"t=1700000000, v1=00", []string{dedupKey(key, "d-1"), dedupKey(key, signedPayloadID("1700000000", body))}},
	} {
		get := hdr(tc.sig)
		env := signedEnvelope(auth, get)
		got := durableTeamKeys(newDeliveryKeys(key, deliveryID(auth, body, get), body, env), env)
		if len(got) != len(tc.want) {
			t.Errorf("%s: keys %q, want %q", what, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: keys %q, want %q", what, got, tc.want)
			}
		}
	}
	bearer := config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_SECRET", DeliveryIDHeader: "X-Delivery"}
	get := hdr("")
	env := signedEnvelope(bearer, get)
	if got := durableTeamKeys(newDeliveryKeys(key, deliveryID(bearer, body, get), body, env), env); len(got) != 1 || got[0] != dedupKey(key, "d-1") {
		t.Errorf("bearer: keys %q, want the sender's id alone", got)
	}
}
