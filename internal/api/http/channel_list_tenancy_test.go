package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func findChannel(t *testing.T, resp connector.ListChannelsResponse, name string) (connector.ChannelDescriptor, bool) {
	t.Helper()
	for _, ch := range resp.Channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return connector.ChannelDescriptor{}, false
}

// An operator's yaml webhook is the operator's: a tenant operator listing the
// channels sees that the channel is hooked and by what name, never the URL or
// the headers, which may carry a token or an expanded secret. The operator
// sees them whole.
func TestListChannels_ATenantSeesNoYamlWebhookEndpoint(t *testing.T) {
	srv, _ := channelHooksFixture(t, true)
	cfg := *srv.cfg()
	cfg.Channels = map[string]config.Channel{
		"notify": {Scope: "global", Semantic: "queue", MaxMessages: 10,
			Hooks: hooks.EventHooks{hooks.PhaseChannelPublish: {{Inline: &hooks.Inline{
				Name: "relay", URL: "https://relay.internal/in?key=url-secret",
				Headers: map[string]string{"Authorization": "Bearer header-secret"},
			}}}}},
	}
	srv.cfgHolder = config.NewHolder(&cfg)

	resp, err := srv.ListChannels(tenantCtx("acme"))
	if err != nil {
		t.Fatal(err)
	}
	ch, ok := findChannel(t, resp, "notify")
	if !ok {
		t.Fatal("notify is not listed")
	}
	raw, _ := json.Marshal(ch.Hooks)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "relay.internal") || !strings.Contains(string(raw), "relay") {
		t.Fatalf("a tenant sees the hooks as %s", raw)
	}

	resp, err = srv.ListChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ch, _ = findChannel(t, resp, "notify")
	if raw, _ := json.Marshal(ch.Hooks); !strings.Contains(string(raw), "url-secret") || !strings.Contains(string(raw), "header-secret") {
		t.Fatalf("the operator sees the hooks as %s", raw)
	}
}

// Channel stats are per tenant: two tenants' channels of one name each report
// their own counts, on the list and on an update, and a tenant is never shown
// another tenant's undeclared channels.
func TestListChannels_StatsAreEachTenantsOwn(t *testing.T) {
	srv, st := channelHooksFixture(t, true)
	for tenant, n := range map[string]int{"acme": 1, "globex": 3} {
		if _, err := srv.CreateChannel(tenantCtx(tenant), connector.ChannelCreateRequest{Name: "inbox", Scope: "tenant"}); err != nil {
			t.Fatalf("%s create: %v", tenant, err)
		}
		for i := 0; i < n; i++ {
			if _, err := srv.PublishChannel(tenantCtx(tenant), connector.ChannelPublishRequest{Channel: "inbox", Scope: "tenant", Payload: json.RawMessage(`{}`)}); err != nil {
				t.Fatalf("%s publish: %v", tenant, err)
			}
		}
	}
	if _, _, err := st.ChannelPublish(context.Background(), store.ChannelMessage{
		Channel: "globex-private", Scope: store.MemoryScopeTenant, ScopeID: "globex", TenantID: "globex", Payload: []byte(`{}`),
	}, 100); err != nil {
		t.Fatal(err)
	}

	for tenant, want := range map[string]int64{"acme": 1, "globex": 3} {
		resp, err := srv.ListChannels(tenantCtx(tenant))
		if err != nil {
			t.Fatal(err)
		}
		if ch, _ := findChannel(t, resp, "inbox"); ch.MessageCount != want {
			t.Errorf("%s lists inbox with %d messages, want %d", tenant, ch.MessageCount, want)
		}
		_, orphan := findChannel(t, resp, "globex-private")
		if orphan != (tenant == "globex") {
			t.Errorf("%s sees globex-private: %v", tenant, orphan)
		}
		d := "d"
		desc, err := srv.UpdateChannel(tenantCtx(tenant), "inbox", connector.ChannelUpdateRequest{Description: &d})
		if err != nil || desc.MessageCount != want {
			t.Errorf("%s update reports %d messages (%v), want %d", tenant, desc.MessageCount, err, want)
		}
	}
}
