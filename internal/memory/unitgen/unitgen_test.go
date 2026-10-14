package unitgen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)

// scripted answers each call with the next reply in turn and records the requests.
type scripted struct {
	mu      sync.Mutex
	replies []string
	reqs    []providers.Request
}

func (s *scripted) ID() string                                   { return "stub" }
func (s *scripted) Capabilities() providers.Capabilities         { return providers.Capabilities{} }
func (s *scripted) Probe(context.Context) error                  { return nil }
func (s *scripted) ListModels(context.Context) ([]string, error) { return nil, nil }
func (s *scripted) KeyEnvName() string                           { return "" }
func (s *scripted) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	s.mu.Lock()
	reply := ""
	if len(s.replies) > 0 {
		reply, s.replies = s.replies[0], s.replies[1:]
	}
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: reply}
	ch <- providers.Event{Type: providers.EventDone}
	close(ch)
	return ch, nil
}

func req(kinds ...string) memory.UnitRequest {
	return memory.UnitRequest{DocumentTitle: "Leave policy", SectionPath: "Annual leave > Carry-over", Text: "Up to five days carry over.", Kinds: kinds}
}

// TestGenerate_WritesTheKindsAskedFor — a description is its own call; claims and
// questions are one JSON call, asked for with a schema, trimmed to six a list and
// de-duplicated; a kind not asked for is not returned.
func TestGenerate_WritesTheKindsAskedFor(t *testing.T) {
	p := &scripted{replies: []string{
		"States the carry-over limit.",
		`{"claims": ["Up to five days carry over.", "Up to five days carry over.", "c2", "c3", "c4", "c5", "c6", "c7"], "questions": ["How many days carry over?"]}`,
	}}
	g := New(p, config.UnitGeneratorConfig{Model: "m", Effort: "low"})
	got, err := g.Generate(context.Background(), req("description", "claims"))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, u := range got {
		kinds = append(kinds, u.Kind)
	}
	if strings.Join(kinds, ",") != "description,claim,claim,claim,claim,claim,claim" {
		t.Errorf("kinds = %v (one description, six distinct claims, no questions)", kinds)
	}
	if len(p.reqs) != 2 || p.reqs[0].OutputFormat != nil || p.reqs[1].OutputFormat == nil {
		t.Fatalf("calls = %d; the description is free text and the units call carries a schema", len(p.reqs))
	}
	r := p.reqs[1]
	if r.Effort != "low" || r.MaxTokens != 2000 || r.MaxContextTokens != 16384 || *r.Temperature != 0 {
		t.Errorf("units request = effort %q max %d ctx %d temp %v", r.Effort, r.MaxTokens, r.MaxContextTokens, *r.Temperature)
	}
	prompt := r.Messages[0].Content[0].Text
	if !strings.Contains(prompt, "Document: Leave policy") || !strings.Contains(prompt, "Section: Annual leave > Carry-over") {
		t.Errorf("prompt does not carry the document and section:\n%s", prompt)
	}
}

// TestGenerate_RetriesAnUnparseableAnswerAtATemperature — a degenerate answer at
// temperature 0 would repeat exactly, so the retry samples; JSON wrapped in prose
// or a fence still parses.
func TestGenerate_RetriesAnUnparseableAnswerAtATemperature(t *testing.T) {
	p := &scripted{replies: []string{"\\textstyle\\textstyle\\textstyle", "Sure! ```json\n{\"claims\": [\"a\"], \"questions\": []}\n```"}}
	got, err := New(p, config.UnitGeneratorConfig{Model: "m"}).Generate(context.Background(), req("claims", "questions"))
	if err != nil || len(got) != 1 || got[0].Text != "a" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	if len(p.reqs) != 2 || *p.reqs[0].Temperature != 0 || *p.reqs[1].Temperature == 0 {
		t.Errorf("the retry must sample: temperatures %v then %v", *p.reqs[0].Temperature, *p.reqs[1].Temperature)
	}
}

func TestGenerate_FailsWhenNoAnswerParses(t *testing.T) {
	p := &scripted{replies: []string{"no", "still no", "never"}}
	if _, err := New(p, config.UnitGeneratorConfig{Model: "m"}).Generate(context.Background(), req("questions")); err == nil {
		t.Error("three unparseable answers must fail the chunk, not store nothing silently")
	}
	if len(p.reqs) != attempts {
		t.Errorf("%d attempts, want %d", len(p.reqs), attempts)
	}
}

