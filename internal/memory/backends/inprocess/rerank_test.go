package inprocess_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/memory/reranker"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryEmbedGet lets the double answer the point read the rerank makes for each
// candidate's index text — the real stores serve it from memory_embeddings.
func (v *vectorStore) MemoryEmbedGet(_ context.Context, _ string, scope store.MemoryScope, id, key string) (store.MemoryEmbedding, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.embeds[vsKey(scope, id, key)]
	if !ok {
		return store.MemoryEmbedding{}, &store.ErrNotFound{Kind: "embedding", ID: key}
	}
	return e, nil
}

type scriptedReranker struct {
	reply  string
	err    error
	calls  int
	prompt string
}

func (s *scriptedReranker) Complete(_ context.Context, prompt string) (string, error) {
	s.calls++
	s.prompt = prompt
	return s.reply, s.err
}

// rerankFixture stores five document chunks that all match the query "go", in a
// known similarity order: each adds a word the query does not have, so c1 ("go"
// alone) is the closest and c5 the furthest, and search alone returns c1..c5.
func rerankFixture(t *testing.T) (*inprocess.Backend, func()) {
	t.Helper()
	b, _, _, cleanup := vectorFixture(t)
	ctx := context.Background()
	texts := []string{
		"go",
		"go rust",
		"go rust python",
		"go rust python alice",
		"go rust python alice bob",
	}
	for i, txt := range texts {
		key := "doc.chunk:c" + string(rune('1'+i))
		if _, err := b.Set(ctx, store.MemoryScopeUser, "u1", key, json.RawMessage(`{"body":"body `+txt+`"}`),
			memory.SetOptions{Embed: true, EmbedText: "Guide — Section\n" + txt}); err != nil {
			t.Fatal(err)
		}
	}
	return b, cleanup
}

func keys(res memory.SearchResult) string {
	var ks []string
	for _, e := range res.Entries {
		ks = append(ks, strings.TrimPrefix(e.Key, "doc.chunk:"))
	}
	return strings.Join(ks, ",")
}

func search(t *testing.T, b *inprocess.Backend, q memory.SearchQuery) memory.SearchResult {
	t.Helper()
	res, err := b.Search(context.Background(), store.MemoryScopeUser, "u1", q, memory.DefaultRankConfig(), memory.DedupConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

var rerankOn = memory.RerankOptions{Enabled: true}

// TestInProcessRerank_PromotesFromBelowTopK — the point of reranking BEFORE the
// trim: with top_k 2, the model's pick from rank 5 is returned first.
func TestInProcessRerank_PromotesFromBelowTopK(t *testing.T) {
	b, cleanup := rerankFixture(t)
	defer cleanup()
	if got := keys(search(t, b, memory.SearchQuery{QueryText: "go", TopK: 2})); got != "c1,c2" {
		t.Fatalf("precondition: search order %s, want c1,c2", got)
	}
	r := &scriptedReranker{reply: "[5, 3]"}
	b.SetReranker(r)

	res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 2, Rerank: rerankOn})
	if got := keys(res); got != "c5,c3" {
		t.Errorf("reranked top 2 = %s, want c5,c3 (promoted from below top_k)", got)
	}
	if res.Rerank == nil || !res.Rerank.Applied || res.Rerank.Candidates != 5 {
		t.Errorf("report = %+v, want applied over 5 candidates", res.Rerank)
	}
	if len(res.RankScores) != len(res.Entries) {
		t.Errorf("rank scores not index-aligned after a rerank: %d vs %d", len(res.RankScores), len(res.Entries))
	}
}

// TestInProcessRerank_TheModelReadsEachCandidatesIndexText — the header the index
// was built from is what labels each candidate with its document and section.
func TestInProcessRerank_TheModelReadsEachCandidatesIndexText(t *testing.T) {
	b, cleanup := rerankFixture(t)
	defer cleanup()
	r := &scriptedReranker{reply: "[1]"}
	b.SetReranker(r)
	search(t, b, memory.SearchQuery{QueryText: "go", TopK: 5, Rerank: memory.RerankOptions{Enabled: true, MaxChars: 18}})
	if !strings.Contains(r.prompt, "Question: go") || !strings.Contains(r.prompt, "[1] Guide — Section\ngo") {
		t.Errorf("prompt does not carry the index text:\n%s", r.prompt)
	}
	// MaxChars truncates each candidate: 18 runes keeps the header and cuts the rest.
	if strings.Contains(r.prompt, "Guide — Section\ngo rust") {
		t.Errorf("max_chars did not truncate the candidates:\n%s", r.prompt)
	}
}

