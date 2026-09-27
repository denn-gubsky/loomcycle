package inprocess

import (
	"context"
	"encoding/json"
	"log"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// rerankPool reorders the first N candidates of a ranked search pool with the
// operator's reranker, leaving everything below N where it was. It never fails:
// every fault keeps the pool's own order and says why in the report.
//
// WHAT THE MODEL READS IS EACH CANDIDATE'S INDEX TEXT — the text its vector was
// embedded from. For a document chunk that is "<document> — <section path>" and
// then the content, so every candidate arrives labelled with where it sits, which
// is how the rerank was measured; for a fact or a note it is the text that was
// indexed for it. A row with no stored index text falls back to its value.
func (b *Backend) rerankPool(ctx context.Context, tenant string, scope store.MemoryScope, scopeID string,
	q memory.SearchQuery, pool []store.MemorySearchEntry) ([]store.MemorySearchEntry, memory.RerankReport) {

	if b.reranker == nil {
		return pool, memory.RerankReport{Reason: memory.RerankNotConfigured}
	}
	if !q.CanReturnDocuments() {
		return pool, memory.RerankReport{Reason: memory.RerankNotDocumentSearch}
	}
	n := q.Rerank.EffectiveCandidates()
	if n > len(pool) {
		n = len(pool)
	}
	if n < 2 {
		return pool, memory.RerankReport{Reason: memory.RerankTooFewCandidates}
	}
	texts := make([]string, n)
	for i := 0; i < n; i++ {
		texts[i] = b.candidateText(ctx, tenant, scope, scopeID, pool[i])
	}
	order, rep := memory.RerankTexts(ctx, b.reranker, q.QueryText, texts, q.Rerank.EffectiveMaxChars())
	if !rep.Applied {
		log.Printf("memory.search: rerank kept search order (reason=%s, candidates=%d)", rep.Reason, rep.Candidates)
		return pool, rep
	}
	out := make([]store.MemorySearchEntry, 0, len(pool))
	for _, i := range order {
		out = append(out, pool[i])
	}
	return append(out, pool[n:]...), rep
}

// candidateText is what the reranker is shown for one candidate: the stored
// index text when there is one, else the row's value as text.
func (b *Backend) candidateText(ctx context.Context, tenant string, scope store.MemoryScope, scopeID string, e store.MemorySearchEntry) string {
	if emb, err := b.store.MemoryEmbedGet(ctx, tenant, scope, scopeID, e.Key); err == nil && emb.EmbedText != "" {
		return emb.EmbedText
	}
	return valueText(e.Value)
}

// valueText renders a stored value as text: a document chunk envelope's body, a
// JSON string's content, or the raw JSON otherwise.
func valueText(v json.RawMessage) string {
	var env struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(v, &env) == nil && env.Body != "" {
		return env.Body
	}
	return recallText(v)
}
