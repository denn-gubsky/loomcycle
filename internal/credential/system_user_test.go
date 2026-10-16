package credential

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
)

// The runs the runtime starts on nobody's behalf — a subscription walk, a
// channel hook's run — are attributed to the system user. A credential stored
// in that "user's" scope is never resolved: whoever could store one would
// have every such run send it in place of the tenant's. The name is taken
// from the constant those runs use, so the two cannot drift apart.
func TestEngine_TheSystemUsersScopeIsNeverResolved(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	system := channels.SystemPublisherUserID
	for scope, val := range map[string]string{"tenant": "team-token", "user": "planted-token"} {
		id := Identity{TenantID: "t", Scope: scope, Name: "telegram"}
		if scope == "user" {
			id.ScopeID = system
		}
		if _, err := e.PutInline(ctx, id, val, nil, nil, nil); err != nil {
			t.Fatalf("put %s: %v", scope, err)
		}
	}
	res, found, err := e.Resolve(ctx, "t", "", system, "telegram")
	if err != nil || !found || res.Value != "team-token" || res.Scope != "tenant" {
		t.Errorf("resolve as the system user = %+v (found=%v err=%v), want the tenant's team-token", res, found, err)
	}
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "user", ScopeID: system, Name: "only-planted"}, "planted", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := e.Resolve(ctx, "t", "", system, "only-planted"); found {
		t.Error("a credential stored only in the system user's scope resolved")
	}
	if ok, _ := e.HasKey(ctx, "t", "", system, "only-planted"); ok {
		t.Error("a credential stored only in the system user's scope counts as a key the run holds")
	}
}
