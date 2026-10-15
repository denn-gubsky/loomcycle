package webhook

import (
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// The spawned run carries the definition's bearer reference, unopened; a
// payload_mapping target named user_bearer does not set it.
func TestReceiver_TheRunCarriesTheDefinitionsBearer(t *testing.T) {
	fr := &fakeRunner{runID: "r"}
	hook := githubPushHook()
	hook.UserBearer = "$cred:hook-token"
	hook.PayloadMapping = map[string]string{"user_bearer": "$.token"}
	rec := newScopeReceiver(openScopeStore(t), fr, map[string]config.Webhook{"gh": hook})
	body := `{"ref":"refs/heads/main","before":"a","after":"b","repository":{"id":1},"token":"$cred:other"}`
	answer(t, "a push", rec, "/v1/_webhooks/gh", body, githubDelivery(body, "d1", "push"), http.StatusAccepted)
	if got := fr.input().UserBearer; got != "$cred:hook-token" {
		t.Errorf("the run's bearer is %q, want the definition's reference", got)
	}
}

// A webhook's run never resolves $cred: in its user's own scope: that user
// can come from the delivery.
func TestBuildRunInput_AWebhookRunHasNoUserScope(t *testing.T) {
	in := buildRunInput(config.Webhook{Agent: "x"}, projectResult{Fields: map[string]string{"user_id": "bob"}}, nil, nil, nil)
	if !in.CredentialsNoUserScope || in.UserID != "bob" {
		t.Errorf("CredentialsNoUserScope = %t user %q, want true for bob", in.CredentialsNoUserScope, in.UserID)
	}
}
