package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func adminCtx(tenant string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: "root", Scopes: []string{auth.ScopeAdmin}})
}

// A global channel is split by tenant over a shared operator layer: what a
// tenant publishes reaches only that tenant, what the operator publishes
// reaches every tenant.
func TestGlobalChannel_ATenantReachesOnlyItselfAndTheOperatorReachesAll(t *testing.T) {
	srv, _, cleanup := channelHoldFixture(t) // yaml "open" is a global channel
	defer cleanup()
	pub := func(ctx context.Context, body string) {
		t.Helper()
		if _, err := srv.PublishChannel(ctx, connector.ChannelPublishRequest{Channel: "open", Scope: "global", Payload: json.RawMessage(body)}); err != nil {
			t.Fatal(err)
		}
	}
	peek := func(ctx context.Context) int {
		t.Helper()
		res, err := srv.PeekChannel(ctx, connector.ChannelPeekRequest{Channel: "open", Scope: "global", MaxMessages: 50})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Messages)
	}
	pub(tenantCtx("acme"), `{"from":"acme"}`)
	pub(adminCtx("ops"), `{"from":"the operator"}`)
	pub(tenantCtx("globex"), `{"from":"globex"}`)
	pub(tenantCtx("globex"), `{"from":"globex"}`)

	for name, want := range map[string]int{"acme": 2, "globex": 3} {
		if got := peek(tenantCtx(name)); got != want {
			t.Errorf("%s reads %d message(s), want its own and the operator's (%d)", name, got, want)
		}
	}
	if got := peek(tenantCtx("initech")); got != 1 {
		t.Errorf("a third tenant reads %d, want only the operator's", got)
	}

	list, err := srv.ListChannels(tenantCtx("globex"))
	if err != nil {
		t.Fatal(err)
	}
	if ch, _ := findChannel(t, list, "open"); ch.MessageCount != 3 {
		t.Errorf("globex lists %d messages, want 3", ch.MessageCount)
	}
	if list, err = srv.ListChannels(adminCtx("ops")); err != nil {
		t.Fatal(err)
	}
	if ch, _ := findChannel(t, list, "open"); ch.MessageCount != 4 {
		t.Errorf("the admin lists %d messages, want every layer's 4", ch.MessageCount)
	}

	// A tenant's purge empties its own layer; an admin's, every layer.
	if res, err := srv.PurgeChannel(tenantCtx("globex"), "open"); err != nil || res.Purged != 2 {
		t.Fatalf("globex purge = %+v, %v; want its own 2", res, err)
	}
	if got := peek(tenantCtx("acme")); got != 2 {
		t.Fatalf("globex's purge touched acme's view: %d", got)
	}
	if res, err := srv.PurgeChannel(adminCtx("ops"), "open"); err != nil || res.Purged != 2 {
		t.Fatalf("admin purge = %+v, %v; want acme's and the operator's 2", res, err)
	}
}

// The review's bypass: a tenant with its own unheld channel named like the
// operator's held global one publishes under that name. Its message stays in
// its own layer — the operator's readers and every other tenant never see it,
// held or not.
func TestGlobalChannel_ATenantsSameNamedChannelCannotFeedAnother(t *testing.T) {
	srv, _, cleanup := channelHoldFixture(t)
	defer cleanup()
	if _, err := srv.CreateChannel(adminCtx(""), connector.ChannelCreateRequest{Name: "inbox", Scope: "global", Semantic: "queue", Hold: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.CreateChannel(tenantCtx("mallory"), connector.ChannelCreateRequest{Name: "inbox", Scope: "tenant", Semantic: "queue"}); err != nil {
		t.Fatal(err)
	}
	res, err := srv.PublishChannel(tenantCtx("mallory"), connector.ChannelPublishRequest{Channel: "inbox", Scope: "global", Payload: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Held {
		t.Fatalf("mallory's own unheld channel held its message: %+v", res)
	}
	for _, ctx := range []context.Context{adminCtx(""), tenantCtx("acme")} {
		got, err := srv.PeekChannel(ctx, connector.ChannelPeekRequest{Channel: "inbox", Scope: "global", MaxMessages: 10})
		if err == nil && len(got.Messages) != 0 {
			t.Fatalf("another reader sees mallory's message: %+v", got.Messages)
		}
	}
	rel, err := srv.ReleaseChannel(adminCtx(""), connector.ChannelReleaseRequest{Channel: "inbox", Scope: "global", Count: 10})
	if err != nil || rel.ReleasedCount != 0 {
		t.Fatalf("the operator's release reached mallory's layer: %+v, %v", rel, err)
	}
}

// Runtime channels are per (tenant, name): the operator, acme and globex each
// own a global "jobs", hooli a tenant-scope one that still holds global rows
// (a wire publish names its scope), and initech none. A purge takes only the
// layers of the channel it names: acme's purge its own; the operator's its
// own and initech's, which resolves to no channel of its own — never globex's
// or hooli's.
func TestPurgeChannel_SparesTenantsWithTheirOwnSameNamedChannel(t *testing.T) {
	srv, st, cleanup := channelHoldFixture(t)
	defer cleanup()
	for _, tenant := range []string{"", "acme", "globex"} {
		if _, err := srv.CreateChannel(adminCtx(tenant), connector.ChannelCreateRequest{Name: "jobs", Scope: "global", Semantic: "queue"}); err != nil {
			t.Fatalf("create %q: %v", tenant, err)
		}
	}
	if _, err := srv.CreateChannel(tenantCtx("hooli"), connector.ChannelCreateRequest{Name: "jobs", Scope: "tenant", Semantic: "queue"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.PublishChannel(tenantCtx("hooli"), connector.ChannelPublishRequest{Channel: "jobs", Scope: "global", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	layers := []string{"", "acme", "globex", "hooli", "initech"}
	for _, tenant := range []string{"", "acme", "globex", "initech"} {
		if _, _, err := st.ChannelPublish(context.Background(), store.ChannelMessage{Channel: "jobs", TenantID: tenant, Scope: store.MemoryScopeGlobal, Payload: json.RawMessage(`{}`)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	own := func(tenant string) int {
		t.Helper()
		msgs, err := st.ChannelPeek(context.Background(), tenant, "jobs", store.MemoryScopeGlobal, "", "", 50)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range msgs {
			if m.TenantID == tenant {
				n++
			}
		}
		return n
	}
	check := func(step string, gone map[string]bool) {
		t.Helper()
		for _, tenant := range layers {
			want := 1
			if gone[tenant] {
				want = 0
			}
			if got := own(tenant); got != want {
				t.Errorf("%s: %q's layer has %d message(s), want %d", step, tenant, got, want)
			}
		}
	}

	if res, err := srv.PurgeChannel(adminCtx("acme"), "jobs"); err != nil || res.Purged != 1 {
		t.Errorf("acme's purge = %+v, %v; want its own 1", res, err)
	}
	check("acme purges", map[string]bool{"acme": true})
	if res, err := srv.PurgeChannel(adminCtx(""), "jobs"); err != nil || res.Purged != 2 {
		t.Errorf("the operator's purge = %+v, %v; want its own and initech's 2", res, err)
	}
	check("the operator purges", map[string]bool{"acme": true, "": true, "initech": true})
}