// TestBuild_ResolvesTheBlockLikeTheReranker — the shared service construction:
// an alias resolves, and an overridden endpoint answers only to the generator's
// own credential name.
func TestBuild_ResolvesTheBlockLikeTheReranker(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	g, err := Build(&config.Config{
		Providers: map[string]config.ProviderConfig{"openai": {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY"}},
		Models:    map[string]config.ModelRef{"writer": {Provider: "openai", Model: "gpt-5.4-mini"}},
		Memory:    config.MemoryConfig{UnitGenerator: config.UnitGeneratorConfig{Model: "writer", BaseURL: "http://gpu.internal:8000/v1"}},
	}, nil)
	if err != nil || g == nil {
		t.Fatalf("Build = %v, %v", g, err)
	}
	if g.ModelID() != "gpt-5.4-mini" {
		t.Errorf("model = %q", g.ModelID())
	}
	// The driver is inside its provider's chat lane, so it is reached as every
	// registry provider is: through providers.As.
	keyed, ok := providers.As[providers.KeyedProvider](g.provider)
	if !ok {
		t.Fatal("the openai driver no longer reports its credential name")
	}
	if k := keyed.KeyEnvName(); k != keyEnvName {
		t.Errorf("credential name = %q, want %q", k, keyEnvName)
	}
	if none, err := Build(&config.Config{}, nil); none != nil || err != nil {
		t.Errorf("no block: %v, %v", none, err)
	}
}

// withUsage answers like scripted but reports usage on its done event, as every
// real driver does.
type withUsage struct{ scripted }

func (w *withUsage) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	in, err := w.scripted.Call(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan providers.Event, 2)
	for ev := range in {
		if ev.Type == providers.EventDone {
			ev.Usage = &providers.Usage{InputTokens: 900, OutputTokens: 40}
		}
		out <- ev
	}
	close(out)
	return out, nil
}

// TestGenerate_ReportsEachCallsTimingToTheObserver — the generator's model may be
// used for nothing else, so its calls are the only samples of its speed there are.
func TestGenerate_ReportsEachCallsTimingToTheObserver(t *testing.T) {
	p := &withUsage{scripted{replies: []string{"States the carry-over limit."}}}
	g := New(p, config.UnitGeneratorConfig{Model: "m"})
	var seen []*providers.Usage
	g.ObserveCall = func(u *providers.Usage) { seen = append(seen, u) }
	if _, err := g.Generate(context.Background(), req("description")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Timing == nil {
		t.Fatalf("observed %d calls (%+v), want one timed call", len(seen), seen)
	}
	if seen[0].Provider != "stub" || seen[0].Model != "m" {
		t.Fatalf("call attributed to %q/%q, want stub/m", seen[0].Provider, seen[0].Model)
	}
}

// TestBuild_AGenerationCallIsCountedInItsProvidersChatLane — the generator
// builds a driver of its own, so Build puts that driver in the chat lane of the
// provider the block resolved to: with the lane at 1, four generation calls
// asked for at once reach the endpoint one at a time. With no scheduler they
// overlap.
func TestBuild_AGenerationCallIsCountedInItsProvidersChatLane(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	generateAll := func(sched *concurrency.CallScheduler) int {
		t.Helper()
		var mu sync.Mutex
		inFlight, peak, hits := 0, 0, 0
		// It refuses every request: what is measured is how many calls were AT
		// the endpoint together, not what came back.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			http.Error(w, "not today", http.StatusTeapot)
		}))
		defer srv.Close()
		g, err := Build(&config.Config{
			Providers: map[string]config.ProviderConfig{"openai": {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY"}},
			Models:    map[string]config.ModelRef{"writer": {Provider: "openai", Model: "gpt-5.4-mini"}},
			Memory:    config.MemoryConfig{UnitGenerator: config.UnitGeneratorConfig{Model: "writer", BaseURL: srv.URL + "/v1"}},
		}, sched)
		if err != nil || g == nil {
			t.Fatalf("Build = %v, %v", g, err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = g.call(context.Background(), "prompt", nil, 0)
			}()
		}
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		if hits < 4 {
			t.Fatalf("the endpoint saw %d requests, want at least 4", hits)
		}
		return peak
	}
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"openai": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: 1}}},
	})
	if peak := generateAll(sched); peak != 1 {
		t.Errorf("gated: %d generation calls at the endpoint at once, want 1 (providers.openai.max_concurrent)", peak)
	}
	if st := sched.Stats()["openai"].Lanes[config.ModelKindChat]; st.Grants < 4 || st.Active != 0 || st.Queued != 0 {
		t.Errorf("chat lane = %+v, want the 4 calls granted and nothing left in it", st)
	}
	// Control: the same four with no lane overlap, so the 1 above is the lane.
	if peak := generateAll(nil); peak < 2 {
		t.Errorf("ungated: peak %d, want the calls to overlap — the check above proves nothing", peak)
	}
}
