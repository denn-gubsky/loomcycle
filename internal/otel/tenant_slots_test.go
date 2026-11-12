package otel

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// A tenant whose collector never answers holds each export slot it takes for
// a whole export timeout. However many exports it starts (span batches,
// metric pushes, tests), it holds at most its own share, and a healthy
// tenant's export still finds a slot.
func TestTenantRouter_StalledTenantsCannotTakeEveryExportSlot(t *testing.T) {
	clearOTELEnv(t)
	release := make(chan struct{})
	var once sync.Once
	hole := newFakeCollector(t)
	hole.handler = func(http.ResponseWriter, *http.Request) bool { <-release; return true }
	ok := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{MaxConcurrentExports: 5, ExportTimeout: 5 * time.Second})
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = r.Shutdown(context.Background()) })

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Test(context.Background(), fmt.Sprint("hole-", i%2), dest(hole, nil), 5*time.Second)
		}()
	}
	waitFor(t, "the stalled tenants' exports", func() bool { return len(hole.gotHeaders()) >= 4 })
	time.Sleep(50 * time.Millisecond) // any export past the caps would be in by now
	if err := r.Test(context.Background(), "ok", dest(ok, nil), 3*time.Second); err != nil {
		t.Errorf("a healthy tenant's test with two stalled tenants: %v, want it sent", err)
	}
	once.Do(func() { close(release) })
	wg.Wait()
}

// A span batch waits a short while for a slot, not a whole export timeout: it
// is dropped as busy and the pipeline moves on to its next batch.
func TestTenantRouter_ASpanBatchWaitsBrieflyForASlot(t *testing.T) {
	clearOTELEnv(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	hole := newFakeCollector(t)
	hole.handler = func(http.ResponseWriter, *http.Request) bool { <-release; return true }
	t.Cleanup(unblock) // before the collector's Close, which waits on its handlers
	ok := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{MaxConcurrentExports: 1, ExportTimeout: 10 * time.Second, ExportSlotWait: 100 * time.Millisecond})
	r.SetDestination("hole", dest(hole, nil))
	r.SetDestination("ok", dest(ok, nil))
	tp := routedProvider(t, r)
	t.Cleanup(unblock) // before the provider's shutdown flushes the stalled export
	end := func(tenant string) {
		_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(AttrTenant, tenant)))
		s.End()
	}
	end("hole")
	waitFor(t, "the stalled export", func() bool { return len(hole.gotHeaders()) == 1 })
	start := time.Now()
	end("ok")
	waitFor(t, "the healthy tenant's batch dropped as busy", func() bool { return r.Status("ok").Dropped == 1 })
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("the batch waited %v for a slot", el)
	}
	if got := r.Status("ok").LastError; got != CategoryBusy {
		t.Errorf("last error = %q, want %q", got, CategoryBusy)
	}
}
