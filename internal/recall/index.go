// Package recall implements a run-scoped, embedding-keyed index of the
// conversation spans a run's context distillation evicts, plus the free-text
// lookup the Recall tool uses to fetch them back.
//
// The invariant it rests on: context distillation (compaction / recap / stateful)
// is lossy and one-way, but the ORIGINAL turns are already retained on the
// transcript and the embedding machinery already exists. So as the loop distils a
// span, this package embeds each evicted message and keeps the vector in memory
// for the rest of the run; a later Recall(query) embeds the query and returns the
// most similar original spans. Distillation therefore SHRINKS the fed context
// while GROWING a searchable index of what it dropped — a needle buried past a
// distillation boundary stays reachable by a free-text question.
//
// A run-scoped in-memory index (rather than the persistent vector store) is
// deliberate: the persistent store is per-scope and durable, so indexing every
// evicted turn there would pollute the agent's memory and add store writes on the
// distillation hot path. The durable half — curating facts worth keeping across
// runs — is the consolidator (compaction.memory_flush) and the Recall tool's
// silent fallback to the agent's memory; this index is the immediate, within-run
// half.
package recall

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// DefaultMaxEntries caps a run-scoped index. A long run evicts many spans; past
// the cap the oldest entries are dropped (FIFO) so memory stays bounded. The cap
// is generous — recall targets recently-evicted facts — and each entry is only a
// vector plus a bounded text snippet.
const DefaultMaxEntries = 4096

// DefaultTopK is the fetch size when a Recall query does not name one. The
// needle-recall probe found k=3 recovers a buried fact reliably without diluting
// the answer with near-duplicates.
const DefaultTopK = 3

// MaxTopK bounds a caller-supplied k.
const MaxTopK = 10

// maxEntryChars bounds the text embedded per evicted message, so one giant tool
// result cannot blow the embedder's token budget. The head of a span carries the
// identifying fact; the tail is usually boilerplate.
const maxEntryChars = 4000

// Index is a run-scoped, in-memory, embedding-keyed store of the spans a run's
// context distillation evicted. One Index per run, discarded when the run ends.
// Safe for concurrent Harvest/Search: the loop harvests at a distillation
// boundary while a tool call may be searching.
type Index struct {
	embedder   providers.Embedder
	maxEntries int

	mu      sync.Mutex
	entries []entry
}

type entry struct {
	vec  []float32
	text string
}

// Hit is one recall result: an original span and its cosine similarity to the
// query. Source names where it came from ("run" = this run's evicted spans,
// "memory" = the agent's durable memory) so a caller can tell them apart.
type Hit struct {
	Text   string
	Score  float64
	Source string
}

// NewIndex builds a run-scoped index. embedder may be nil — Harvest and Search
// then no-op — so a caller need not special-case a no-embedder deployment.
// maxEntries <= 0 uses DefaultMaxEntries.
func NewIndex(embedder providers.Embedder, maxEntries int) *Index {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Index{embedder: embedder, maxEntries: maxEntries}
}

// Harvest embeds each evicted message that carries text and appends it as its OWN
// entry — the probe found a per-message granularity beats a per-block one (a
// coarse block dilutes the discriminating fact). One batch Embed call covers the
// whole span.
//
// Never fatal: harvest exists to help a later recall, and an embed failure (local
// model down, a timeout) must not break a live run — the batch is dropped and the
// run continues. A nil embedder or empty span is a no-op.
func (ix *Index) Harvest(ctx context.Context, evicted []providers.Message) {
	if ix == nil || ix.embedder == nil || len(evicted) == 0 {
		return
	}
	texts := make([]string, 0, len(evicted))
	for _, m := range evicted {
		if t := renderMessage(m); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return
	}
	vecs, err := ix.embedder.Embed(ctx, texts)
	if err != nil || len(vecs) != len(texts) {
		return // swallowed — see the doc comment
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for i, v := range vecs {
		ix.entries = append(ix.entries, entry{vec: v, text: texts[i]})
	}
	// FIFO trim to the cap: keep the most recently harvested spans.
	if len(ix.entries) > ix.maxEntries {
		ix.entries = ix.entries[len(ix.entries)-ix.maxEntries:]
	}
}

// Search embeds the query and returns up to k entries by descending cosine
// similarity, tagged Source="run". Returns (nil, nil) when the index is empty,
// the embedder is unset, or the query is blank. k <= 0 uses DefaultTopK; k is
// clamped to MaxTopK.
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	if ix == nil || ix.embedder == nil {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if k <= 0 {
		k = DefaultTopK
	}
	if k > MaxTopK {
		k = MaxTopK
	}
	ix.mu.Lock()
	snapshot := make([]entry, len(ix.entries))
	copy(snapshot, ix.entries)
	ix.mu.Unlock()
	if len(snapshot) == 0 {
		return nil, nil
	}
	vecs, err := ix.embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 {
		return nil, err
	}
	q := vecs[0]
	hits := make([]Hit, 0, len(snapshot))
	for _, e := range snapshot {
		hits = append(hits, Hit{Text: e.text, Score: cosine(q, e.vec), Source: "run"})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

// Len reports the number of indexed spans (for tests / diagnostics).
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.entries)
}

// renderMessage flattens a message to the text worth embedding: its role, the
// text of each content block (tool results included — they carry the facts a
// distillation drops), any tool_use tool name (so an action is still findable),
// and the assistant reasoning trace when present. Returns "" when there is
// nothing textual to index. Capped at maxEntryChars so a giant tool result does
// not dominate the embedder's input.
func renderMessage(m providers.Message) string {
	var b strings.Builder
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	for _, c := range m.Content {
		switch c.Type {
		case "text", "tool_result":
			add(c.Text)
		case "tool_use":
			add(c.ToolName)
		}
	}
	add(m.Reasoning)
	body := strings.TrimSpace(b.String())
	if body == "" {
		return ""
	}
	if m.Role != "" {
		body = m.Role + ": " + body
	}
	if len(body) > maxEntryChars {
		body = body[:maxEntryChars]
	}
	return body
}

// cosine is the standard cosine similarity of two equal-length vectors, 0 for a
// length mismatch or a zero vector. Kept local so the package depends only on
// providers (the memory package has its own copy for the same reason).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		af, bf := float64(a[i]), float64(b[i])
		dot += af * bf
		na += af * af
		nb += bf * bf
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
