package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channelhooks"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// channelWriter writes through the channel writer the way the server wires
// it, resolving each channel's definition from s's runtime rows (the part of
// Server.ChannelWriteDef a store alone can answer).
func channelWriter(s store.Store) *channels.StorePublisher {
	return &channels.StorePublisher{Store: s, HooksEnabled: true,
		Defs: func(ctx context.Context, tenant, ch string) (channels.WriteDef, error) {
			row, err := s.ChannelGet(ctx, tenant, ch)
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return channels.WriteDef{}, nil
			}
			if err != nil {
				return channels.WriteDef{}, err
			}
			return channels.WriteDef{Hold: row.Hold, Hooked: !store.NoChannelHooks(row.Hooks), HookTenant: row.TenantID}, nil
		}}
}

// channelHookDefs resolves a hooked channel for the hook worker from s's
// runtime rows, as Server.ChannelHookDef does for a channel yaml does not
// declare.
func channelHookDefs(s store.Store) channelhooks.DefResolver {
	return func(ctx context.Context, tenant, ch string) (channelhooks.Def, bool, error) {
		row, err := s.ChannelGet(ctx, tenant, ch)
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return channelhooks.Def{}, false, nil
		}
		if err != nil {
			return channelhooks.Def{}, false, err
		}
		def := channelhooks.Def{Hold: row.Hold}
		if !store.NoChannelHooks(row.Hooks) {
			if err := json.Unmarshal(row.Hooks, &def.Hooks); err != nil {
				return channelhooks.Def{}, false, err
			}
		}
		return def, true, nil
	}
}

func writeChannel(t *testing.T, w *channels.StorePublisher, tenant, ch string, scope store.MemoryScope, scopeID, payload string) channels.WriteResult {
	t.Helper()
	res, err := w.Write(context.Background(), channels.WriteRequest{
		Channel: ch, TenantID: tenant, Scope: scope, ScopeID: scopeID,
		Payload: json.RawMessage(payload), PublishedBy: "u",
	})
	if err != nil {
		t.Fatalf("write %s/%s: %v", tenant, ch, err)
	}
	return res
}