// TestInProcessRerank_CandidatesBoundsWhatTheModelSees — only the first N reach
// the model; the rest keep their places below the reranked ones.
func TestInProcessRerank_CandidatesBoundsWhatTheModelSees(t *testing.T) {
	b, cleanup := rerankFixture(t)
	defer cleanup()
	r := &scriptedReranker{reply: "[3, 2, 1]"}
	b.SetReranker(r)
	res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 5, Rerank: memory.RerankOptions{Enabled: true, Candidates: 3}})
	if got := keys(res); got != "c3,c2,c1,c4,c5" {
		t.Errorf("order = %s, want c3,c2,c1 then c4,c5 untouched", got)
	}
	if strings.Contains(r.prompt, "[4]") {
		t.Error("a candidate past `candidates` reached the model")
	}
}

// TestInProcessRerank_EveryFailureKeepsSearchOrderAndSaysWhy — the backend half of
// fail-open: an unconfigured server, a failing model and an unusable reply each
// return search's own order, with the reason on the result.
func TestInProcessRerank_EveryFailureKeepsSearchOrderAndSaysWhy(t *testing.T) {
	for _, c := range []struct {
		name     string
		reranker memory.RerankModel
		reason   string
	}{
		{"not configured", nil, memory.RerankNotConfigured},
		{"call fails", &scriptedReranker{err: errors.New("refused")}, memory.RerankCallFailed},
		{"no ranking", &scriptedReranker{reply: "I think c5."}, memory.RerankUnparseable},
	} {
		b, cleanup := rerankFixture(t)
		if c.reranker != nil {
			b.SetReranker(c.reranker)
		}
		res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 3, Rerank: rerankOn})
		if got := keys(res); got != "c1,c2,c3" {
			t.Errorf("%s: order = %s, want search's own c1,c2,c3", c.name, got)
		}
		if res.Rerank == nil || res.Rerank.Applied || res.Rerank.Reason != c.reason {
			t.Errorf("%s: report = %+v, want reason %q", c.name, res.Rerank, c.reason)
		}
		cleanup()
	}
}

// TestInProcessRerank_OnlyWhereItWasAskedAndCanHelp — no request, no call and no
// report; a search restricted away from documents is not reranked either.
func TestInProcessRerank_OnlyWhereItWasAskedAndCanHelp(t *testing.T) {
	b, cleanup := rerankFixture(t)
	defer cleanup()
	r := &scriptedReranker{reply: "[5]"}
	b.SetReranker(r)

	if res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 3}); res.Rerank != nil || r.calls != 0 {
		t.Errorf("unrequested: report %+v, %d calls", res.Rerank, r.calls)
	}
	res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 3, Rerank: rerankOn,
		Sources: []memory.Source{memory.SourceFacts, memory.SourceNotes}})
	if res.Rerank == nil || res.Rerank.Reason != memory.RerankNotDocumentSearch || r.calls != 0 {
		t.Errorf("facts+notes search: report %+v, %d calls", res.Rerank, r.calls)
	}
}

// TestInProcessRerank_RecallCarriesItWhenDocumentsAreAsked — recall reaches the
// rerank when it asks for documents, and reports it; its default (the agent's own
// facts and notes) is not a document search.
func TestInProcessRerank_RecallCarriesItWhenDocumentsAreAsked(t *testing.T) {
	b, cleanup := rerankFixture(t)
	defer cleanup()
	r := &scriptedReranker{reply: "[4]"}
	b.SetReranker(r)

	res, err := b.Recall(context.Background(), store.MemoryScopeUser, "u1", memory.RecallQuery{
		Query: "go", TopK: 2, Sources: []memory.Source{memory.SourceDocuments}, Rerank: rerankOn})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Facts) == 0 || res.Facts[0].ID != "doc.chunk:c4" {
		t.Errorf("recall[0] = %+v, want the reranked c4", res.Facts)
	}
	if res.Rerank == nil || !res.Rerank.Applied {
		t.Errorf("recall report = %+v", res.Rerank)
	}

	res, err = b.Recall(context.Background(), store.MemoryScopeUser, "u1", memory.RecallQuery{Query: "go", TopK: 2, Rerank: rerankOn})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rerank == nil || res.Rerank.Reason != memory.RerankNotDocumentSearch {
		t.Errorf("default recall report = %+v, want %q", res.Rerank, memory.RerankNotDocumentSearch)
	}
}

