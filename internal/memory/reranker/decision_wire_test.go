package reranker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
)

// The wire shape of the one request a decision rerank sends, as the endpoint
// double in decision_test.go decodes it. The reranker no longer builds the
// request itself (the shared decision driver does), so these describe what must
// arrive, not what any production type holds.
const decisionPath = "/v1/systemone"

type systemOneRequest struct {
	Model     string                       `json:"model"`
	State     map[string]string            `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// TestDecision_TheRequestIsByteForByteTheMeasuredOne — the rerank's numbers were
// measured against one exact request. This is that request, to the byte, so a
// change to how it is built (the shared decision driver builds it now) cannot
// alter what the model is asked without failing here.
func TestDecision_TheRequestIsByteForByteTheMeasuredOne(t *testing.T) {
	var got, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got, contentType = string(raw), r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, `{"answers":{"best":{"type":"choice","probabilities":{"A":0.1,"B":0.9}}}}`)
	}))
	defer srv.Close()
	_, rep := newDecision(t, srv.URL, config.RerankerConfig{}).Rank(context.Background(), `which <one> & "why"?`, []string{"alpha one", "bravo two"}, 5)
	if !rep.Applied {
		t.Fatalf("report = %+v, want applied", rep)
	}
	// encoding/json writes <, > and & as escapes; esc spells one without this
	// file containing the escape itself.
	esc := func(hex string) string { return string(rune('\\')) + "u" + hex }
	want := `{"model":"nimble","state":{"question":"which ` + esc("003c") + `one` + esc("003e") + ` ` + esc("0026") + ` \"why\"?"},` +
		`"questions":{"best":{"type":"choice","instructions":"Which passage best answers the question?",` +
		`"criteria":{"A":"alpha","B":"bravo"}}}}`
	if got != want {
		t.Errorf("request body\n got: %s\nwant: %s", got, want)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
}

// TestDecision_AnAliasReachesTheProviderAsItsModel — `memory.reranker.model`
// naming a models: alias asks the provider for the model the alias names. The
// alias name itself is not a model the provider serves.
func TestDecision_AnAliasReachesTheProviderAsItsModel(t *testing.T) {
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){
		probabilities(map[string]float64{"A": 0.1, "B": 0.9}),
	}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cfg := decisionConfig(srv.URL, config.RerankerConfig{Model: "decide"})
	cfg.Memory.Reranker.Provider = ""
	cfg.Models = map[string]config.ModelRef{"decide": {Provider: "ollama-local", Model: "nimble", Kind: config.ModelKindDecision}}
	r, err := BuildRanker(cfg, nil)
	if err != nil {
		t.Fatalf("BuildRanker: %v", err)
	}
	if _, rep := r.Rank(context.Background(), "q", fiveTexts[:2], 0); !rep.Applied {
		t.Fatalf("report = %+v, want applied", rep)
	}
	if got := f.reqs[0].Model; got != "nimble" || r.ModelID() != "nimble" || r.ProviderID() != "ollama-local" {
		t.Errorf("asked for %q as %s/%s, want nimble on ollama-local", got, r.ProviderID(), r.ModelID())
	}
}

// TestDecision_ReranksAreCountedInTheProvidersDecisionLane — the decision kind
// has no cap of its own: its calls take the decision lane of the provider the
// block resolved to, so that lane's max_concurrent is what bounds them.
func TestDecision_ReranksAreCountedInTheProvidersDecisionLane(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	held := func(w http.ResponseWriter, r systemOneRequest) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		probabilities(map[string]float64{"A": 0.1, "B": 0.9})(w, r)
	}
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){held}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"ollama-local": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindDecision: {MaxConcurrent: 1}}},
	})
	d, err := BuildRanker(decisionConfig(srv.URL, config.RerankerConfig{}), sched)
	if err != nil {
		t.Fatalf("BuildRanker: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, rep := d.Rank(context.Background(), "q", fiveTexts, 0); !rep.Applied {
				t.Errorf("report = %+v, want applied", rep)
			}
		}()
	}
	wg.Wait()
	if f.calls() != 4 || peak != 1 {
		t.Errorf("%d calls, %d at once; want 4 calls, one at a time", f.calls(), peak)
	}
	if st := sched.Stats()["ollama-local"].Lanes[config.ModelKindDecision]; st.Grants != 4 || st.Active != 0 {
		t.Errorf("decision lane = %+v, want 4 grants and none in flight", st)
	}
}

// TestDecision_ARerankThatWaitsPastItsTimeoutKeepsSearchOrder — timeout_ms
// bounds a rerank, its wait for a lane slot included: a search does not block
// behind a full lane, it keeps its own order and says the rerank timed out.
func TestDecision_ARerankThatWaitsPastItsTimeoutKeepsSearchOrder(t *testing.T) {
	release := make(chan struct{})
	held := func(w http.ResponseWriter, r systemOneRequest) {
		<-release
		probabilities(map[string]float64{"A": 0.1, "B": 0.9})(w, r)
	}
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){held}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	defer close(release) // before srv.Close, which waits for the held handler
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"ollama-local": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindDecision: {MaxConcurrent: 1}}},
	})
	d, err := BuildRanker(decisionConfig(srv.URL, config.RerankerConfig{TimeoutMs: 2000}), sched)
	if err != nil {
		t.Fatalf("BuildRanker: %v", err)
	}
	go d.Rank(context.Background(), "holds the slot", fiveTexts[:2], 0)
	deadline := time.Now().Add(3 * time.Second)
	for f.calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first rerank never reached the endpoint")
		}
		time.Sleep(time.Millisecond)
	}
	quick, err := BuildRanker(decisionConfig(srv.URL, config.RerankerConfig{TimeoutMs: 60}), sched)
	if err != nil {
		t.Fatalf("BuildRanker: %v", err)
	}
	order, rep := quick.Rank(context.Background(), "waits", fiveTexts[:2], 0)
	if rep.Applied || rep.Reason != memory.RerankTimeout {
		t.Errorf("report = %+v, want a timeout", rep)
	}
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Errorf("order = %v, want search's own", order)
	}
	if n := f.calls(); n != 1 {
		t.Errorf("the endpoint saw %d calls, want 1: the waiting rerank must not reach it", n)
	}
}
