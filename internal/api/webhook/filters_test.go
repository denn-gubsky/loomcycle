package webhook

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// githubBranchPushHook is the RFC's verification definition: github push
// events, to a branch, that are not a deletion, of the repository its
// metadata names.
func githubBranchPushHook() config.Webhook {
	w := githubPushHook()
	w.Metadata = map[string]any{"repo": "acme/app"}
	w.Filters = []config.WebhookFilter{
		{Path: "$.ref", Op: "glob", Value: "refs/heads/*"},
		{Path: "$.deleted", Op: "eq", Value: false},
		{Path: "$.repository.full_name", Op: "eq", ValueFrom: "metadata.repo"},
	}
	return w
}

func pushTo(ref string, deleted bool, repo string) string {
	b, _ := json.Marshal(map[string]any{
		"ref": ref, "before": "aaa", "after": "bbb", "deleted": deleted,
		"repository": map[string]any{"id": 42, "full_name": repo},
	})
	return string(b)
}

func answerAny(t *testing.T, label string, rec *Receiver, path, body string, hdr http.Header, wantCode int) map[string]any {
	t.Helper()
	w := post(rec, path, []byte(body), hdr)
	if w.Code != wantCode {
		t.Fatalf("%s: %d %s, want %d", label, w.Code, w.Body.String(), wantCode)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return got
}

// RFC verification: a push to a tag, a branch deletion and a push to
// another repository are answered 202 filtered, naming the filter that
// missed, and start nothing; a push to a branch starts its run.
func TestReceiver_AFilteredDeliveryStartsNothing(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubBranchPushHook()})

	for i, tc := range []struct {
		label, body string
		filter      float64
	}{
		{"a tag", pushTo("refs/tags/v1", false, "acme/app"), 0},
		{"a branch deletion", pushTo("refs/heads/old", true, "acme/app"), 1},
		{"another repository", pushTo("refs/heads/main", false, "acme/web"), 2},
	} {
		got := answerAny(t, tc.label, rec, "/v1/_webhooks/gh", tc.body, githubDelivery(tc.body, "d"+string(rune('0'+i)), "push"), http.StatusAccepted)
		if got["outcome"] != "filtered" || got["filter"] != tc.filter || got["event"] != "push" {
			t.Errorf("%s: answered %v, want filtered at filter %v", tc.label, got, tc.filter)
		}
	}
	if fr.wasCalled() {
		t.Fatal("a filtered delivery started a run")
	}
	body := pushTo("refs/heads/main", false, "acme/app")
	if got := answerAny(t, "a branch push", rec, "/v1/_webhooks/gh", body, githubDelivery(body, "d9", "push"), http.StatusAccepted); got["run_id"] == nil || !fr.wasCalled() {
		t.Fatalf("a branch push started no run: %v", got)
	}
}

// A filtered delivery claims no replay key: a delivery with its id that the
// filters let through starts its run, not a duplicate's ack.
func TestReceiver_AFilteredDeliveryClaimsNoReplayKey(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubBranchPushHook()})
	tag := pushTo("refs/tags/v1", false, "acme/app")
	answerAny(t, "the tag", rec, "/v1/_webhooks/gh", tag, githubDelivery(tag, "same-id", "push"), http.StatusAccepted)
	branch := pushTo("refs/heads/main", false, "acme/app")
	got := answerAny(t, "the branch", rec, "/v1/_webhooks/gh", branch, githubDelivery(branch, "same-id", "push"), http.StatusAccepted)
	if got["deduped"] == "true" || !fr.wasCalled() {
		t.Fatalf("the delivery after a filtered one with its id: %v (run started: %t)", got, fr.wasCalled())
	}
}

// Filters need no events: a definition for any sender filters its body. A
// body that is not JSON is refused as malformed, as the mapping refuses it.
func TestReceiver_FiltersWithoutEvents(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	hook := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "x",
		Auth:    config.WebhookAuth{Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"},
		Filters: []config.WebhookFilter{{Path: "$.status", Op: "in", Value: []any{"failed", "errored"}}}}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"ci": hook})
	signed := func(body string) http.Header {
		h := http.Header{}
		h.Set("X-Hub-Signature-256", githubSig(scopeSecret, []byte(body)))
		return h
	}
	ok := `{"status":"ok"}`
	if got := answerAny(t, "a passing build", rec, "/v1/_webhooks/ci", ok, signed(ok), http.StatusAccepted); got["outcome"] != "filtered" {
		t.Errorf("answered %v, want filtered", got)
	}
	if _, has := answerAny(t, "again", rec, "/v1/_webhooks/ci", ok, signed(ok), http.StatusAccepted)["event"]; has {
		t.Error("a definition without events answered with an event")
	}
	notJSON := `status=failed`
	w := post(rec, "/v1/_webhooks/ci", []byte(notJSON), signed(notJSON))
	if w.Code != http.StatusBadRequest || !jsonHas(w.Body.Bytes(), "error", "bad_mapping") {
		t.Errorf("a body that is not JSON: %d %s, want 400 bad_mapping", w.Code, w.Body.String())
	}
	if fr.wasCalled() {
		t.Fatal("a filtered or malformed delivery started a run")
	}
	failed := `{"status":"failed"}`
	answerAny(t, "a failed build", rec, "/v1/_webhooks/ci", failed, signed(failed), http.StatusAccepted)
	if !fr.wasCalled() {
		t.Fatal("a failed build started no run")
	}
}

// The filters run after the signature: an unsigned body a filter would miss
// is refused with the unknown-name 404, and learns nothing about the filters.
func TestReceiver_FiltersRunAfterTheSignature(t *testing.T) {
	rec := newScopeReceiver(openScopeStore(t), &fakeRunner{runID: "r"}, map[string]config.Webhook{"gh": githubBranchPushHook()})
	tag := pushTo("refs/tags/v1", false, "acme/app")
	h := githubDelivery(tag, "d1", "push")
	h.Set("X-Hub-Signature-256", githubSig("not-the-secret", []byte(tag)))
	answerAny(t, "unsigned", rec, "/v1/_webhooks/gh", tag, h, http.StatusNotFound)
}

// The dry-run makes the same check and names the filter that missed.
func TestReceiver_Test_ReportsTheFilterThatMissed(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": githubBranchPushHook()})
	srv := triageServer(t, rec, triageTokens)
	for body, want := range map[string]int{
		pushTo("refs/heads/main", true, "acme/app"):  1,
		pushTo("refs/heads/main", false, "acme/app"): -1,
	} {
		w := triageDo(srv, http.MethodPost, "/v1/_webhooks/gh/test", triageAdminBearer, []byte(body), githubDelivery(body, "", "push"))
		var res testResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		switch {
		case want < 0 && (!res.WouldAccept || res.Filter != nil):
			t.Errorf("%s: %+v, want would_accept", body, res)
		case want >= 0 && (res.WouldAccept || res.Verdict != "filtered" || res.Filter == nil || *res.Filter != want):
			t.Errorf("%s: %+v, want filtered at %d", body, res, want)
		}
	}
	if fr.wasCalled() {
		t.Fatal("the dry-run started a run")
	}
}
