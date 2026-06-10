package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestResolveChannelScope_StaticRuntimeAndPrecedence covers the resolver the
// scheduler's on_complete: channel.publish hook uses (F37 / RFC T): a static
// yaml channel resolves to its declared scope, a runtime-substrate channel
// resolves to its store scope, yaml wins on a name collision, and an
// undeclared channel reports ok=false.
func TestResolveChannelScope_StaticRuntimeAndPrecedence(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	srv := &Server{
		cfg: &config.Config{
			Channels: map[string]config.Channel{
				"static-global": {Scope: "global", Semantic: "queue"},
				"static-user":   {Scope: "user", Semantic: "queue"},
				// Same name as a runtime row below — yaml must win.
				"shared": {Scope: "global", Semantic: "queue"},
			},
		},
		store: st,
	}

	ctx := context.Background()
	// Runtime-substrate channels: one unique, one colliding with yaml.
	if err := st.ChannelsCreate(ctx, store.ChannelRow{Name: "runtime-agent", Scope: "agent", Semantic: "queue"}); err != nil {
		t.Fatalf("ChannelsCreate runtime-agent: %v", err)
	}
	if err := st.ChannelsCreate(ctx, store.ChannelRow{Name: "shared", Scope: "user", Semantic: "queue"}); err != nil {
		t.Fatalf("ChannelsCreate shared: %v", err)
	}

	cases := []struct {
		channel   string
		wantScope string
		wantOK    bool
	}{
		{"static-global", "global", true},
		{"static-user", "user", true},
		{"runtime-agent", "agent", true},
		{"shared", "global", true}, // yaml precedence over the runtime "user" row
		{"undeclared", "", false},
	}
	for _, tc := range cases {
		gotScope, gotOK := srv.ResolveChannelScope(ctx, tc.channel)
		if gotScope != tc.wantScope || gotOK != tc.wantOK {
			t.Errorf("ResolveChannelScope(%q) = (%q, %v), want (%q, %v)",
				tc.channel, gotScope, gotOK, tc.wantScope, tc.wantOK)
		}
	}
}
