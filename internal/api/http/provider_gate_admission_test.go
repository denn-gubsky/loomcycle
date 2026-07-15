package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gateTestConfig returns a base config with two pinned agents: "capped" →
// provider "capped-prov" and "uncapped" → provider "uncapped-prov". The gate
// keys off the resolved provider id (= the agent's Provider pin), so this lets a
// test cap one provider and leave another uncapped.
func gateTestConfig() *config.Config {
	cfg := makeBaseConfig()
	cfg.Agents["capped"] = config.AgentDef{Provider: "capped-prov", Model: "m", Tools: []string{}}
	cfg.Agents["uncapped"] = config.AgentDef{Provider: "uncapped-prov", Model: "m", Tools: []string{}}
	return cfg
}

func gateRunBody(agent, userID string) string {
	return `{"agent":"` + agent + `","user_id":"` + userID +
		`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`
}

// waitForGate polls a provider gate's Stats until (active,queued) match or the
// deadline elapses — poll-until instead of a fixed sleep so -race doesn't flake.
func waitForGate(t *testing.T, gates *concurrency.ProviderGates, id string, wantActive, wantQueued int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		st := gates.Stats()[id]
		if st.Active == wantActive && st.Queued == wantQueued {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := gates.Stats()[id]
	t.Fatalf("waitForGate(%s): timed out; want active=%d queued=%d, got active=%d queued=%d",
		id, wantActive, wantQueued, st.Active, st.Queued)
}

// TestProviderGate_CappedOverflowDoesNotHoldGlobalSlot is THE ordering guard:
// with provider "capped-prov" capped at 1 and the global cap high, a first
// capped run + an uncapped run both admit (2 global slots held); a SECOND capped
// run queues on the PROVIDER gate and — critically — does NOT hold a global slot
// (global active stays 2, gate queued 1). This proves the provider gate is
// acquired BEFORE the global slot, so a saturated provider's overflow can't
// starve runs targeting other providers.
func TestProviderGate_CappedOverflowDoesNotHoldGlobalSlot(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	cfg := gateTestConfig()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sem := concurrency.New(8, 8, time.Second) // global cap high — never gates first
	// Long queue timeout so the 2nd capped run STAYS queued while we observe it.
	gates := concurrency.NewProviderGates(map[string]int{"capped-prov": 1}, 4, 5*time.Second)
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())

	// Robust teardown: even if an assertion below fails (t.Fatalf), the held
	// runs must be released so ts.Close() doesn't block on in-flight requests.
	// Cleanups run LIFO — register ts.Close FIRST so the drain runs before it.
	var wg sync.WaitGroup
	var releaseOnce sync.Once
	drain := func() { releaseOnce.Do(func() { close(prov.release) }) }
	t.Cleanup(ts.Close)
	t.Cleanup(func() { drain(); wg.Wait() })

	post := func(agent, user string) {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(gateRunBody(agent, user)))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	// Run A: capped-prov, takes the single gate slot + a global slot.
	wg.Add(1)
	go func() { defer wg.Done(); post("capped", "a") }()
	waitForGate(t, gates, "capped-prov", 1, 0, 2*time.Second)

	// Run B: uncapped-prov, admits immediately (NOT starved) → global active 2.
	wg.Add(1)
	go func() { defer wg.Done(); post("uncapped", "b") }()
	waitForActive(t, sem, 2, 2*time.Second)

	// Run C: a SECOND capped-prov run — must queue on the provider gate.
	wg.Add(1)
	go func() { defer wg.Done(); post("capped", "c") }()
	waitForGate(t, gates, "capped-prov", 1, 1, 2*time.Second)

	// THE ASSERTION: C is queued on the gate but holds NO global slot — global
	// active is still 2 (A + B), not 3. If the global slot were acquired before
	// the gate, C would be occupying a 3rd global slot here.
	if got := sem.Stats().Active; got != 2 {
		t.Fatalf("global active = %d while a capped run is gate-queued; want 2 (queued run must NOT hold a global slot)", got)
	}

	drain()
	wg.Wait()
	if g := gates.Stats()["capped-prov"]; g.Active != 0 || g.Queued != 0 {
		t.Errorf("final gate state active=%d queued=%d, want 0/0", g.Active, g.Queued)
	}
}

// TestProviderGate_BatchesToCap — N > cap runs to a capped provider run at most
// `cap` at a time through the FULL admission path; the observed peak concurrency
// (measured inside provider.Call) equals the cap exactly (the operator's VRAM
// goal). The rest queue and drain.
func TestProviderGate_BatchesToCap(t *testing.T) {
	const cap = 2
	const total = 5
	prov := &countingProvider{delay: 40 * time.Millisecond}
	cfg := gateTestConfig()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sem := concurrency.New(16, 16, 5*time.Second) // global never gates first
	gates := concurrency.NewProviderGates(map[string]int{"capped-prov": cap}, total, 5*time.Second)
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(gateRunBody("capped", "u")))
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	if peak := prov.peakInFlight(); peak != cap {
		t.Errorf("peak concurrent provider.Call = %d, want exactly cap=%d", peak, cap)
	}
	if g := gates.Stats()["capped-prov"]; g.Active != 0 || g.Queued != 0 {
		t.Errorf("final gate state active=%d queued=%d, want 0/0", g.Active, g.Queued)
	}
}

