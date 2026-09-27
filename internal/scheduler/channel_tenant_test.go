package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
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
