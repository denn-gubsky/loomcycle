package webhook

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A GitHub push body: what the push shape needs, and nothing a
// pull_request needs.
const pushBody = `{"ref":"refs/heads/main","before":"aaa","after":"bbb","repository":{"id":42}}`

// githubPushHook is a definition with the github preset listing push: no
// auth header and no delivery header of its own, so the preset supplies them.
func githubPushHook(events ...string) config.Webhook {
	if len(events) == 0 {
		events = []string{"push"}
	}
	return config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x", Provider: "github", Events: events,
		Auth: config.WebhookAuth{SigningSecretEnv: "WH_SECRET"}}
}

// githubDelivery signs body with the shared test secret, under the event
// headers given ("" for none).
func githubDelivery(body, delivery string, events ...string) http.Header {
	h := http.Header{}
	h.Set("X-Hub-Signature-256", githubSig(scopeSecret, []byte(body)))
	if delivery != "" {
		h.Set("X-GitHub-Delivery", delivery)
	}
	for _, e := range events {
		h.Add("X-GitHub-Event", e)
	}
	return h
}

func answer(t *testing.T, label string, rec *Receiver, path, body string, hdr http.Header, wantCode int) map[string]string {
	t.Helper()
	w := post(rec, path, []byte(body), hdr)
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != wantCode {
		t.Fatalf("%s: %d %s, want %d", label, w.Code, w.Body.String(), wantCode)
	}
	return got
}

// RFC verification, GitHub: a ping is answered and starts nothing, an event
// not listed is ignored, a listed event whose signed body has its shape
// starts one run, and the same body sent under another listed event's name
// is refused. All of it after the signature: an unsigned ping gets 401.
func TestReceiver_GitHubEventsAreAnsweredBeforeARunStarts(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubPushHook("push", "pull_request")})

	ping := `{"zen":"Keep it logically awesome.","hook_id":1}`
	if got := answer(t, "ping", rec, "/v1/_webhooks/gh", ping, githubDelivery(ping, "d1", "ping"), http.StatusOK); got["outcome"] != "ping" || got["event"] != "ping" {
		t.Errorf("ping answered %v", got)
	}
	unsigned := http.Header{"X-Github-Event": {"ping"}}
	answer(t, "an unsigned ping", rec, "/v1/_webhooks/gh", ping, unsigned, http.StatusUnauthorized)

	issue := `{"action":"opened","issue":{"number":1},"repository":{"id":42}}`
	if got := answer(t, "issues", rec, "/v1/_webhooks/gh", issue, githubDelivery(issue, "d2", "issues"), http.StatusAccepted); got["outcome"] != "ignored" {
		t.Errorf("an event not listed answered %v", got)
	}
	if fr.wasCalled() {
		t.Fatal("a ping or an ignored event started a run")
	}

	answer(t, "a push body sent as pull_request", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "d3", "pull_request"), http.StatusBadRequest)
	if fr.wasCalled() {
		t.Fatal("a body without the event's shape started a run")
	}
	if got := answer(t, "push", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "d4", "push"), http.StatusAccepted); got["run_id"] == "" {
		t.Errorf("a push started no run: %v", got)
	}
	// The preset's delivery header was read: the answer carries its id.
	if !fr.wasCalled() {
		t.Fatal("no run for the push")
	}
}

// A delivery whose event header is missing, repeated or not a short plain
// token is refused.
func TestReceiver_ABadEventHeaderIsRefused(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubPushHook()})
	for label, events := range map[string][]string{
		"no header":  nil,
		"two":        {"push", "push"},
		"not a name": {"push\x00"},
		"a list":     {"push,ping"},
	} {
		w := post(rec, "/v1/_webhooks/gh", []byte(pushBody), githubDelivery(pushBody, "", events...))
		if w.Code != http.StatusBadRequest || !jsonHas(w.Body.Bytes(), "error", "bad_event") {
			t.Errorf("%s: %d %s, want 400 bad_event", label, w.Code, w.Body.String())
		}
	}
	if fr.wasCalled() {
		t.Fatal("a delivery with a bad event header started a run")
	}
}

// An event answered without a run claims nothing: the same signed body sent
// afterwards as a listed event starts its run, not a duplicate's ack.
func TestReceiver_AnIgnoredDeliveryClaimsNoReplayKey(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubPushHook()})
	answer(t, "as create", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "same-id", "create"), http.StatusAccepted)
	got := answer(t, "as push", rec, "/v1/_webhooks/gh", pushBody, githubDelivery(pushBody, "same-id", "push"), http.StatusAccepted)
	if got["deduped"] == "true" || !fr.wasCalled() {
		t.Fatalf("the push after an ignored delivery of the same body: %v (run started: %t)", got, fr.wasCalled())
	}
}

