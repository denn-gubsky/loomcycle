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

// TestLLMGateway_SaturatedProviderRefusesPastTheBoundAndGivesItsSlotBack — a
// gateway request is sent directly, outside any run, and holds its global slot
// while it waits for its provider, so that wait is BOUNDED. With provider P at
// max_concurrent 1 and provider_queue_depth 1: the first request computes, the
// second waits for P, and the third is refused with 429
// provider_concurrency_exhausted and gives its global slot back.
//
// With an unbounded wait, every request queued on a saturated provider would
// sit on a global slot and one caller could use the global cap up for every
// tenant. The slots are taken global-first, the order a run uses: lane-first, a
// request would hold a provider slot while it waited for a global slot held by
// a run that is waiting for that provider slot.
func TestLLMGateway_SaturatedProviderRefusesPastTheBoundAndGivesItsSlotBack(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Concurrency.ProviderQueueDepth = 1
	prov := &heldProvider{gate: make(chan struct{})}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": prov})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	const body = `{"messages":[{"role":"user","content":"hi"}],"max_tokens":8,"provider":"P","model":"m"}`
	client := &http.Client{Timeout: 30 * time.Second}
	type result struct {
		status int
		body   string
	}
	post := func() <-chan result {
		out := make(chan result, 1)
		go func() {
			resp, err := client.Post(ts.URL+"/v1/_llm/chat", "application/json", strings.NewReader(body))
			if err != nil {
				out <- result{body: err.Error()}
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			out <- result{resp.StatusCode, string(raw)}
		}()
		return out
	}

	first := post()
	waitForLane(t, sched, "P", 1, 0)
	second := post()
	waitForLane(t, sched, "P", 1, 1)

	// Both hold a global slot: the one computing and the one waiting for P.
	if st := srv.sem.Stats(); st.Active != 2 {
		t.Fatalf("global slots with one request computing and one waiting for P: active=%d, want 2", st.Active)
	}

	// P's queue is at its bound: the next request is refused at once.
	third := <-post()
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
	if st := srv.sem.Stats(); st.Active != 2 {
		t.Fatalf("global active after the refusal = %d, want 2: a refused request gives its slot back", st.Active)
	}
	waitForLane(t, sched, "P", 1, 1)

	close(prov.gate)
	for name, ch := range map[string]<-chan result{"first": first, "second": second} {
		if r := <-ch; r.status != http.StatusOK {
			t.Errorf("%s request: status=%d body=%s, want 200", name, r.status, r.body)
		}
	}
	waitForLane(t, sched, "P", 0, 0)
	if st := srv.sem.Stats(); st.Active != 0 {
		t.Errorf("global active at the end = %d, want 0", st.Active)
	}
}
