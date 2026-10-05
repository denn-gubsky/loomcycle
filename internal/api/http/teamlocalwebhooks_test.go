package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/webhook"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// These tests deliver to a team's own webhook through the real receiver and
// read what the store holds: a delivery is a message in the team's own
// channel, and none lands once no walk of the team is running.

const hookSecret = "team-hook-secret"

// hookedTeamJSON declares its own `events` channel (scope given) and a
// `github` webhook publishing into it; its entry Starter reads `events`,
// waiting waitMS. extra is spliced into the webhook's body.
func hookedTeamJSON(scope string, waitMS int, extra string) string {
	return `{"entry":"wave",
	  "local":{"channels":{"events":{"scope":"` + scope + `"}},
	    "webhooks":{"github":{"channel":"./events"` + extra + `,
	      "auth":{"signing_secret_env":"LOOMCYCLE_TEAM_HOOK_SECRET","header":"X-Hub-Signature-256","delivery_id_header":"X-Delivery"}}}},
	  "states":[{"state":"wave","handler":{"kind":"starter","source":{"channel":"./events","wait_ms":` + strconv.Itoa(waitMS) + `},
	    "fanout":{"agent":"reviewer","per":"message","max":1}}},
	    {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"wave","to":"done","on":"success"}]}`
}

// hookHarness is a channel harness whose server receives a team's own
// webhooks, behind the real receiver's routes.
type hookHarness struct {
	*channelHarness
	mux *http.ServeMux
}

func newHookHarness(t *testing.T) *hookHarness {
	t.Helper()
	h := newChannelHarness(t, nil)
	h.srv.cfg().Env.ChannelsLongPollCapMS = 30000
	return &hookHarness{channelHarness: h, mux: receiverFor(h.srv)}
}

// receiverFor mounts a webhook receiver resolving srv's team webhooks, with
// the team webhook secret set in its environment.
func receiverFor(srv *Server) *http.ServeMux {
	rec := webhook.New(webhook.Deps{
		Cfg:          srv.cfg(),
		TeamWebhooks: srv.ReceiveTeamWebhooks(),
		Getenv: func(name string) string {
			if name == "LOOMCYCLE_TEAM_HOOK_SECRET" {
				return hookSecret
			}
			return ""
		},
	})
	mux := http.NewServeMux()
	rec.Mount(mux)
	return mux
}