// TestInProcessRerank_ASkippedRerankLeavesTheSearchAsItWas — requested but not
// served (no reranker, or a search that cannot return documents), the rerank is
// reported and the search is otherwise the one it would have been: same order,
// same rank_score scale. Forcing the deep fused pool here changed rank_score from
// cosine to RRF values on a store without full-text, for nothing.
func TestInProcessRerank_ASkippedRerankLeavesTheSearchAsItWas(t *testing.T) {
	b, cleanup := rerankFixture(t)
	defer cleanup()
	plain := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 3})

	skipped := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 3, Rerank: rerankOn}) // no reranker set
	if skipped.Rerank == nil || skipped.Rerank.Reason != memory.RerankNotConfigured {
		t.Fatalf("report = %+v", skipped.Rerank)
	}
	if keys(skipped) != keys(plain) {
		t.Errorf("order %s, want %s", keys(skipped), keys(plain))
	}
	for i := range plain.RankScores {
		if skipped.RankScores[i] != plain.RankScores[i] {
			t.Errorf("rank_score[%d] = %v, want %v — a skipped rerank changed the scoring", i, skipped.RankScores[i], plain.RankScores[i])
		}
	}
}

// TestInProcessRerank_ThePoolHoldsCandidatesWhateverTopK — at top_k 2 the
// over-fetch alone is 8 rows; the model must still be shown `candidates` (20).
func TestInProcessRerank_ThePoolHoldsCandidatesWhateverTopK(t *testing.T) {
	b, _, _, cleanup := vectorFixture(t)
	defer cleanup()
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("doc.chunk:c%02d", i)
		if _, err := b.Set(ctx, store.MemoryScopeUser, "u1", key, json.RawMessage(`{"body":"go"}`),
			memory.SetOptions{Embed: true, EmbedText: "Guide — Section\ngo"}); err != nil {
			t.Fatal(err)
		}
	}
	r := &scriptedReranker{reply: "[20]"}
	b.SetReranker(r)
	res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 2, Rerank: rerankOn})
	if res.Rerank == nil || res.Rerank.Candidates != 20 {
		t.Fatalf("the model was shown %+v candidates, want 20 at top_k 2", res.Rerank)
	}
	if !strings.Contains(r.prompt, "[20] ") || strings.Contains(r.prompt, "[21] ") {
		t.Error("the prompt does not hold exactly 20 candidates")
	}
}

// orderRanker is a Ranker that answers with a fixed order and never sees a prompt:
// the shape of a reranker kind that does not reply in text.
type orderRanker struct {
	order    []int
	report   memory.RerankReport
	texts    []string
	maxChars int
}

func (o *orderRanker) Rank(_ context.Context, _ string, texts []string, maxChars int) ([]int, memory.RerankReport) {
	o.texts, o.maxChars = texts, maxChars
	return o.order, o.report
}

// TestInProcessRerank_AnyRankerOrdersTheSearch — the search holds a Ranker, not a
// text model: a ranker that answers with an order is applied as the listwise one
// is, sees each candidate's index text and the agent's character budget, and a
// report it marks not applied keeps search's own order.
func TestInProcessRerank_AnyRankerOrdersTheSearch(t *testing.T) {
	b, cleanup := rerankFixture(t)
	defer cleanup()
	r := &orderRanker{order: []int{4, 2, 0, 1, 3}, report: memory.RerankReport{Applied: true, Candidates: 5}}
	b.SetRanker(r)
	res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 5, Rerank: memory.RerankOptions{Enabled: true, MaxChars: 99}})
	if got := keys(res); got != "c5,c3,c1,c2,c4" {
		t.Errorf("order = %s, want the ranker's c5,c3,c1,c2,c4", got)
	}
	if res.Rerank == nil || !res.Rerank.Applied {
		t.Errorf("report = %+v, want applied", res.Rerank)
	}
	if len(r.texts) != 5 || r.texts[0] != "Guide — Section\ngo" || r.maxChars != 99 {
		t.Errorf("ranker saw texts %q with max_chars %d, want the five index texts and 99", r.texts, r.maxChars)
	}

	b.SetRanker(&orderRanker{order: []int{4, 3, 2, 1, 0}, report: memory.RerankReport{Reason: memory.RerankTimeout, Candidates: 5}})
	res = search(t, b, memory.SearchQuery{QueryText: "go", TopK: 5, Rerank: rerankOn})
	if got := keys(res); got != "c1,c2,c3,c4,c5" {
		t.Errorf("order = %s, want search's own c1..c5 when the ranker reports not applied", got)
	}
	if res.Rerank == nil || res.Rerank.Applied || res.Rerank.Reason != memory.RerankTimeout {
		t.Errorf("report = %+v, want not applied with reason timeout", res.Rerank)
	}
}

