package webhook

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

const (
	senderTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	senderSpanID  = "00f067aa0ba902b7"
)

// spanRunner opens the run span on the ctx the delivery's run starts on.
type spanRunner struct {
	mu   sync.Mutex
	done chan struct{}
}

func (r *spanRunner) RunOnce(ctx context.Context, _ runner.RunInput, cb runner.RunCallbacks) error {
	_, span := lcotel.RecordRunStart(ctx, lcotel.RunStartAttrs{RunID: "run-1"})
	span.End()
	if cb.OnRegistered != nil {
		cb.OnRegistered("agent-1", "run-1", "sess-1", "")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	return nil
}

func deliverWithTraceparent(t *testing.T, sign bool) tracetest.SpanStub {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restoreTP := lcotel.SetTracerProviderForTest(tp)
	restoreIn := lcotel.SetInboundForTest(lcotel.InboundParent, propagation.TraceContext{})
	t.Cleanup(func() { restoreIn(); restoreTP(); _ = tp.Shutdown(context.Background()) })

	secret := "shhh"
	body := []byte(`{"goal":"go"}`)
	auth := config.WebhookAuth{Kind: "hmac", Header: "X-Hub-Signature-256", SigningSecretEnv: "WH_SECRET"}
	if !sign {
		auth = config.WebhookAuth{Kind: "none"}
	}
	wh := config.Webhook{Enabled: true, Delivery: "spawn", Agent: "researcher", Auth: auth,
		PayloadMapping: map[string]string{"goal": "$.goal"}}
	sr := &spanRunner{done: make(chan struct{})}
	rec := newTestReceiver(t, map[string]config.Webhook{"gh": wh}, sr, nil,
		map[string]string{"WH_SECRET": secret}, []string{"WH_SECRET"}, time.Unix(1_700_000_000, 0))
	rec.allowUnauthenticated = true

	h := http.Header{}
	h.Set("traceparent", "00-"+senderTraceID+"-"+senderSpanID+"-01")
	if sign {
		h.Set("X-Hub-Signature-256", githubSig(secret, body))
	}
	if w := doPost(rec, "gh", body, h); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	select {
	case <-sr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery's run never started")
	}
	for _, s := range exp.GetSpans() {
		if s.Name == lcotel.SpanRun {
			return s
		}
	}
	t.Fatal("no run span")
	return tracetest.SpanStub{}
}

// TestDelivery_VerifiedSendersTraceparentParentsTheRun: a signed delivery's
// run joins the sender's trace, though it starts on a detached context.
func TestDelivery_VerifiedSendersTraceparentParentsTheRun(t *testing.T) {
	s := deliverWithTraceparent(t, true)
	if s.SpanContext.TraceID().String() != senderTraceID || s.Parent.SpanID().String() != senderSpanID {
		t.Errorf("run trace=%s parent=%s, want the sender's %s/%s", s.SpanContext.TraceID(), s.Parent.SpanID(), senderTraceID, senderSpanID)
	}
}

// TestDelivery_UnauthenticatedDeliveryIgnoresTraceparent: auth.kind=none
// verifies nobody, so the header is not read.
func TestDelivery_UnauthenticatedDeliveryIgnoresTraceparent(t *testing.T) {
	s := deliverWithTraceparent(t, false)
	if s.Parent.IsValid() || s.SpanContext.TraceID().String() == senderTraceID {
		t.Errorf("an unauthenticated delivery's run joined the sender's trace")
	}
}
