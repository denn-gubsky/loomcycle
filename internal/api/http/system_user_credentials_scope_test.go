package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A subscription walk is started by a channel message, with nobody on the
// sweep's ctx: it runs as the system user, which is no person. There is no
// user whose own credentials it may use, so the walk — and every member it
// spawns — resolves $cred: for its agent and tenant only, whoever promoted
// the team. (A user-scoped credential stored under the literal "_system"
// subject must never stand in for the tenant's.)
func TestTeamSubscription_AWalkNeverUsesAUsersOwnCredentials(t *testing.T) {
	h := newPromoterHarness(t)
	admin := auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}}
	h.promote(tools.RunIdentityValue{AgentID: "a_root", TenantID: "acme"}, &admin, h.authorUnpromoted())
	seen, _ := h.walkOnce()
	for i, id := range seen {
		if id.UserID != channels.SystemPublisherUserID || !id.CredentialsNoUserScope {
			t.Errorf("member call %d ran as %q with CredentialsNoUserScope=%t, want the system user and no user scope", i, id.UserID, id.CredentialsNoUserScope)
		}
	}
}

// A channel hook's run is the system's too: the asks and decisions its hooks
// make are filed under it, and a webhook hook's header $cred: resolves as it.
// Its context and its row both withhold the user scope.
func TestChannelHookRuns_AHookRunNeverUsesAUsersOwnCredentials(t *testing.T) {
	srv, _ := continuationServer(t, nil)
	rctx, runID, err := channelHookRuns{srv}.Open(context.Background(), "acme", "hooks", "")
	if err != nil {
		t.Fatal(err)
	}
	if ri := tools.RunIdentity(rctx); ri.UserID != hookRunUser || !ri.CredentialsNoUserScope {
		t.Errorf("hook run ctx: user %q CredentialsNoUserScope=%t, want the system user and no user scope", ri.UserID, ri.CredentialsNoUserScope)
	}
	run, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.CredentialsNoUserScope {
		t.Error("the hook run's row does not record the no-user-scope bit")
	}
}
