package http

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channelhooks"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/hooks/codehook"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// putHookDef stores an active HookDef in a tenant.
func putHookDef(t *testing.T, st store.Store, tenant, name string, d hooks.Def) {
	t.Helper()
	raw, _ := json.Marshal(d)
	row, err := st.HookDefCreate(context.Background(), store.HookDefRow{DefID: "hdf_" + tenant + "_" + name, Name: name, TenantID: tenant, Definition: raw})
	if err != nil {
		t.Fatalf("HookDefCreate: %v", err)
	}
	if err := st.HookDefSetActive(context.Background(), tenant, name, row.DefID, ""); err != nil {
		t.Fatalf("HookDefSetActive: %v", err)
	}
}

func codeDef(body string) hooks.Def {
	return hooks.Def{Event: hooks.PhaseChannelPublish, Body: hooks.DefBody{Kind: hooks.BodyKindCode, Code: body}}
}

func tenantCtx(tenant string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: "op", Scopes: []string{auth.ScopeTenant}})
}

func onChannel(refs ...string) hooks.EventHooks {
	var entries []hooks.Entry
	for _, r := range refs {
		entries = append(entries, hooks.Entry{Ref: r})
	}
	return hooks.EventHooks{hooks.PhaseChannelPublish: entries}
}

// A runtime channel carries hooks: they are stored with it, shown on its
// descriptor, replaced by an update and removed by an update with none.
func TestRuntimeChannel_HooksRoundTrip(t *testing.T) {
	srv, st := channelHooksFixture(t, true)
	putHookDef(t, st, "acme", "screen", codeDef(`function hook(ev){}`))
	putHookDef(t, st, "acme", "audit", codeDef(`function hook(ev){}`))
	ctx := tenantCtx("acme")
	desc, err := srv.CreateChannel(ctx, connector.ChannelCreateRequest{Name: "inbox", Scope: "tenant", Hooks: onChannel("screen")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(desc.Hooks[hooks.PhaseChannelPublish]) != 1 {
		t.Fatalf("created descriptor hooks = %+v", desc.Hooks)
	}
	replaced := onChannel("screen", "audit")
	if desc, err = srv.UpdateChannel(ctx, "inbox", connector.ChannelUpdateRequest{Hooks: &replaced}); err != nil || len(desc.Hooks[hooks.PhaseChannelPublish]) != 2 {
		t.Fatalf("update: %+v, %v", desc.Hooks, err)
	}
	d := "d"
	if desc, err = srv.UpdateChannel(ctx, "inbox", connector.ChannelUpdateRequest{Description: &d}); err != nil || len(desc.Hooks[hooks.PhaseChannelPublish]) != 2 {
		t.Fatalf("an update without hooks changed them: %+v, %v", desc.Hooks, err)
	}
	none := hooks.EventHooks{}
	if desc, err = srv.UpdateChannel(ctx, "inbox", connector.ChannelUpdateRequest{Hooks: &none}); err != nil || desc.Hooks != nil {
		t.Fatalf("clearing: %+v, %v", desc.Hooks, err)
	}
	if def, _ := srv.ChannelWriteDef(context.Background(), "acme", "inbox"); def.Hooked {
		t.Fatal("the writer still sees hooks after they were removed")
	}
}

// A hook that cannot be attached is refused when the channel is saved: a
// HookDef that does not exist in the channel's tenant or the shared one, one
// that answers another event, or another tenant's. So is any hook while the
// server runs none.
func TestRuntimeChannel_RefusesHooksThatCannotRun(t *testing.T) {
	srv, st := channelHooksFixture(t, true)
	putHookDef(t, st, "acme", "gate", hooks.Def{Event: hooks.PhasePre, Body: hooks.DefBody{Kind: hooks.BodyKindCode, Code: `function hook(ev){}`}})
	putHookDef(t, st, "other", "theirs", codeDef(`function hook(ev){}`))
	for ref, want := range map[string]string{"missing": "no HookDef", "gate": "answers pre", "theirs": "no HookDef"} {
		_, err := srv.CreateChannel(tenantCtx("acme"), connector.ChannelCreateRequest{Name: "c-" + ref, Scope: "tenant", Hooks: onChannel(ref)})
		if !errors.Is(err, connector.ErrChannelHooksInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want invalid (%q)", ref, err, want)
		}
	}
}

// With channel hooks off, a runtime channel still takes hooks — they are
// checked and kept, so turning channel hooks on enforces them — but they are
// skipped meanwhile: a publish is delivered at once.
func TestRuntimeChannel_HooksAreKeptAndSkippedWhenDisabled(t *testing.T) {
	off, st := channelHooksFixture(t, false)
	putHookDef(t, st, "acme", "screen", codeDef(`function hook(ev){}`))
	if _, err := off.CreateChannel(tenantCtx("acme"), connector.ChannelCreateRequest{Name: "c", Scope: "tenant", Hooks: onChannel("missing")}); !errors.Is(err, connector.ErrChannelHooksInvalid) {
		t.Fatalf("an unknown HookDef with hooks off: err = %v", err)
	}
	desc, err := off.CreateChannel(tenantCtx("acme"), connector.ChannelCreateRequest{Name: "inbox", Scope: "tenant", Hooks: onChannel("screen")})
	if err != nil || len(desc.Hooks[hooks.PhaseChannelPublish]) != 1 {
		t.Fatalf("create with hooks off: %+v, %v", desc.Hooks, err)
	}
	res, err := off.PublishChannel(tenantCtx("acme"), connector.ChannelPublishRequest{Channel: "inbox", Scope: "tenant", Payload: json.RawMessage(`{}`)})
	if err != nil || res.AwaitingHooks {
		t.Fatalf("publish with hooks off: %+v, %v", res, err)
	}
	if msgs, _ := st.ChannelPeek(context.Background(), "acme", "inbox", store.MemoryScopeTenant, "", "", 10); len(msgs) != 1 {
		t.Fatalf("delivered %d, want 1", len(msgs))
	}
}

// Two tenants with a channel of the same name: each one's messages are decided
// by its own hook, never the other's, and each one's decisions are recorded
// where only it reads them.
func TestRuntimeChannel_EachTenantsHooksDecideItsOwnMessages(t *testing.T) {
	srv, st := channelHooksFixture(t, true)
	disp := hooks.NewDispatcher(nil, nil)
	disp.SetCodeRunner(codehook.New(nil))
	srv.hookDispatcher = disp
	putHookDef(t, st, "acme", "screen", codeDef(`function hook(ev){ return {updated_body: {by: "acme"}}; }`))
	putHookDef(t, st, "globex", "screen", codeDef(`function hook(ev){ return {decision: "drop", reason: "globex says no"}; }`))
	for _, tenant := range []string{"acme", "globex"} {
		if _, err := srv.CreateChannel(tenantCtx(tenant), connector.ChannelCreateRequest{Name: "inbox", Scope: "tenant", Hooks: onChannel("screen")}); err != nil {
			t.Fatalf("%s create: %v", tenant, err)
		}
		if _, err := srv.PublishChannel(tenantCtx(tenant), connector.ChannelPublishRequest{Channel: "inbox", Scope: "tenant", Payload: json.RawMessage(`{"from":"` + tenant + `"}`)}); err != nil {
			t.Fatalf("%s publish: %v", tenant, err)
		}
	}
	w := srv.NewChannelHookWorker("w1", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	// Wait for what the hooks did to land, not for the counters: a decision is
	// counted before its release is written.
	settled := func() bool {
		acme, _ := st.ChannelPeek(context.Background(), "acme", "inbox", store.MemoryScopeTenant, "", "", 10)
		dropped, _ := st.ChannelPeek(context.Background(), "globex", channelhooks.DecisionsChannel, store.MemoryScopeTenant, "", "", 10)
		return len(acme) == 1 && len(dropped) == 1
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !settled() {
		time.Sleep(10 * time.Millisecond)
	}
	acme, _ := st.ChannelPeek(context.Background(), "acme", "inbox", store.MemoryScopeTenant, "", "", 10)
	if len(acme) != 1 || string(acme[0].Payload) != `{"by":"acme"}` {
		t.Fatalf("acme's inbox = %+v", acme)
	}
	if globex, _ := st.ChannelPeek(context.Background(), "globex", "inbox", store.MemoryScopeTenant, "", "", 10); len(globex) != 0 {
		t.Fatalf("globex's inbox = %+v, want its message dropped by its own hook", globex)
	}
	decisions := func(tenant string) string {
		msgs, _ := st.ChannelPeek(context.Background(), tenant, channelhooks.DecisionsChannel, store.MemoryScopeTenant, "", "", 10)
		var b strings.Builder
		for _, m := range msgs {
			b.Write(m.Payload)
		}
		return b.String()
	}
	if a, g := decisions("acme"), decisions("globex"); !strings.Contains(a, "rewrite_body") || strings.Contains(a, "globex") ||
		!strings.Contains(g, "globex says no") || strings.Contains(g, "rewrite_body") {
		t.Fatalf("acme's decisions %s; globex's %s", a, g)
	}
}

// A hook run belongs to the tenant whose definition carries the hook: that
// tenant's operators may answer its ask, another tenant cannot even see it,
// and the run's user is the system, never the publisher.
func TestHookRun_AnswerableInItsOwnerTenantOnly(t *testing.T) {
	srv, st := channelHooksFixture(t, true)
	rctx, runID, err := channelHookRuns{srv}.Open(context.Background(), "acme", "hook:gate", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if ident := tools.RunIdentity(rctx); ident.TenantID != "acme" || ident.UserID != "_system" || tools.RunID(rctx) != runID {
		t.Fatalf("hook run identity = %+v (run %s)", ident, tools.RunID(rctx))
	}
	row := store.InterruptRow{InterruptID: store.MintInterruptID(time.Now()), RunID: runID, Kind: store.InterruptKindQuestion,
		Status: store.InterruptStatusPending, Question: "deliver?", Options: json.RawMessage(`["release","drop"]`), CreatedAt: time.Now(), UserID: "_system"}
	if _, err := st.InterruptCreate(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ResolveInterrupt(tenantCtx("globex"), runID, row.InterruptID, "question", "release", "api", "answer"); !errors.Is(err, connector.ErrInterruptNotFound) {
		t.Fatalf("another tenant resolving: %v, want not found", err)
	}
	if _, err := srv.ResolveInterrupt(tenantCtx("acme"), runID, row.InterruptID, "question", "release", "api", "answer"); err != nil {
		t.Fatalf("the owner tenant resolving: %v", err)
	}
	// Reopening an open run keeps it; an ended one is replaced.
	if _, again, _ := (channelHookRuns{srv}).Open(context.Background(), "acme", "hook:gate", runID); again != runID {
		t.Fatalf("an open run was replaced: %s", again)
	}
	channelHookRuns{srv}.Finish(runID, store.RunCompleted, "released")
	if _, fresh, _ := (channelHookRuns{srv}).Open(context.Background(), "acme", "hook:gate", runID); fresh == runID || fresh == "" {
		t.Fatalf("an ended run was reused: %s", fresh)
	}
}
