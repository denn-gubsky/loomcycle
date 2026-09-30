package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// These tests pin WHERE a schedule's on_complete hooks write. The rule: a hook
// writes into the tenant the run executed in. For a single-run schedule that is
// the def's own tenant. For an operator-layer consolidation fan-out, whose runs
// execute in every target's own tenant, each tenant's hooks fire in that tenant,
// with that tenant's run — as if the operator had declared one schedule per
// tenant with `tenant_id`.

// hookedFanoutDef is an operator-layer fan-out with a channel.publish hook on a
// global channel and an agent-scoped memory.set hook.
func hookedFanoutDef() scheduleDef {
	def := fanoutDef(nil)
	def.OnComplete = []scheduleHook{
		{Kind: "channel.publish", Channel: "passes", Payload: map[string]any{"note": "done"}},
		{Kind: "memory.set", Scope: "agent", Key: "last-pass", Payload: map[string]any{"ok": true}},
	}
	return def
}

// runIDPerTenant names each child's run after its target, so a hook's run_id
// says which tenant's run it reports.
func runIDPerTenant(in runner.RunInput) (string, error) {
	return "r_" + in.TenantID + "_" + in.UserID, nil
}

// globalLayer reads ONE tenant's layer of a global channel, without the operator
// layer every tenant also reads.
func globalLayer(t *testing.T, st store.Store, tenant, channel string) []store.ChannelMessage {
	t.Helper()
	msgs, err := st.ChannelPeek(context.Background(), tenant, channel, store.MemoryScopeGlobal, "", "", 50)
	if err != nil {
		t.Fatalf("peek %q/%s: %v", tenant, channel, err)
	}
	var own []store.ChannelMessage
	for _, m := range msgs {
		if m.TenantID == tenant {
			own = append(own, m)
		}
	}
	return own
}

func hookRunID(t *testing.T, m store.ChannelMessage) string {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		t.Fatalf("decode hook payload: %v", err)
	}
	id, _ := p["run_id"].(string)
	return id
}

func memoryHookLanded(t *testing.T, st store.Store, tenant string) bool {
	t.Helper()
	_, err := st.MemoryGet(context.Background(), tenant, store.MemoryScopeAgent, "sched-test", "last-pass")
	if err == nil {
		return true
	}
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("memory get %q: %v", tenant, err)
	}
	return false
}

// TestFanoutHooks_OperatorScheduleWritesIntoEachRunsTenant is the reported bug.
// The fan-out ran alice's pass in "default" and sam's in "acme", then fired its
// hooks ONCE in the def's layer "": the completion message landed in the
// operator layer of a global channel — which every tenant reads — carrying one
// tenant's run id, and the memory write landed in a tenant nobody signs in to.
//
// Fails-before: the operator layer holds the message; default and acme hold
// none; the memory.set lands in "".
func TestFanoutHooks_OperatorScheduleWritesIntoEachRunsTenant(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, hookedFanoutDef(), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	sched.SetChannelScope(func(context.Context, string, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global"}, true
	})
	fr.resultFor = runIDPerTenant

	seedSettledSession(t, st, "default", "alice")
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	for tenant, wantRun := range map[string]string{"default": "r_default_alice", "acme": "r_acme_sam"} {
		msgs := globalLayer(t, st, tenant, "passes")
		if len(msgs) != 1 {
			t.Errorf("tenant %q's layer holds %d completion message(s), want 1; logs:\n%s", tenant, len(msgs), logs.all())
			continue
		}
		if got := hookRunID(t, msgs[0]); got != wantRun {
			t.Errorf("tenant %q's completion message reports run %q, want its own run %q", tenant, got, wantRun)
		}
		if !memoryHookLanded(t, st, tenant) {
			t.Errorf("memory.set did not land in tenant %q", tenant)
		}
	}
	if msgs := globalLayer(t, st, "", "passes"); len(msgs) != 0 {
		t.Errorf("the operator layer, which every tenant reads, holds %d completion message(s) (run %q) — no run executed there", len(msgs), hookRunID(t, msgs[0]))
	}
	if memoryHookLanded(t, st, "") {
		t.Error("memory.set landed in the operator layer, where no run executed")
	}
}

// TestFanoutHooks_OneTenantsFailureDoesNotSilenceAnothersHooks: each tenant's
// hooks follow that tenant's runs, as they would for a schedule per tenant. A
// failed pass in acme must not withhold default's completion message, and acme
// gets none — its batch did not complete.
//
// Fails-before: the batch-level "failed" fires no hook anywhere.
func TestFanoutHooks_OneTenantsFailureDoesNotSilenceAnothersHooks(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, hookedFanoutDef(), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	sched.SetChannelScope(func(context.Context, string, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global"}, true
	})
	fr.resultFor = func(in runner.RunInput) (string, error) {
		if in.TenantID == "acme" {
			return "r_acme", errors.New("extractor exploded")
		}
		return runIDPerTenant(in)
	}

	seedSettledSession(t, st, "default", "alice")
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	if n := len(globalLayer(t, st, "default", "passes")); n != 1 {
		t.Errorf("default's completed batch published %d message(s), want 1; logs:\n%s", n, logs.all())
	}
	if n := len(globalLayer(t, st, "acme", "passes")); n != 0 {
		t.Errorf("acme's failed batch published %d message(s), want 0", n)
	}
	if memoryHookLanded(t, st, "acme") {
		t.Error("memory.set fired for acme's failed batch")
	}
}

