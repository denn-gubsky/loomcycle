package webhook

import (
	"net/http"
	"testing"
)

// A team delivery's walk resolves $cred: no more widely than a spawned run of
// the same webhook: its user can come from the delivery, so neither the walk
// nor any member it spawns looks a credential up in that user's own scope.
func TestReceiver_TeamDelivery_TheWalkNeverTakesTheUsersOwnCredentials(t *testing.T) {
	teams := &fakeTeams{}
	rec, _ := newTeamReceiver(t, teamHook(), teams, nil)
	body := []byte(`{"repository":{"full_name":"denn/loomcycle"},"pull_request":{"number":1612}}`)

	if code, resp := signedTeamPost(rec, body, "d-1"); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%v", code, resp)
	}
	calls := teams.started()
	if len(calls) != 1 || !calls[0].CredentialsNoUserScope {
		t.Fatalf("walks = %+v, want one that resolves no credential in its user's scope", calls)
	}
}