// TestListwiseRanker_NilModelIsNoRanker — a server without a reranker must never
// hold a Ranker that would call one (a typed-nil would be called, not skipped).
func TestListwiseRanker_NilModelIsNoRanker(t *testing.T) {
	if r := memory.ListwiseRanker(nil); r != nil {
		t.Fatalf("ListwiseRanker(nil) = %#v, want nil", r)
	}
}

// TestInProcessRerank_TheDecisionKindReordersASearch — the crossing: a decision
// reranker built from config (kind: decision on an Ollama provider) reorders a real
// search by the probabilities its endpoint returns, shown each candidate's index
// text as an option.
func TestInProcessRerank_TheDecisionKindReordersASearch(t *testing.T) {
	var seen map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]struct {
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = req.Questions["best"].Criteria
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{"best": map[string]any{"probabilities": map[string]float64{
				"A": 0.01, "B": 0.02, "C": 0.90, "D": 0.03, "E": 0.04}}},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
		})
	}))
	defer srv.Close()
	rr, err := reranker.BuildRanker(&config.Config{
		Providers: map[string]config.ProviderConfig{"ollama-local": {Driver: "ollama", BaseURL: srv.URL}},
		Memory: config.MemoryConfig{Reranker: config.RerankerConfig{
			Kind: config.RerankerKindDecision, Provider: "ollama-local", Model: "nimble"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, cleanup := rerankFixture(t)
	defer cleanup()
	b.SetRanker(rr)
	res := search(t, b, memory.SearchQuery{QueryText: "go", TopK: 5, Rerank: rerankOn})
	if got := keys(res); got != "c3,c5,c4,c2,c1" {
		t.Errorf("order = %s, want c3 (0.90) then c5, c4, c2, c1 by probability", got)
	}
	if res.Rerank == nil || !res.Rerank.Applied {
		t.Errorf("report = %+v, want applied", res.Rerank)
	}
	if seen["A"] != "Guide — Section\ngo" {
		t.Errorf("option A = %q, want the first candidate's index text", seen["A"])
	}
}

// TestInProcessRerank_RecallGetsTheDeepPoolUnderMemorySources — with facts and notes
// in the operator's sources, a recall of the agent's own notes is reranked over the
// deep pool, so the model's pick from below top_k is returned first; under the
// default sources the same recall keeps its order with not_a_document_search.
func TestInProcessRerank_RecallGetsTheDeepPoolUnderMemorySources(t *testing.T) {
	b, _, _, cleanup := vectorFixture(t)
	defer cleanup()
	ctx := context.Background()
	for i, txt := range []string{"go", "go rust", "go rust python", "go rust python alice", "go rust python alice bob"} {
		if _, err := b.Set(ctx, store.MemoryScopeUser, "u1", "n"+string(rune('1'+i)), json.RawMessage(`"`+txt+`"`),
			memory.SetOptions{Embed: true, EmbedText: txt}); err != nil {
			t.Fatal(err)
		}
	}
	recall := func() memory.RecallResult {
		res, err := b.Recall(ctx, store.MemoryScopeUser, "u1", memory.RecallQuery{Query: "go", TopK: 2, Rerank: rerankOn})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	ids := func(res memory.RecallResult) string {
		var s []string
		for _, f := range res.Facts {
			s = append(s, f.ID)
		}
		return strings.Join(s, ",")
	}

	r := &scriptedReranker{reply: "[5, 4]"}
	b.SetReranker(r)
	res := recall()
	if ids(res) != "n1,n2" || res.Rerank == nil || res.Rerank.Applied || res.Rerank.Reason != memory.RerankNotDocumentSearch || r.calls != 0 {
		t.Fatalf("default sources: %s %+v after %d calls, want n1,n2 with not_a_document_search and no call", ids(res), res.Rerank, r.calls)
	}

	b.SetRerankSources([]memory.Source{memory.SourceFacts, memory.SourceNotes})
	res = recall()
	if ids(res) != "n5,n4" || res.Rerank == nil || !res.Rerank.Applied || res.Rerank.Candidates != 5 {
		t.Errorf("memory sources: %s %+v, want n5,n4 promoted from below top_k over 5 candidates", ids(res), res.Rerank)
	}
}