// TestFanoutHooks_ChannelResolvesInTheRunsTenant: which channel a hook writes
// to is decided in the tenant it writes into, so a tenant's own declaration of
// the channel governs a message stored in that tenant.
func TestFanoutHooks_ChannelResolvesInTheRunsTenant(t *testing.T) {
	sched, _, st, _ := fanoutFixture(t, hookedFanoutDef(), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	rec := &tenantRecorder{scope: "global"}
	sched.SetChannelScope(rec.resolve)

	seedSettledSession(t, st, "default", "alice")
	seedSettledSession(t, st, "acme", "sam")

	fireT(t, sched)

	got := rec.tenants()
	sort.Strings(got)
	if strings.Join(got, ",") != "acme,default" {
		t.Fatalf("resolved the hook's channel in tenants %q, want [acme default]", got)
	}
}

// TestFanoutHooks_TenantScheduleWritesOnlyIntoItsOwnTenant: a tenant's fan-out
// is unchanged — its runs execute in its tenant, and so do its hooks, once per
// fire. Other tenants' users exist and receive nothing.
func TestFanoutHooks_TenantScheduleWritesOnlyIntoItsOwnTenant(t *testing.T) {
	def := hookedFanoutDef()
	def.TenantID = "acme"
	sched, fr, st, _ := fanoutFixture(t, def, nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	sched.SetChannelScope(func(context.Context, string, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global"}, true
	})
	fr.resultFor = runIDPerTenant

	seedSettledSession(t, st, "acme", "sam")
	seedSettledSession(t, st, "acme", "tia")
	seedSettledSession(t, st, "default", "alice")

	fireT(t, sched)

	if n := len(globalLayer(t, st, "acme", "passes")); n != 1 {
		t.Errorf("acme's schedule published %d completion message(s) in acme, want 1 per fire", n)
	}
	for _, other := range []string{"", "default"} {
		if n := len(globalLayer(t, st, other, "passes")); n != 0 {
			t.Errorf("acme's schedule published %d message(s) into tenant %q", n, other)
		}
		if memoryHookLanded(t, st, other) {
			t.Errorf("acme's schedule wrote memory into tenant %q", other)
		}
	}
	if !memoryHookLanded(t, st, "acme") {
		t.Error("acme's schedule's memory.set did not land in acme")
	}
}

// TestFanoutHooks_TenantOwnedDefWithEmptyBodyTenantWritesOnlyIntoTheOperatorLayer:
// a row a tenant wrote before create stamped the body's tenant stays confined
// to "" (see operatorLayerFanout) — and so do its hooks. It must not become a
// way for a tenant's schedule to write into other tenants.
func TestFanoutHooks_TenantOwnedDefWithEmptyBodyTenantWritesOnlyIntoTheOperatorLayer(t *testing.T) {
	sched, _, st := fanoutFixtureOwnedBy(t, hookedFanoutDef(), "acme")
	sched.SetChannelScope(func(context.Context, string, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global"}, true
	})

	seedSettledSession(t, st, "", "olga")
	seedSettledSession(t, st, "default", "alice")

	fireT(t, sched)

	if n := len(globalLayer(t, st, "default", "passes")); n != 0 {
		t.Errorf("a tenant-owned def published %d message(s) into tenant default", n)
	}
	if n := len(globalLayer(t, st, "", "passes")); n != 1 {
		t.Errorf("the confined def published %d message(s) in its own layer, want 1", n)
	}
}

// TestScheduler_SingleRunHooksWriteIntoTheRunsTenant: a single-run schedule's
// run executes in the def's tenant, so its hooks already write where its run
// did — "" for an operator-layer schedule, the tenant for a tenant's. Pinned so
// the fan-out change cannot move them.
func TestScheduler_SingleRunHooksWriteIntoTheRunsTenant(t *testing.T) {
	for _, tenant := range []string{"", "acme"} {
		def := channelHookDef("passes")
		def.TenantID = tenant
		sched, fr, _, _, st := schedulerFixture(t, def, time.Now().Add(-time.Minute))
		sched.SetChannelScope(func(context.Context, string, string) (DeclaredChannel, bool) {
			return DeclaredChannel{Scope: "global"}, true
		})

		fireT(t, sched)

		if calls := fr.Calls(); len(calls) != 1 || calls[0].TenantID != tenant {
			t.Fatalf("tenant %q: run calls = %+v", tenant, calls)
		}
		if n := len(globalLayer(t, st, tenant, "passes")); n != 1 {
			t.Errorf("tenant %q: the hook published %d message(s) in the run's tenant, want 1", tenant, n)
		}
		if tenant != "" {
			if n := len(globalLayer(t, st, "", "passes")); n != 0 {
				t.Errorf("tenant %q: the hook published %d message(s) into the operator layer", tenant, n)
			}
		}
	}
}
