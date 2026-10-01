package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// tenantRecorder is a channel resolver that remembers which tenant it was
// asked about, and declares every channel at the given scope.
type tenantRecorder struct {
	mu    sync.Mutex
	scope string
	asked []string
}

func (r *tenantRecorder) resolve(_ context.Context, tenantID, _ string) (DeclaredChannel, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, tenantID)
	return DeclaredChannel{Scope: r.scope}, true
}

func (r *tenantRecorder) tenants() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// A schedule resolves the channel it publishes to in its OWN tenant. The sweep
// runs on a ctx with no identity, so a resolver reading the tenant from ctx
// looked in the shared tenant: a tenant's runtime channel was "not declared"
// and the tick or hook was lost.
func TestScheduler_ChannelTickResolvesInTheSchedulesTenant(t *testing.T) {
	enabled := true
	def := scheduleDef{
		Delivery: "channel", Channel: "inbox", Schedule: "0 * * * *",
		Enabled: &enabled, TenantID: "t1",
	}
	sched, _, _, _, _ := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	rec := &tenantRecorder{scope: "global"}
	sched.SetChannelScope(rec.resolve)

	fireT(t, sched)

	if got := rec.tenants(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("resolved the channel in tenants %q, want [t1]", got)
	}
}

func TestScheduler_OnCompletePublishResolvesInTheSchedulesTenant(t *testing.T) {
	def := channelHookDef("inbox")
	def.TenantID = "t1"
	sched, _, _, _, _ := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	rec := &tenantRecorder{scope: "global"}
	sched.SetChannelScope(rec.resolve)

	fireT(t, sched)

	if got := rec.tenants(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("resolved the channel in tenants %q, want [t1]", got)
	}
}

// A `scope: tenant` channel is a valid publish target: shared across the
// schedule's tenant (scope_id ""), keyed by the message's tenant. It used to
// fail every publish with "unknown scope".
func TestScheduler_PublishesToATenantScopedChannel(t *testing.T) {
	def := channelHookDef("team-news")
	def.TenantID = "t1"
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope((&tenantRecorder{scope: "tenant"}).resolve)

	fireT(t, sched)

	msgs, err := st.ChannelPeek(context.Background(), "t1", "team-news", store.MemoryScopeTenant, "", "", 10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("t1's tenant-scoped channel holds %d messages, want 1", len(msgs))
	}
}

// A channel tick starts no run, so it lands in the schedule's own tenant. For an
// operator-layer schedule (a yaml entry with no tenant_id) that is the operator
// layer of a global channel, which every tenant reads merged with its own: a
// legacy-bearer reader in "default" and a minted-token reader in "acme" both
// receive the tick. Pinned because "the tick lands in tenant \"\"" reads like
// the same bug as the fan-out's hooks and is not: the operator layer IS where an
// operator's writer publishes to reach every tenant.
func TestScheduler_OperatorChannelTickReachesEveryTenantsReader(t *testing.T) {
	enabled := true
	def := scheduleDef{Delivery: "channel", Channel: "clock", Schedule: "0 * * * *", Enabled: &enabled}
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope((&tenantRecorder{scope: "global"}).resolve)

	fireT(t, sched)

	for _, reader := range []string{"default", "acme"} {
		msgs, _, err := st.ChannelSubscribe(context.Background(), reader, "clock", store.MemoryScopeGlobal, "", "", 10)
		if err != nil {
			t.Fatalf("subscribe as %q: %v", reader, err)
		}
		if len(msgs) != 1 {
			t.Errorf("a reader in tenant %q received %d tick(s), want 1", reader, len(msgs))
		}
	}
}

// A tenant's channel tick stays in that tenant: another tenant's reader of the
// same global channel never sees it.
func TestScheduler_TenantChannelTickStaysInItsTenant(t *testing.T) {
	enabled := true
	def := scheduleDef{Delivery: "channel", Channel: "clock", Schedule: "0 * * * *", Enabled: &enabled, TenantID: "acme"}
	sched, _, _, _, st := schedulerFixture(t, def, time.Now().Add(-1*time.Minute))
	sched.SetChannelScope((&tenantRecorder{scope: "global"}).resolve)

	fireT(t, sched)

	for reader, want := range map[string]int{"acme": 1, "default": 0, "": 0} {
		msgs, err := st.ChannelPeek(context.Background(), reader, "clock", store.MemoryScopeGlobal, "", "", 10)
		if err != nil {
			t.Fatalf("peek as %q: %v", reader, err)
		}
		if len(msgs) != want {
			t.Errorf("a reader in tenant %q sees %d of acme's ticks, want %d", reader, len(msgs), want)
		}
	}
}