// TestProviderGate_OverCapReturns429 — an over-cap admission past a full queue
// returns HTTP 429 with code "provider_concurrency_exhausted" + Retry-After and
// the provider/cap fields.
func TestProviderGate_OverCapReturns429(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	cfg := gateTestConfig()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "429.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sem := concurrency.New(8, 8, time.Second)
	// cap 1, queue depth 0 → the 2nd run is refused immediately (deterministic).
	gates := concurrency.NewProviderGates(map[string]int{"capped-prov": 1}, 0, time.Second)
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())

	var wg sync.WaitGroup
	var releaseOnce sync.Once
	drain := func() { releaseOnce.Do(func() { close(prov.release) }) }
	t.Cleanup(ts.Close)
	t.Cleanup(func() { drain(); wg.Wait() })

	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(gateRunBody("capped", "a")))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitForGate(t, gates, "capped-prov", 1, 0, 2*time.Second)

	// 2nd capped run: gate full (depth 0) → 429.
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(gateRunBody("capped", "b")))
	if err != nil {
		t.Fatalf("2nd post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Code     string `json:"code"`
		Provider string `json:"provider"`
		Cap      int    `json:"cap"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if parsed.Code != "provider_concurrency_exhausted" || parsed.Provider != "capped-prov" || parsed.Cap != 1 {
		t.Errorf("body = %+v, want code=provider_concurrency_exhausted provider=capped-prov cap=1", parsed)
	}

	drain()
	wg.Wait()
}

// TestProviderGate_ZeroOverheadWhenUnconfigured — with no max_concurrent
// anywhere, no gates are built and admission is the unchanged path: many
// concurrent runs to the same provider all admit (only the global cap applies),
// and the gate holder is a noop.
func TestProviderGate_ZeroOverheadWhenUnconfigured(t *testing.T) {
	prov := &countingProvider{delay: 20 * time.Millisecond}
	cfg := gateTestConfig()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sem := concurrency.New(8, 8, 5*time.Second)
	// The default deployment: NewProviderGates with an empty caps map → zero gates.
	gates := concurrency.NewProviderGates(map[string]int{}, 16, time.Second)
	if gates.Len() != 0 {
		t.Fatalf("expected 0 gates built, got %d", gates.Len())
	}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, sem, st)
	srv.SetProviderGates(gates)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(gateRunBody("capped", "u")))
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (no gate should apply)", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	// No gate means no per-provider ceiling; peak is bounded only by the global
	// cap, so all n overlapped (peak > 1 proves the path wasn't accidentally
	// serialised). Gate stats stay empty.
	if peak := prov.peakInFlight(); peak <= 1 {
		t.Errorf("peak = %d; with no gate all %d runs should overlap (peak > 1)", peak, n)
	}
	if gates.Stats() != nil {
		t.Errorf("gates.Stats() = %v, want nil (no gates configured)", gates.Stats())
	}
}

// TestProviderSlot_SwapMovesSlotNoLeak covers the fallback slot handling
// (Deliverable 4): swapping a run's provider slot from P to Q releases P's slot
// and holds Q's, and after the final release BOTH gate counts return to 0 — no
// leak. Also covers the same-provider no-op and the uncapped-target case.
func TestProviderSlot_SwapMovesSlotNoLeak(t *testing.T) {
	gates := concurrency.NewProviderGates(map[string]int{"P": 1, "Q": 1}, 2, time.Second)

	slot, err := (&Server{providerGates: gates}).acquireProviderSlot(context.Background(), "P")
	if err != nil {
		t.Fatalf("acquire P: %v", err)
	}
	if g := gates.Stats()["P"]; g.Active != 1 {
		t.Fatalf("P active = %d, want 1", g.Active)
	}

	// Same-provider swap is a no-op — keeps P's slot (a same-provider retry must
	// not surrender its slot to a competitor).
	slot.swap(context.Background(), gates, "P")
	if g := gates.Stats()["P"]; g.Active != 1 {
		t.Fatalf("after same-provider swap P active = %d, want 1 (kept)", g.Active)
	}

	// Swap P → Q: P released, Q acquired.
	slot.swap(context.Background(), gates, "Q")
	if gP, gQ := gates.Stats()["P"], gates.Stats()["Q"]; gP.Active != 0 || gQ.Active != 1 {
		t.Fatalf("after P->Q swap: P active=%d (want 0) Q active=%d (want 1)", gP.Active, gQ.Active)
	}
	if slot.providerID != "Q" {
		t.Errorf("holder providerID = %q, want Q", slot.providerID)
	}

	// Final release frees Q — no slot leaked on either gate.
	slot.releaseCurrent()
	if gP, gQ := gates.Stats()["P"], gates.Stats()["Q"]; gP.Active != 0 || gQ.Active != 0 {
		t.Errorf("after release: P active=%d Q active=%d, want 0/0 (no leak)", gP.Active, gQ.Active)
	}
}

// countingProvider records the peak number of concurrent provider.Call
// executions — used to assert the gate limits real in-flight LLM calls to the
// cap through the full admission + loop path.
type countingProvider struct {
	delay time.Duration

	mu       sync.Mutex
	inFlight int
	peak     int
}

func (c *countingProvider) ID() string                    { return "counting" }
func (c *countingProvider) Probe(_ context.Context) error { return nil }
func (c *countingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"m"}, nil
}
func (c *countingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (c *countingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
	c.mu.Unlock()
	ch := make(chan providers.Event, 4)
	go func() {
		defer close(ch)
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
		}
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
		ch <- providers.Event{Type: providers.EventText, Text: "ok"}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	}()
	return ch, nil
}
func (c *countingProvider) peakInFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}
