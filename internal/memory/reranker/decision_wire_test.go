package reranker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
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
	r, err := BuildRanker(cfg)
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

// TestDecision_BoundsReranksInFlight — memory.reranker.max_concurrent still
// bounds the decision kind's calls now that the driver holds the bound.
func TestDecision_BoundsReranksInFlight(t *testing.T) {
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
	d := newDecision(t, srv.URL, config.RerankerConfig{MaxConcurrent: 1})
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
}
