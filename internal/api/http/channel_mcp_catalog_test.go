package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The MCP plane calls the Channel tool with publish / subscribe grants and no
// channel catalog on the policy, so every channel read as "not declared" and
// the MCP `channel` tool could reach nothing. These drive the tool exactly as
// the MCP handler does: through the connector, on a ctx carrying the principal
// and that grant-only policy.
func channelCatalogFixture(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"news":    {Scope: "global", Semantic: "queue", MaxMessages: 100},
			"sysfeed": {Scope: "global", Semantic: "queue", MaxMessages: 100, Publisher: "system"},
		},
		Env: config.Env{ChannelsMaxValueBytes: 64 * 1024},
	}
	srv := New(cfg, &stubResolver{}, []tools.Tool{&builtin.Channel{Store: st}}, concurrency.New(4, 4, time.Second), st)
	return srv, st
}

// mcpChannelCtx is the ctx the MCP plane hands the connector for a principal.
func mcpChannelCtx(tenant string) context.Context {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: "op"})
	return tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{AllPublish: true, AllSubscribe: true})
}

func TestMCPChannelTool_PublishesToADeclaredChannel(t *testing.T) {
	srv, _ := channelCatalogFixture(t)
	res, err := srv.Channel(mcpChannelCtx(""), json.RawMessage(`{"op":"publish","channel":"news","value":{"n":1}}`))
	if err != nil || res.IsError {
		t.Fatalf("publish to a declared channel: err=%v result=%s", err, res.Text)
	}
	res, err = srv.Channel(mcpChannelCtx(""), json.RawMessage(`{"op":"peek","channel":"news"}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, `"n":1`) {
		t.Fatalf("peek after publish: err=%v result=%s", err, res.Text)
	}
}

// The catalog is the caller's tenant's: another tenant's runtime channel stays
// undeclared, as it is for a run.
func TestMCPChannelTool_CatalogIsTenantConfined(t *testing.T) {
	srv, st := channelCatalogFixture(t)
	if err := st.ChannelsCreate(context.Background(), store.ChannelRow{
		Name: "t1-inbox", TenantID: "t1", Scope: "tenant", Semantic: "queue",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	in := json.RawMessage(`{"op":"publish","channel":"t1-inbox","value":{"n":1}}`)
	if res, err := srv.Channel(mcpChannelCtx("t1"), in); err != nil || res.IsError {
		t.Fatalf("t1 publishing to its own channel: err=%v result=%s", err, res.Text)
	}
	res, err := srv.Channel(mcpChannelCtx("t2"), in)
	if err != nil {
		t.Fatalf("t2 publish: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "not declared") {
		t.Fatalf("t2 reached t1's channel: %s", res.Text)
	}
}

// A `publisher: system` channel stays closed to the MCP plane: the catalog
// declares it, and the refusal that applies to a run applies here.
func TestMCPChannelTool_StillRefusesASystemPublisherChannel(t *testing.T) {
	srv, _ := channelCatalogFixture(t)
	res, err := srv.Channel(mcpChannelCtx(""), json.RawMessage(`{"op":"publish","channel":"sysfeed","value":{"n":1}}`))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "publisher: system") {
		t.Fatalf("a publisher:system channel accepted an MCP publish: %s", res.Text)
	}
}
