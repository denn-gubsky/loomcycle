package http

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// identityProvider answers like numberedProvider and records the run identity
// each call was made under — the identity a member's tools resolve
// credentials for.
type identityProvider struct {
	numberedProvider
	mu   sync.Mutex
	seen []tools.RunIdentityValue
}

func (p *identityProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.RunIdentity(ctx))
	p.mu.Unlock()
	return p.numberedProvider.Call(ctx, req)
}

// A triggered walk that may not use its user's own scope passes that on to
// every member it spawns: a member's MCP header or A2A bearer resolves $cred:
// for its agent and tenant only, as a spawned run of the same trigger would.
func TestStartTeamWalk_MembersInheritTheTriggersNoUserScope(t *testing.T) {
	for _, noUserScope := range []bool{true, false} {
		h := newTriggerWalkHarness(t)
		prov := &identityProvider{numberedProvider: numberedProvider{answer: "done"}}
		h.srv = New(h.cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), h.st)
		h.srv.SetSteerRegistry(steer.NewRegistry(0))
		h.srv.SetTeamDefTool(&builtin.TeamDef{Store: h.st})
		seedTenantTeam(t, h.st, "acme", "pr-review", varsTeam)

		runID, err := h.srv.StartTeamWalk(context.Background(), runner.TeamWalkInput{
			Team: "pr-review", Input: "the payload", TenantID: "acme", UserID: "u-42",
			CredentialsNoUserScope: noUserScope,
		})
		if err != nil {
			t.Fatalf("StartTeamWalk: %v", err)
		}
		if run := h.finished(runID); run.Status != store.RunCompleted {
			t.Fatalf("walk status = %s (%s), want completed", run.Status, run.ErrorMsg)
		}
		prov.mu.Lock()
		seen := append([]tools.RunIdentityValue(nil), prov.seen...)
		prov.mu.Unlock()
		if len(seen) != 1 {
			t.Fatalf("no-user-scope=%t: the model was called %d times, want once for the one member", noUserScope, len(seen))
		}
		if seen[0].UserID != "u-42" || seen[0].CredentialsNoUserScope != noUserScope {
			t.Errorf("no-user-scope=%t: the member ran as user %q with CredentialsNoUserScope=%t, want the walk's",
				noUserScope, seen[0].UserID, seen[0].CredentialsNoUserScope)
		}
	}
}