// Gitea's preset: a bare-hex signature in X-Gitea-Signature, X-Gitea-Event.
// GitLab's: a shared token in X-Gitlab-Token, and its event names turned
// into the spelling a definition lists ("Push Hook" is push).
func TestReceiver_GiteaAndGitLabPresets(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	hooks := map[string]config.Webhook{
		"gitea": {Enabled: true, Delivery: "spawn", Agent: "x", Provider: "gitea", Events: []string{"push"},
			Auth: config.WebhookAuth{SigningSecretEnv: "WH_SECRET"}},
		"gitlab": {Enabled: true, Delivery: "spawn", Agent: "x", Provider: "gitlab", Events: []string{"push"},
			Auth: config.WebhookAuth{BearerTokenEnv: "WH_SECRET"}},
	}
	rec := newScopeReceiver(openScopeStore(t), fr, hooks)

	gitea := http.Header{}
	gitea.Set("X-Gitea-Signature", githubSig(scopeSecret, []byte(pushBody))[len("sha256="):])
	gitea.Set("X-Gitea-Event", "push")
	answer(t, "gitea push", rec, "/v1/_webhooks/gitea", pushBody, gitea, http.StatusAccepted)
	gitea.Set("X-Gitea-Event", "issues")
	answer(t, "gitea issues", rec, "/v1/_webhooks/gitea", pushBody, gitea, http.StatusAccepted)

	glPush := `{"object_kind":"push","ref":"refs/heads/main","before":"a","after":"b","project":{"id":7}}`
	gl := func(token, event string) http.Header {
		h := http.Header{}
		h.Set("X-Gitlab-Token", token)
		h.Set("X-Gitlab-Event", event)
		return h
	}
	fr.reset()
	if got := answer(t, "gitlab Push Hook", rec, "/v1/_webhooks/gitlab", glPush, gl(scopeSecret, "Push Hook"), http.StatusAccepted); got["run_id"] == "" || !fr.wasCalled() {
		t.Errorf("a GitLab push started no run: %v", got)
	}
	fr.reset()
	if got := answer(t, "gitlab Merge Request Hook", rec, "/v1/_webhooks/gitlab", glPush, gl(scopeSecret, "Merge Request Hook"), http.StatusAccepted); got["outcome"] != "ignored" || got["event"] != "merge_request" {
		t.Errorf("an unlisted GitLab event answered %v", got)
	}
	answer(t, "gitlab wrong token", rec, "/v1/_webhooks/gitlab", glPush, gl("not-the-token", "Push Hook"), http.StatusUnauthorized)
	if fr.wasCalled() {
		t.Fatal("an ignored or unauthenticated GitLab delivery started a run")
	}
}

// Without a provider, a definition names its event header and gives each
// event its shape. And without events, a delivery is handled as before: a
// GitHub ping to a definition that lists none starts a run, as it always has.
func TestReceiver_OwnEventHeaderAndShapes_AndNoEventsIsUnchanged(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	own := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x", EventHeader: "X-Ci-Event",
		Events: []string{"build_done"}, EventShapes: map[string][]string{"build_done": {"$.build.id", "$.status"}},
		Auth: config.WebhookAuth{Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}}
	plain := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x", Provider: "github",
		Auth: config.WebhookAuth{SigningSecretEnv: "WH_SECRET"}}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"ci": own, "plain": plain})

	hdr := func(body string) http.Header {
		h := http.Header{}
		h.Set("X-Hub-Signature-256", githubSig(scopeSecret, []byte(body)))
		h.Set("X-Ci-Event", "build_done")
		return h
	}
	answer(t, "without the status path", rec, "/v1/_webhooks/ci", `{"build":{"id":1}}`, hdr(`{"build":{"id":1}}`), http.StatusBadRequest)
	answer(t, "a null status", rec, "/v1/_webhooks/ci", `{"build":{"id":1},"status":null}`, hdr(`{"build":{"id":1},"status":null}`), http.StatusBadRequest)
	answer(t, "the shape", rec, "/v1/_webhooks/ci", `{"build":{"id":1},"status":"ok"}`, hdr(`{"build":{"id":1},"status":"ok"}`), http.StatusAccepted)

	fr.reset()
	ping := `{"zen":"z","hook_id":1}`
	answer(t, "a ping with no events listed", rec, "/v1/_webhooks/plain", ping, githubDelivery(ping, "p1", "ping"), http.StatusAccepted)
	if !fr.wasCalled() {
		t.Error("a definition that lists no events no longer starts a run for every delivery")
	}
}

// The admin dry-run makes the same event check, so it never reports
// would_accept for a delivery the receiver answers without a run.
func TestReceiver_Test_ReportsTheEventOutcome(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubPushHook()})
	srv := triageServer(t, rec, triageTokens)
	for event, want := range map[string]string{"ping": "ping", "issues": "ignored", "push": ""} {
		w := triageDo(srv, http.MethodPost, "/v1/_webhooks/gh/test", triageAdminBearer, []byte(pushBody), githubDelivery(pushBody, "", event))
		var res testResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("%s: %d %s", event, w.Code, w.Body.String())
		}
		if want == "" {
			if !res.WouldAccept {
				t.Errorf("push: %+v, want would_accept", res)
			}
		} else if res.WouldAccept || res.Verdict != want {
			t.Errorf("%s: %+v, want verdict %s", event, res, want)
		}
	}
	if fr.wasCalled() {
		t.Fatal("the dry-run started a run")
	}
}

func jsonHas(body []byte, key, want string) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	v, _ := m[key].(string)
	return v == want
}