func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// Every runtime channel definition comes back on restore under its own
// tenant with every setting it had: a tenant's held, hooked, bounded channel
// and an operator's global one. Before the channel_defs section a restore
// brought back neither, so the channels' messages arrived undeclared.
func TestRoundTrip_RuntimeChannelDefsKeepTheirTenantAndSettings(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	hooksJSON := json.RawMessage(`{"channel_publish":[{"name":"screen","url":"https://hooks.example/screen","fail_mode":"closed"}]}`)
	want := []store.ChannelRow{
		{Name: "gate", TenantID: "acme", Description: "review queue", Scope: "user", Semantic: "queue",
			DefaultTTL: 3600, MaxMessages: 7, Hold: true, Hooks: hooksJSON},
		{Name: "bcast", TenantID: store.ChannelOperatorTenant, Scope: "global", Semantic: "broadcast",
			Publisher: "system", Period: "1m"},
	}
	for _, row := range want {
		if err := src.ChannelsCreate(ctx, row); err != nil {
			t.Fatal(err)
		}
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.ChannelDefsRestored != 2 || len(res.Warnings) != 0 {
		t.Fatalf("channel defs restored = %d, warnings %v; want 2 and none", res.ChannelDefsRestored, res.Warnings)
	}

	for _, w := range want {
		srcRow, err := src.ChannelGet(ctx, w.TenantID, w.Name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := dst.ChannelGet(ctx, w.TenantID, w.Name)
		if err != nil {
			t.Fatalf("%q/%q after restore: %v", w.TenantID, w.Name, err)
		}
		if got.TenantID != w.TenantID || got.Description != w.Description || got.Scope != w.Scope ||
			got.Semantic != w.Semantic || got.DefaultTTL != w.DefaultTTL || got.MaxMessages != w.MaxMessages ||
			got.Publisher != w.Publisher || got.Period != w.Period || got.Hold != w.Hold {
			t.Errorf("%q/%q restored as %+v, want %+v", w.TenantID, w.Name, got, w)
		}
		if store.NoChannelHooks(w.Hooks) != store.NoChannelHooks(got.Hooks) ||
			(!store.NoChannelHooks(w.Hooks) && !sameJSON(t, got.Hooks, w.Hooks)) {
			t.Errorf("%q/%q hooks = %s, want %s", w.TenantID, w.Name, got.Hooks, w.Hooks)
		}
		if !got.CreatedAt.Equal(srcRow.CreatedAt) {
			t.Errorf("%q/%q created_at = %v, want the source's %v", w.TenantID, w.Name, got.CreatedAt, srcRow.CreatedAt)
		}
	}
	// A tenant's definition stays its own.
	var nf *store.ErrNotFound
	if _, err := dst.ChannelGet(ctx, store.ChannelOperatorTenant, "gate"); !errors.As(err, &nf) {
		t.Errorf("acme's channel is visible in the operator layer after restore (err = %v)", err)
	}

	again, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.ChannelDefsRestored != 0 || len(again.Warnings) != 0 {
		t.Fatalf("second restore wrote %d channel defs, warnings %v; want nothing", again.ChannelDefsRestored, again.Warnings)
	}
}

// A definition already live on the target is not overwritten by an older one
// from the snapshot; the rest of the section still lands.
func TestRestore_ChannelDefLeavesALiveDefinitionAlone(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	if err := src.ChannelsCreate(ctx, store.ChannelRow{Name: "gate", TenantID: "acme", Scope: "user", Semantic: "queue", Hold: true, MaxMessages: 7}); err != nil {
		t.Fatal(err)
	}
	if err := src.ChannelsCreate(ctx, store.ChannelRow{Name: "bcast", Scope: "global", Semantic: "broadcast"}); err != nil {
		t.Fatal(err)
	}
	live := store.ChannelRow{Name: "gate", TenantID: "acme", Scope: "user", Semantic: "queue", MaxMessages: 99}
	if err := dst.ChannelsCreate(ctx, live); err != nil {
		t.Fatal(err)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChannelDefsRestored != 1 || len(res.Warnings) != 0 {
		t.Fatalf("channel defs restored = %d, warnings %v; want 1 (bcast) and none", res.ChannelDefsRestored, res.Warnings)
	}
	got, err := dst.ChannelGet(ctx, "acme", "gate")
	if err != nil {
		t.Fatal(err)
	}
	if got.Hold || got.MaxMessages != 99 {
		t.Errorf("live definition = %+v, want it unchanged (no hold, max_messages 99)", got)
	}
	if _, err := dst.ChannelGet(ctx, "", "bcast"); err != nil {
		t.Errorf("bcast after restore: %v", err)
	}
}

// A held channel is still held on the target: a message held at the capture
// stays held, a new publish is held too, and both are releasable. Without its
// definition the channel delivered the next publish at once.
func TestRoundTrip_HeldChannelStaysHeldAndReleasable(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	if err := src.ChannelsCreate(ctx, store.ChannelRow{Name: "gate", TenantID: "acme", Scope: "user", Semantic: "queue", Hold: true}); err != nil {
		t.Fatal(err)
	}
	if res := writeChannel(t, channelWriter(src), "acme", "gate", store.MemoryScopeUser, "alice", `{"n":1}`); !res.Held {
		t.Fatalf("setup: the source write was not held: %+v", res)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	peek := func() []store.ChannelMessage {
		t.Helper()
		msgs, err := dst.ChannelPeek(ctx, "acme", "gate", store.MemoryScopeUser, "alice", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		return msgs
	}
	if msgs := peek(); len(msgs) != 0 {
		t.Fatalf("restored held message was delivered: %+v", msgs)
	}
	if res := writeChannel(t, channelWriter(dst), "acme", "gate", store.MemoryScopeUser, "alice", `{"n":2}`); !res.Held {
		t.Fatalf("a publish on the restored channel was not held: %+v", res)
	}
	if msgs := peek(); len(msgs) != 0 {
		t.Fatalf("delivered before a release: %+v", msgs)
	}
	released, stillHeld, err := dst.ChannelRelease(ctx, "acme", "gate", store.MemoryScopeUser, "alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 2 || stillHeld != 0 {
		t.Fatalf("release = %v, still held %d; want both messages released", released, stillHeld)
	}
	if msgs := peek(); len(msgs) != 2 {
		t.Fatalf("after the release %d messages are deliverable, want 2", len(msgs))
	}
}

// A message awaiting its channel's hooks when the snapshot was taken is
// claimed and decided by the hook worker on the target, running the chain
// from the start (the hook progress row is not carried). Without the
// channel's definition the worker found the channel undeclared and kept the
// message back until its deadline dropped it.
func TestRoundTrip_MessageAwaitingHooksIsDecidedOnTheTarget(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"updated_body":{"text":"[screened]"}}`))
	}))
	defer hook.Close()

	hooksJSON, err := json.Marshal(hooks.EventHooks{hooks.PhaseChannelPublish: {
		{Inline: &hooks.Inline{Name: "screen", URL: hook.URL, FailMode: hooks.FailClosed}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.ChannelsCreate(ctx, store.ChannelRow{Name: "inbox", TenantID: "acme", Scope: "user", Semantic: "queue", Hooks: hooksJSON}); err != nil {
		t.Fatal(err)
	}
	if res := writeChannel(t, channelWriter(src), "acme", "inbox", store.MemoryScopeUser, "alice", `{"text":"hi"}`); !res.AwaitingHooks {
		t.Fatalf("setup: the source write is not awaiting hooks: %+v", res)
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	w := channelhooks.New(channelhooks.Config{
		// A tenant's hook may reach a private address only on the operator's
		// allowlist; the test's hook listens on loopback.
		Store: dst, Dispatcher: hooks.NewDispatcherWithPrivateHosts(nil, nil, []string{"127.0.0.1"}),
		Owner: "target", Poll: 10 * time.Millisecond,
		Lookup: func(context.Context, string, string, int) (hooks.Def, string, error) {
			return hooks.Def{}, "", hooks.DefNotFound(errors.New("not found"))
		},
		Defs: channelHookDefs(dst),
	})
	wctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(wctx); close(done) }()
	defer func() { stop(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs, err := dst.ChannelPeek(ctx, "acme", "inbox", store.MemoryScopeUser, "alice", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) == 1 {
			if string(msgs[0].Payload) != `{"text":"[screened]"}` || calls.Load() != 1 {
				t.Fatalf("delivered %s after %d hook calls; want the screened body after one", msgs[0].Payload, calls.Load())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restored message awaiting hooks was never decided (%d hook calls, worker stats %+v)", calls.Load(), w.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A snapshot taken before the channel_defs section restores no channel
// definitions and everything else as it always did.
func TestRestore_WithoutChannelDefsSectionRestoresAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	if err := src.ChannelsCreate(ctx, store.ChannelRow{Name: "gate", TenantID: "acme", Scope: "user", Semantic: "queue", Hold: true}); err != nil {
		t.Fatal(err)
	}
	writeChannel(t, channelWriter(src), "acme", "gate", store.MemoryScopeUser, "alice", `{}`)

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw = withoutSection(t, raw, "channel_defs")
	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.ChannelDefsRestored != 0 || res.ChannelMessagesRestored != 1 || len(res.Warnings) != 0 {
		t.Fatalf("restore = %+v; want no channel defs, the one message, no warnings", res)
	}
	rows, err := dst.ChannelsList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("channel defs after an old-format restore = %+v, want none", rows)
	}
}

// withoutSection rewrites an envelope as a snapshot taken before a section
// existed, failing if the section was not there to remove.
func withoutSection(t *testing.T, raw []byte, name string) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	sections := env["sections"].(map[string]any)
	if _, ok := sections[name]; !ok {
		t.Fatalf("the captured envelope has no %s section; the old-format case would prove nothing", name)
	}
	delete(sections, name)
	// The checksum covers the sections; drop it as a pre-checksum snapshot would.
	delete(env, "checksum")
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
