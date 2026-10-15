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

// TestLLMGateway_WaitsForARunSlotInTheQueueWithoutTimingOut — with every run
// slot taken, a gateway request waits in the run queue, as a run does: it does
// not give up after the semaphore's own timeout (here 50ms) and answer 429, and it gets
// the first slot that frees.
func TestLLMGateway_WaitsForARunSlotInTheQueueWithoutTimingOut(t *testing.T) {
	cfg := makeBaseConfig()
	prov := &heldProvider{gate: make(chan struct{})}
	close(prov.gate) // P answers at once: only the run slot is scarce
	srv, _ := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": prov})
	srv.sem = concurrency.New(1, 8, 50*time.Millisecond)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	relRun, err := srv.sem.Admit(context.Background(), concurrency.AdmitRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waiting := postGateway(ts, gatewayBodyFor("alice"))
	deadline := time.Now().Add(3 * time.Second)
	for srv.sem.Stats().Queued != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the gateway request never queued for a run slot: %+v", srv.sem.Stats())
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // four times the old wait timeout
	select {
	case r := <-waiting:
		t.Fatalf("the request gave up while queued: status=%d body=%s, want it still waiting", r.status, r.body)
	default:
	}

	relRun()
	if r := <-waiting; r.status != http.StatusOK {
		t.Fatalf("after a slot freed: status=%d body=%s, want 200", r.status, r.body)
	}
	if st := srv.sem.Stats(); st.Active != 0 || st.Queued != 0 {
		t.Errorf("at the end: active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}
}

func gatewayBodyFor(user string) string {
	return `{"messages":[{"role":"user","content":"hi"}],"max_tokens":8,"provider":"P","model":"m","user_id":"` + user + `"}`
}

// TestLLMGateway_RequestWaitingForItsProviderStaysCountedAgainstItsUser — a
// request waiting for its provider has given its global slot back, but it is
// still that user's request: it stays counted against the per-user cap. With a
// cap of 1, a user whose request is waiting cannot send another; a different
// user can.
//
// Counted only while it held a global slot, one user could pile waiting
// requests onto the provider past their cap and fill its bounded queue for
// everyone else.
func TestLLMGateway_RequestWaitingForItsProviderStaysCountedAgainstItsUser(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Concurrency.ProviderQueueDepth = 8
	prov := &heldProvider{gate: make(chan struct{})}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": prov})
	srv.sem.WithPerUserCap(1)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	relP, err := sched.Acquire(context.Background(), "P", "chat", concurrency.CallInfo{})
	if err != nil {
		t.Fatal(err)
	}
	aliceWaiting := postGateway(ts, gatewayBodyFor("alice"))
	waitForLane(t, sched, "P", 1, 1)
	if st := srv.sem.Stats(); st.Active != 0 || st.PerUser["alice"] != 1 {
		t.Fatalf("alice waiting for P: global active=%d perUser=%v, want no global slot and alice counted once", st.Active, st.PerUser)
	}

	again := <-postGateway(ts, gatewayBodyFor("alice"))
	if again.status != http.StatusTooManyRequests || !strings.Contains(again.body, `"per_user_quota_exhausted"`) {
		t.Fatalf("alice's second request while her first waits: status=%d body=%s, want 429 per_user_quota_exhausted", again.status, again.body)
	}
	waitForLane(t, sched, "P", 1, 1) // it joined no queue

	bobWaiting := postGateway(ts, gatewayBodyFor("bob"))
	waitForLane(t, sched, "P", 1, 2)

	relP()
	close(prov.gate)
	for name, ch := range map[string]<-chan gatewayResult{"alice": aliceWaiting, "bob": bobWaiting} {
		if r := <-ch; r.status != http.StatusOK {
			t.Errorf("%s: status=%d body=%s, want 200", name, r.status, r.body)
		}
	}
	waitForLane(t, sched, "P", 0, 0)
	if st := srv.sem.Stats(); st.Active != 0 || len(st.PerUser) != 0 {
		t.Errorf("at the end: global active=%d perUser=%v, want 0 and nobody counted", st.Active, st.PerUser)
	}
}
