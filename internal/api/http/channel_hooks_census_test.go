package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// channelHooksFixture is a server with one yaml channel that carries a hook,
// wired as main.go wires it (the writer resolving definitions through the
// server, hooks enabled).
func channelHooksFixture(t *testing.T, enabled bool) (*Server, store.Store) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := &config.Config{
		Channels: map[string]config.Channel{
			"screened": {Scope: "global", Semantic: "queue", MaxMessages: 100,
				Hooks: hooks.EventHooks{hooks.PhaseChannelPublish: {{Ref: "screen"}}}},
		},
		Env: config.Env{AuthToken: "test-token", ChannelsMaxValueBytes: 64 * 1024, ChannelsLongPollCapMS: 1000, ChannelHooksEnabled: enabled},
	}
	bus := channels.NewBus()
	srv := &Server{
		cfgHolder:      config.NewHolder(cfg),
		store:          s,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookDispatcher: hooks.NewDispatcher(hooks.NewSet(), nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	srv.SetSystemPublisher(&channels.StorePublisher{Store: s, Bus: bus, Scheduler: channels.NewScheduler(bus, 100),
		Defs: srv.ChannelWriteDef, HooksEnabled: enabled})
	srv.SetChannelBus(bus)
	return srv, s
}

// The channel-hook census: every surface that writes a channel message,
// driven against a channel that carries a hook. Each must store the message
// awaiting its hooks and deliver nothing: a hook that five surfaces honour
// and a sixth does not is a gate with a hole in it. (The scheduler and the
// webhook write through the same writer, covered in their packages.)
func TestChannelHookCensus_EverySurfaceIsIntercepted(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		origin string
		write  func(t *testing.T, srv *Server, st store.Store)
	}{
		{"agent Channel tool", "", func(t *testing.T, srv *Server, st store.Store) {
			tool := &builtin.Channel{Store: st, Writer: srv.systemPublisher.(channels.Writer)}
			runCtx := tools.WithChannelPolicy(ctx, tools.ChannelPolicyValue{
				Publish:  []string{"screened"},
				Channels: map[string]tools.ChannelDef{"screened": {Name: "screened", Scope: "global", MaxMessages: 100}},
			})
			res, err := tool.Execute(runCtx, json.RawMessage(`{"op":"publish","channel":"screened","value":{}}`))
			if err != nil || res.IsError || !strings.Contains(res.Text, `"awaiting_hooks":true`) {
				t.Fatalf("publish: err=%v result=%s", err, res.Text)
			}
		}},
		{"admin publish", "", func(t *testing.T, srv *Server, _ store.Store) {
			rec := postJSON(t, srv, "/v1/_channels/screened/publish", `{"payload":{}}`)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"awaiting_hooks":true`) {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		}},
		{"admin broadcast", "", func(t *testing.T, srv *Server, _ store.Store) {
			rec := postJSON(t, srv, "/v1/_channels/_broadcast", `{"channels":["screened"],"scope":"global","payload":{}}`)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"awaiting_hooks":true`) {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		}},
		{"system publisher", "", func(t *testing.T, srv *Server, _ store.Store) {
			if _, err := srv.systemPublisher.PublishNow(ctx, "screened", "t1", store.MemoryScopeGlobal, "", json.RawMessage(`{}`), channels.SystemPublisherUserID, 0, 0); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}},
		{"team channel node", "", func(t *testing.T, srv *Server, _ store.Store) {
			io := &teamChannelIO{srv: srv, acl: &teamgraph.TeamChannels{Publish: []string{"screened"}}}
			if err := io.Publish(ctx, "screened", json.RawMessage(`{}`)); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}},
		{"Starter sink", channels.OriginStarterSink, func(t *testing.T, srv *Server, _ store.Store) {
			io := &teamChannelIO{srv: srv, acl: &teamgraph.TeamChannels{Publish: []string{"screened"}}}
			if err := io.PublishSink(ctx, "screened", json.RawMessage(`{"wave":"w","index":0,"status":"ok"}`)); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := channelHooksFixture(t, true)
			tc.write(t, srv, st)
			if msgs, err := st.ChannelPeek(ctx, "", "screened", store.MemoryScopeGlobal, "", "", 10); err != nil || len(msgs) != 0 {
				t.Fatalf("delivered %d message(s) past the channel's hooks (err %v)", len(msgs), err)
			}
			items, err := st.ChannelHookClaim(ctx, "w", time.Now(), time.Now().Add(time.Minute), 10)
			if err != nil || len(items) != 1 {
				t.Fatalf("awaiting hooks: %d (err %v), want the 1 message", len(items), err)
			}
			if m := items[0].Message; m.Origin != tc.origin || m.HookTenant != "" {
				t.Fatalf("origin %q hook_tenant %q; want origin %q, the operator's tenant", m.Origin, m.HookTenant, tc.origin)
			}
		})
	}
}

// With channel hooks off, a hooked channel's hooks are skipped: a publish is
// delivered at once, as if the channel declared none, and nothing waits for a
// worker that is not running.
func TestChannelHooks_HooksAreSkippedWhenDisabled(t *testing.T) {
	srv, st := channelHooksFixture(t, false)
	rec := postJSON(t, srv, "/v1/_channels/screened/publish", `{"payload":{}}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"awaiting_hooks":true`) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if msgs, _ := st.ChannelPeek(context.Background(), "", "screened", store.MemoryScopeGlobal, "", "", 10); len(msgs) != 1 {
		t.Fatalf("delivered %d message(s), want the 1 published", len(msgs))
	}
	if items, _ := st.ChannelHookClaim(context.Background(), "w", time.Now(), time.Now().Add(time.Minute), 10); len(items) != 0 {
		t.Fatalf("%d message(s) wait for hooks that will never run", len(items))
	}
}
