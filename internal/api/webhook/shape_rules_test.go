package webhook

import (
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A signed issue_comment body re-sent as issues is refused: its shape says
// the body holds no comment. The issues body itself starts a run.
func TestReceiver_AnIssueCommentBodySentAsIssuesIsRefused(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubPushHook("issues")})
	comment := `{"action":"created","issue":{"number":1},"comment":{"id":9},"repository":{"id":42}}`
	w := post(rec, "/v1/_webhooks/gh", []byte(comment), githubDelivery(comment, "d1", "issues"))
	if w.Code != http.StatusBadRequest || !jsonHas(w.Body.Bytes(), "error", "event_shape_mismatch") {
		t.Fatalf("an issue_comment body sent as issues: %d %s, want 400 event_shape_mismatch", w.Code, w.Body.String())
	}
	if fr.wasCalled() {
		t.Fatal("the impostor started a run")
	}
	issue := `{"action":"opened","issue":{"number":1},"repository":{"id":42}}`
	if got := answer(t, "an issues body", rec, "/v1/_webhooks/gh", issue, githubDelivery(issue, "d2", "issues"), http.StatusAccepted); got["run_id"] == "" {
		t.Errorf("an issues body started no run: %v", got)
	}
}

// GitLab names the event in object_kind: a tag push sent on the Push Hook is
// refused, and a definition's own value rule is read the same way.
func TestReceiver_AValueRuleTellsGitLabEventsApart(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	own := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x", EventHeader: "X-Kind", Events: []string{"build"},
		EventShapes: map[string][]string{"build": {`$.kind == "build"`, "$.id", "!$.dry_run"}},
		Auth:        config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "WH_SECRET", Header: "X-Token"}}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{
		"gitlab": {Enabled: true, Delivery: "spawn", Agent: "x", Provider: "gitlab", Events: []string{"push"},
			Auth: config.WebhookAuth{BearerTokenEnv: "WH_SECRET"}},
		"own": own,
	})
	gl := http.Header{}
	gl.Set("X-Gitlab-Token", scopeSecret)
	gl.Set("X-Gitlab-Event", "Push Hook")
	tagPush := `{"object_kind":"tag_push","ref":"refs/tags/v1","before":"a","after":"b","project":{"id":7}}`
	if w := post(rec, "/v1/_webhooks/gitlab", []byte(tagPush), gl); w.Code != http.StatusBadRequest {
		t.Errorf("a tag push on the Push Hook: %d %s, want 400", w.Code, w.Body.String())
	}

	h := http.Header{}
	h.Set("X-Token", scopeSecret)
	h.Set("X-Kind", "build")
	for body, want := range map[string]int{
		`{"kind":"build","id":1}`:                http.StatusAccepted,
		`{"kind":"deploy","id":1}`:               http.StatusBadRequest,
		`{"kind":"build","id":1,"dry_run":true}`: http.StatusBadRequest,
		`{"kind":"build","id":1,"dry_run":null}`: http.StatusAccepted,
	} {
		if w := post(rec, "/v1/_webhooks/own", []byte(body), h); w.Code != want {
			t.Errorf("%s: %d %s, want %d", body, w.Code, w.Body.String(), want)
		}
	}
}