// deliverToTeam POSTs body to path, signed with the team's secret when sign, with
// delivery id did.
func deliverToTeam(mux *http.ServeMux, path, body, did string, sign bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	if sign {
		mac := hmac.New(sha256.New, []byte(hookSecret))
		mac.Write([]byte(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	if did != "" {
		req.Header.Set("X-Delivery", did)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

const hookPath = "/v1/_teams/acme/hooked/webhooks/github"

// waitArmed blocks until n webhook registrations are live.
func (h *hookHarness) waitArmed(n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.srv.teamHooks.count() == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("%d webhook registration(s) live, want %d", h.srv.teamHooks.count(), n)
}

// notFound is what the receiver answers for a WebhookDef that does not exist
// — the answer a team's webhook that cannot take a delivery must give too.
func notFound(t *testing.T, mux *http.ServeMux) *httptest.ResponseRecorder {
	t.Helper()
	w := deliverToTeam(mux, "/v1/_webhooks/acme/no-such-webhook", `{}`, "", true)
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown WebhookDef: %d %s", w.Code, w.Body.String())
	}
	return w
}

func sameResponse(a, b *httptest.ResponseRecorder) bool {
	return a.Code == b.Code && a.Body.String() == b.Body.String() && a.Header().Get("Content-Type") == b.Header().Get("Content-Type")
}

// A running walk's webhook takes a signed delivery into the team's own
// channel, the walk's Starter reads it, and once the walk is over the webhook
// answers as one that does not exist.
func TestTeamLocalWebhook_ARunningWalksWebhookFeedsItsStarterAndCloses(t *testing.T) {
	h := newHookHarness(t)
	h.seed("tdf_hooked_1", "hooked", hookedTeamJSON("tenant", 30000, ""))

	runID := h.detach(acmeUser("alice"), "hooked")
	h.waitArmed(1)
	w := deliverToTeam(h.mux, hookPath, `{"pr": 7}`, "d-1", true)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"channel":"./events"`) {
		t.Fatalf("a signed delivery to a running walk's webhook: %d %s", w.Code, w.Body.String())
	}
	if st := h.waitRun(runID); st != store.RunCompleted {
		t.Fatalf("the walk ended %s, want completed (its Starter fed by the delivery)", st)
	}
	got := h.stored("_team/hooked/events", store.MemoryScopeTenant, "")
	if len(got) != 1 || string(got[0].Payload) != `{"pr": 7}` || got[0].PublishedByUserID != "alice" {
		t.Fatalf("the team's channel holds %+v, want the raw body once, attributed to the walk's user", got)
	}
	if n := h.srv.teamHooks.count(); n != 0 {
		t.Errorf("%d webhook registration(s) left after the walk ended", n)
	}
	after := deliverToTeam(h.mux, hookPath, `{"pr": 8}`, "d-2", true)
	if !sameResponse(after, notFound(t, h.mux)) {
		t.Errorf("after the walk ended the webhook answers %d %s, want the unknown-webhook answer", after.Code, after.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("a delivery after the walk ended landed: %d message(s)", len(got))
	}
}

// A walk that fails or is cancelled closes its webhook too.
func TestTeamLocalWebhook_ClosesWhenTheWalkFailsOrIsCancelled(t *testing.T) {
	h := newHookHarness(t)
	h.seed("tdf_hooked_1", "hooked", hookedTeamJSON("tenant", 50, ""))
	runID := h.detach(acmeUser("alice"), "hooked")
	if st := h.waitRun(runID); st != store.RunFailed {
		t.Fatalf("the walk ended %s, want failed (nothing arrived within its wait)", st)
	}
	h.waitArmed(0)

	h.seed("tdf_hooked_2", "hooked", hookedTeamJSON("tenant", 30000, ""))
	runID = h.detach(acmeUser("alice"), "hooked")
	h.waitArmed(1)
	alice := acmeUser("alice")(context.Background())
	if stopped, isWalk, err := h.srv.cancelTeamWalk(alice, runID, "enough"); !stopped || !isWalk || err != nil {
		t.Fatalf("cancel the walk: stopped=%v walk=%v err=%v", stopped, isWalk, err)
	}
	if st := h.waitRun(runID); st != store.RunCancelled {
		t.Fatalf("the walk ended %s, want cancelled", st)
	}
	h.waitArmed(0)
	if w := deliverToTeam(h.mux, hookPath, `{}`, "d-1", true); !sameResponse(w, notFound(t, h.mux)) {
		t.Errorf("after a cancelled walk: %d %s", w.Code, w.Body.String())
	}
}

// Not running, unknown team, unknown webhook, retired version: one answer,
// byte for byte, signed or not — and nothing stored.
func TestTeamLocalWebhook_NotRunningUnknownAndRetiredAnswerAlike(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	want := notFound(t, h.mux)

	for _, sign := range []bool{true, false} {
		if w := deliverToTeam(h.mux, hookPath, `{}`, "", sign); !sameResponse(w, want) {
			t.Errorf("no walk running (signed=%v): %d %s", sign, w.Code, w.Body.String())
		}
	}
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	for what, path := range map[string]string{
		"unknown webhook":    "/v1/_teams/acme/hooked/webhooks/gitlab",
		"another tenant":     "/v1/_teams/globex/hooked/webhooks/github",
		"the shared tenant":  "/v1/_teams/hooked/webhooks/github",
		"unknown team":       "/v1/_teams/acme/nohook/webhooks/github",
		"a name that is not": "/v1/_teams/acme/hooked/webhooks/%2e%2e",
	} {
		for _, sign := range []bool{true, false} {
			if w := deliverToTeam(h.mux, path, `{}`, "", sign); !sameResponse(w, want) {
				t.Errorf("%s (signed=%v): %d %s", what, sign, w.Code, w.Body.String())
			}
		}
	}
	if w := deliverToTeam(h.mux, hookPath, `{}`, "d-live", true); w.Code != http.StatusAccepted {
		t.Fatalf("the armed webhook itself: %d %s", w.Code, w.Body.String())
	}
	if err := h.st.TeamDefSetRetired(context.Background(), sc.DefID, true); err != nil {
		t.Fatal(err)
	}
	for _, sign := range []bool{true, false} {
		if w := deliverToTeam(h.mux, hookPath, `{"retired":1}`, "d-retired", sign); !sameResponse(w, want) {
			t.Errorf("a retired version (signed=%v): %d %s", sign, w.Code, w.Body.String())
		}
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s) stored, want only the armed, live delivery", len(got))
	}
}

// Unsigned and badly signed deliveries are the receiver's opaque 401, and
// publish nothing; a redelivery is an idempotent ack and publishes nothing.
func TestTeamLocalWebhook_VerifiesAndDedupsLikeAWebhookDef(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()

	if w := deliverToTeam(h.mux, hookPath, `{"pr":1}`, "d-1", false); w.Code != http.StatusUnauthorized || w.Body.String() != "{\"error\":\"unauthorized\"}\n" {
		t.Errorf("unsigned: %d %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, hookPath, strings.NewReader(`{"pr":1}`))
	req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || w.Body.String() != "{\"error\":\"unauthorized\"}\n" {
		t.Errorf("badly signed: %d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 0 {
		t.Fatalf("an unauthenticated delivery published %d message(s)", len(got))
	}
	if w := deliverToTeam(h.mux, hookPath, `{"pr":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("signed: %d %s", w.Code, w.Body.String())
	}
	if w := deliverToTeam(h.mux, hookPath, `{"pr":1}`, "d-1", true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deduped":"true"`) {
		t.Errorf("a redelivery: %d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s), want one per delivery", len(got))
	}
}

// Two walks of one team running at once: one message per delivery, published
// as the walk that armed first; when it ends, as the next.
func TestTeamLocalWebhook_TwoWalksGetOneMessagePerDelivery(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	def := mustTeamDef(t, raw)
	disarmA, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), def)
	if err != nil {
		t.Fatal(err)
	}
	disarmB, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_b", "bob"), def)
	if err != nil {
		t.Fatal(err)
	}
	defer disarmB()
	if w := deliverToTeam(h.mux, hookPath, `{"n":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	got := h.stored("_team/hooked/events", store.MemoryScopeTenant, "")
	if len(got) != 1 || got[0].PublishedByUserID != "alice" {
		t.Fatalf("two walks: %+v, want one message, as the earliest-armed walk's user", got)
	}
	disarmA()
	if w := deliverToTeam(h.mux, hookPath, `{"n":2}`, "d-2", true); w.Code != http.StatusAccepted {
		t.Fatalf("with one walk left: %d %s", w.Code, w.Body.String())
	}
	got = h.stored("_team/hooked/events", store.MemoryScopeTenant, "")
	if len(got) != 2 || (got[0].PublishedByUserID != "bob" && got[1].PublishedByUserID != "bob") {
		t.Errorf("after the first walk ended: %+v, want the second message as the remaining walk's user", got)
	}
}

// A user-scoped channel's webhook files each delivery under the user its
// payload names, whoever's walk armed it; a delivery naming no user is
// refused and stores nothing. A tenant-scoped one is attributed to the
// payload's user when it names one.
func TestTeamLocalWebhook_UserIDMappingKeysAndAttributes(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("user", 30000, `,"payload_mapping":{"user_id":"$.sender.login"}`)
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	if w := deliverToTeam(h.mux, hookPath, `{"sender":{"login":"bob"}}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeUser, "bob"); len(got) != 1 || got[0].PublishedByUserID != "bob" {
		t.Errorf("bob's delivery under bob: %+v", got)
	}
	if w := deliverToTeam(h.mux, hookPath, `{"sender":{}}`, "d-2", true); w.Code != http.StatusBadRequest {
		t.Errorf("a delivery naming no user, to a user-scoped channel: %d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeUser, "alice"); len(got) != 0 {
		t.Errorf("a delivery naming no user was filed under the walk's user: %+v", got)
	}
	disarm()

	tenantRaw := hookedTeamJSON("tenant", 30000, `,"payload_mapping":{"user_id":"$.sender.login"}`)
	sc2 := h.seed("tdf_hooked_2", "hooked", tenantRaw)
	disarm, err = h.srv.armWalkTriggers(schedWalkCtx(sc2, "r_b", "alice"), mustTeamDef(t, tenantRaw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()
	if w := deliverToTeam(h.mux, hookPath, `{"sender":{"login":"carol"}}`, "d-3", true); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 || got[0].PublishedByUserID != "carol" {
		t.Errorf("a tenant channel's delivery attributed to the payload's user: %+v", got)
	}
}

// A delivery resolved before the walk ended and published after finds the
// webhook gone: nothing lands once the disarm has returned.
func TestTeamLocalWebhook_NothingPublishesAfterTheDisarm(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	hook, ok := h.srv.ResolveTeamWebhook(context.Background(), "acme", "hooked", "github")
	if !ok {
		t.Fatal("the armed webhook does not resolve")
	}
	disarm()
	if err := hook.Publish(context.Background(), "", json.RawMessage(`{"late":true}`)); !errors.Is(err, runner.ErrTeamWebhookGone) {
		t.Fatalf("a publish after the disarm: %v, want ErrTeamWebhookGone", err)
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 0 {
		t.Errorf("%d message(s) landed after the disarm", len(got))
	}
}

// A walk whose webhooks could not be opened is refused at arm time, and opens
// nothing: on a server that does not receive webhooks, in another tenant than
// the team's, outside a walk, and for a user-scoped channel with no user
// mapping (a stored body that skipped the authoring check).
func TestArmWalkTriggers_RefusesWebhooksItCannotOpen(t *testing.T) {
	h := newChannelHarness(t, nil)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	def := mustTeamDef(t, raw)
	if _, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), def); err == nil || !strings.Contains(err.Error(), "does not receive webhooks") {
		t.Fatalf("a server without the receiver: want a refusal, got %v", err)
	}
	h.srv.ReceiveTeamWebhooks()
	userRaw := hookedTeamJSON("user", 30000, "")
	for what, tc := range map[string]struct {
		ctx context.Context
		raw string
	}{
		"another tenant's walk": {tools.WithRunIdentity(schedWalkCtx(sc, "r_a", "alice"),
			tools.RunIdentityValue{AgentID: "team:hooked", TenantID: "globex", UserID: "alice"}), raw},
		"outside every walk":             {tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "alice"}), raw},
		"a user channel with no mapping": {schedWalkCtx(sc, "r_a", "alice"), userRaw},
	} {
		if _, err := h.srv.armWalkTriggers(tc.ctx, mustTeamDef(t, tc.raw)); err == nil {
			t.Errorf("%s: want a refusal", what)
		}
	}
	if n := h.srv.teamHooks.count(); n != 0 {
		t.Errorf("a refused arm left %d webhook registration(s)", n)
	}
}

// A webhook answers only on the replica whose walk armed it. Another replica,
// sharing the store, answers as for a webhook that does not exist; what the
// first accepted is a stored message every replica reads.
func TestTeamLocalWebhook_AnswersOnlyOnTheReplicaRunningTheWalk(t *testing.T) {
	h := newHookHarness(t)
	raw := hookedTeamJSON("tenant", 30000, "")
	sc := h.seed("tdf_hooked_1", "hooked", raw)
	disarm, err := h.srv.armWalkTriggers(schedWalkCtx(sc, "r_a", "alice"), mustTeamDef(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	defer disarm()

	other := New(h.srv.cfg(), &stubResolver{p: h.prov}, nil, concurrency.New(8, 8, 5*time.Second), h.st)
	other.SetSystemPublisher(h.pub)
	otherMux := receiverFor(other)
	if w := deliverToTeam(otherMux, hookPath, `{"n":1}`, "d-1", true); !sameResponse(w, notFound(t, otherMux)) {
		t.Errorf("the replica with no walk of the team: %d %s", w.Code, w.Body.String())
	}
	if w := deliverToTeam(h.mux, hookPath, `{"n":1}`, "d-1", true); w.Code != http.StatusAccepted {
		t.Fatalf("the replica running the walk: %d %s", w.Code, w.Body.String())
	}
	if got := h.stored("_team/hooked/events", store.MemoryScopeTenant, ""); len(got) != 1 {
		t.Errorf("%d message(s), want the one delivery the walk's replica took", len(got))
	}
}
