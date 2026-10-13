package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// heldProvider answers each call only once its gate is closed, so a test can
// keep a provider's slot taken for as long as it needs.
type heldProvider struct{ gate chan struct{} }

func (p *heldProvider) ID() string                    { return "held" }
func (p *heldProvider) Probe(_ context.Context) error { return nil }
func (p *heldProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"m"}, nil
}
func (p *heldProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *heldProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event, 2)
	go func() {
		defer close(ch)
		select {
		case <-p.gate:
		case <-ctx.Done():
			return
		}
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn",
			Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	}()
	return ch, nil
}

// gatewayResult is one gateway response.
type gatewayResult struct {
	status int
	body   string
}

func postGateway(ts *httptest.Server, body string) <-chan gatewayResult {
	out := make(chan gatewayResult, 1)
	go func() {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
		if err != nil {
			out <- gatewayResult{body: err.Error()}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out <- gatewayResult{resp.StatusCode, string(raw)}
	}()
	return out
}

const gatewayBodyP = `{"messages":[{"role":"user","content":"hi"}],"max_tokens":8,"provider":"P","model":"m"}`

// TestLLMGateway_RequestWaitingForItsProviderHoldsNoGlobalSlot — a gateway
// request is sent directly, outside any run, so nothing else limits how many
// can pile onto a saturated provider. With provider P at max_concurrent 1 and
// provider_queue_depth 1: the first request computes, the second waits for P
// holding NO global slot, and the third is refused with 429
// provider_concurrency_exhausted, holding nothing.
//
// If a waiting request kept its global slot, a handful queued on a saturated
// provider would use the global cap up for every tenant.
func TestLLMGateway_RequestWaitingForItsProviderHoldsNoGlobalSlot(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Concurrency.ProviderQueueDepth = 1
	prov := &heldProvider{gate: make(chan struct{})}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": prov})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	first := postGateway(ts, gatewayBodyP)
	waitForLane(t, sched, "P", 1, 0)
	second := postGateway(ts, gatewayBodyP)
	waitForLane(t, sched, "P", 1, 1)

	// Only the computing request holds a global slot; the waiting one gave its
	// own back before it queued.
	if st := srv.sem.Stats(); st.Active != 1 || st.Queued != 0 {
		t.Fatalf("global slots while one request waits for P: active=%d queued=%d, want 1/0", st.Active, st.Queued)
	}

	// P's queue is at its bound: the next request is refused at once.
	third := <-postGateway(ts, gatewayBodyP)
	if third.status != http.StatusTooManyRequests {
		t.Fatalf("third request: status=%d body=%s, want 429", third.status, third.body)
	}
	var refusal struct {
		Code     string `json:"code"`
		Provider string `json:"provider"`
		Cap      int    `json:"cap"`
	}
	if err := json.Unmarshal([]byte(third.body), &refusal); err != nil {
		t.Fatalf("refusal body %q: %v", third.body, err)
	}
	if refusal.Code != "provider_concurrency_exhausted" || refusal.Provider != "P" || refusal.Cap != 1 {
		t.Fatalf("refusal = %+v, want provider_concurrency_exhausted for P with cap 1", refusal)
	}
	if st := srv.sem.Stats(); st.Active != 1 {
		t.Fatalf("global active after the refusal = %d, want 1: a refused request holds nothing", st.Active)
	}
	waitForLane(t, sched, "P", 1, 1)

	close(prov.gate)
	for name, ch := range map[string]<-chan gatewayResult{"first": first, "second": second} {
		if r := <-ch; r.status != http.StatusOK {
			t.Errorf("%s request: status=%d body=%s, want 200", name, r.status, r.body)
		}
	}
	waitForLane(t, sched, "P", 0, 0)
	if st := srv.sem.Stats(); st.Active != 0 {
		t.Errorf("global active at the end = %d, want 0", st.Active)
	}
}

// TestLLMGateway_ProviderSlotIsGivenBackWhenNoGlobalSlotIsFree — a request that
// waited for its provider then needs a global slot, and must not WAIT for one
// while it holds the provider's: a run takes its global slot and then waits for
// the provider, so the two would face each other. With the global cap taken by
// the time the provider frees up, the request is refused (429 backpressure) and
// gives the provider slot straight back.
func TestLLMGateway_ProviderSlotIsGivenBackWhenNoGlobalSlotIsFree(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Concurrency.ProviderQueueDepth = 4
	prov := &heldProvider{gate: make(chan struct{})}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": prov})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Something else holds P's one slot, and a gateway request queues behind it
	// (having given its global slot back).
	relP, err := sched.Acquire(context.Background(), "P", "chat", concurrency.CallInfo{})
	if err != nil {
		t.Fatal(err)
	}
	waiting := postGateway(ts, gatewayBodyP)
	waitForLane(t, sched, "P", 1, 1)

	// Every global slot is taken before P frees up (lanedServer's cap is 16).
	var held []func()
	for i := 0; i < 16; i++ {
		rel, err := srv.sem.Acquire(context.Background())
		if err != nil {
			t.Fatalf("take global slot %d: %v", i, err)
		}
		held = append(held, rel)
	}
	relP()

	r := <-waiting
	if r.status != http.StatusTooManyRequests || !strings.Contains(r.body, `"backpressure"`) {
		t.Fatalf("request granted P with no global slot free: status=%d body=%s, want 429 backpressure", r.status, r.body)
	}
	waitForLane(t, sched, "P", 0, 0)
	for _, rel := range held {
		rel()
	}
}
