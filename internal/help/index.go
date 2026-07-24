package help

// Boot-reconciled, hybrid-searchable index of Context op=help topics
// (RFC BL P1). Each topic is split by `##` heading section — one searchable
// unit — and stored as rows in the existing Memory + memory_embeddings tables
// at a RESERVED, isolated global namespace so help never pollutes (nor is
// polluted by) user/agent/tenant memory search:
//
//	tenant_id = ""                       (the shared/legacy partition)
//	scope     = store.MemoryScopeGlobal
//	scope_id  = HelpNamespaceScopeID ("__help__")  — a sentinel no ordinary
//	          Memory op produces (the Memory tool doesn't even expose the
//	          `global` scope), so the reserved rows are invisible to every
//	          user-facing memory enumeration/erasure path (those key on real
//	          scope_ids: user_id, agent name, tenant id).
//
// Reuse — not a new table — because the reuse is clean: MemoryEmbedSet already
// carries the section text in embed_text, which drives BOTH the vector embedding
// and the generated full-text tsvector, so the PR2 hybrid legs (MemoryEmbedSearch
// ∥ MemoryFullTextSearch → FuseRRF) work over help sections with zero new schema.
//
// The index is read-only from an agent's perspective (only this reconcile writes
// it), shared across tenants, and exempt from provenance/erasure — it is embedded
// documentation, not user content.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// HelpNamespaceScopeID is the sentinel scope_id the help index lives under
	// (scope=global, tenant=""). Chosen so it can never collide with a real
	// Memory scope_id — the Memory tool doesn't expose the global scope at all,
	// so nothing an agent does ever writes or reads this bucket.
	HelpNamespaceScopeID = "__help__"

	// helpTenant is the tenant partition the shared help index lives in. The
	// index is deliberately cross-tenant (embedded docs, identical for everyone),
	// so it lives in the "" shared/legacy partition.
	helpTenant = ""

	// helpListLimit bounds the existing-row enumeration used for the prune pass.
	// The bundled corpus is tens of topics × a handful of sections each (low
	// hundreds of rows), so this is comfortably above any real corpus; a
	// truncation is logged so an unexpectedly huge corpus is visible.
	helpListLimit = 10000

	helpQueryDefaultTopK = 5
	helpQueryMaxTopK     = 20
)

// helpScope is the reserved scope the index rows use.
const helpScope = store.MemoryScopeGlobal

// Section is one searchable unit of a help topic — a single `##` heading
// section, or the pre-first-heading preamble (Heading == ""). Text is the full
// section body INCLUDING the heading line and any fenced content (e.g. a
// ```mermaid diagram's source, which is already text and stays searchable — a
// node label is a lexeme like any other; RFC BL §2.9). Key is the reserved-
// namespace memory key, assigned by AllSections so it is unique across the
// corpus even when two sections share a (topic, heading).
type Section struct {
	TopicSlug string
	Heading   string
	Text      string
	Snippet   string
	Key       string
}

// keyBase is the natural key for a section before duplicate-heading
// disambiguation: "<topic>#<heading>". Topic names are unique and headings are
// almost always unique within a topic, so this is normally already the Key.
func (s Section) keyBase() string { return s.TopicSlug + "#" + s.Heading }

// IndexStore is the minimal subset of store.Store the help index needs. Declared
// here (rather than taking the full store.Store) so the storage surface is
// explicit and tests can supply a tiny fake instead of stubbing the whole Store.
// store.Store satisfies it structurally, so callers pass their live store as-is.
type IndexStore interface {
	SupportsVectors() bool
	SupportsFullText() bool
	MemoryList(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, prefix string, limit int) ([]store.MemoryEntry, bool, error)
	MemorySet(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, value json.RawMessage, ttl time.Duration) error
	MemoryEmbedSet(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error
	MemoryDelete(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, key string) (bool, error)
	MemoryEmbedSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error)
	MemoryFullTextSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID, keyPrefix, queryText string, topK int) ([]store.MemorySearchEntry, error)
}

// indexValue is the JSON stored in each reserved-namespace memory row. Hash is
// the content-hash gate for reconcile (see reconcile); the other fields are what
// a query surfaces without a second lookup.
type indexValue struct {
	TopicSlug string `json:"topic_slug"`
	Heading   string `json:"heading"`
	Snippet   string `json:"snippet"`
	Hash      string `json:"hash"`
}

// ReconcileStats reports what a reconcile did, for the boot log.
type ReconcileStats struct {
	// Written is the number of new-or-changed sections re-embedded + rewritten.
	Written int
	// Pruned is the number of stale sections deleted (topic/heading gone).
	Pruned int
	// Unchanged is the number of sections whose content hash already matched.
	Unchanged int
	// Failed is the number of sections whose embed-set failed after the base
	// row was written; they keep the sentinel (empty) hash so the next reconcile
	// re-attempts them rather than leaving them permanently indexed without a
	// vector/full-text row.
	Failed int
	// Degraded is true when there is no embedder or no vector support, so the
	// index was left untouched and help search degrades to the in-memory scan.
	Degraded bool
}

// sectionsForTopic splits a topic's markdown into `##` sections. Content before
// the first `##` heading is the preamble (Heading == ""). A `## ` line inside a
// fenced code block (``` … ```) is NOT a heading — tracked so a mermaid/code
// fence containing `##` can't spuriously split a section.
func sectionsForTopic(t *Topic) []Section {
	if t == nil {
		return nil
	}
	lines := strings.Split(t.Content, "\n")
	var (
		secs       []Section
		curHeading string // "" == preamble
		curBody    []string
		inFence    bool
	)
	flush := func() {
		body := strings.Join(curBody, "\n")
		var full string
		if curHeading != "" {
			full = "## " + curHeading + "\n" + body
		} else {
			full = body
		}
		full = strings.TrimSpace(full)
		if full == "" {
			return // empty preamble, or empty section — nothing to index
		}
		secs = append(secs, Section{
			TopicSlug: t.Name,
			Heading:   curHeading,
			Text:      full,
			Snippet:   snippet(body),
		})
	}
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
			curBody = append(curBody, ln)
			continue
		}
		// A level-2 ATX heading at column 0, outside a code fence, starts a new
		// section. "### x" is level-3 and stays inside the current `##` section
		// (HasPrefix("### x", "## ") is false).
		if !inFence && strings.HasPrefix(ln, "## ") {
			flush()
			curHeading = strings.TrimSpace(strings.TrimPrefix(ln, "## "))
			curBody = nil
			continue
		}
		curBody = append(curBody, ln)
	}
	flush()
	return secs
}

// snippet collapses whitespace and trims a section body to a short preview for
// query results. Full content is fetched via op=help topic=<slug>.
func snippet(body string) string {
	const max = 200
	s := strings.Join(strings.Fields(body), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

// AllSections returns every section of every topic, with Key assigned and
// disambiguated so duplicate (topic, heading) pairs still get unique, stable
// keys ("<base>#2", "#3", …). Deterministic for a given corpus, so an unchanged
// corpus yields identical keys across boots — the basis for the zero-write gate.
func AllSections(set *Set) []Section {
	if set == nil {
		return nil
	}
	var all []Section
	seen := map[string]int{}
	for _, t := range set.All() { // All() is sorted by topic name
		for _, s := range sectionsForTopic(t) {
			base := s.keyBase()
			n := seen[base]
			seen[base]++
			if n == 0 {
				s.Key = base
			} else {
				s.Key = fmt.Sprintf("%s#%d", base, n+1)
			}
			all = append(all, s)
		}
	}
	return all
}

// contentHash is the reconcile gate. It folds the embedder identity in with the
// text so swapping the embedder (provider/model) re-embeds the corpus even when
// the text is byte-identical — otherwise stale vectors (or a dimension mismatch)
// would linger. For an unchanged corpus + unchanged embedder the hash is stable,
// so reconcile makes zero embed calls.
func contentHash(salt, text string) string {
	h := sha256.Sum256([]byte(salt + "\x00" + text))
	return hex.EncodeToString(h[:])
}

// ReconcileIndex brings the reserved help namespace in line with the current
// go:embed corpus: it re-embeds only new/changed sections (content-hash gated),
// leaves unchanged sections untouched (zero embed calls), and prunes rows whose
// topic/heading no longer exists. It is a NO-OP (Degraded=true) when there is no
// embedder or the store has no vector support — help search then degrades to the
// in-memory substring scan, so there is nothing to index.
//
// Idempotent and safe to call repeatedly; the boot wiring runs it once, advisory-
// lock-gated in a cluster so only one replica does the work.
func ReconcileIndex(ctx context.Context, set *Set, st IndexStore, emb providers.Embedder) (ReconcileStats, error) {
	var stats ReconcileStats
	if set == nil {
		return stats, nil
	}
	if st == nil || emb == nil || !st.SupportsVectors() {
		stats.Degraded = true
		lcotel.RecordHelpReconcile(ctx, stats.Written, stats.Pruned, stats.Unchanged, stats.Failed, stats.Degraded)
		return stats, nil
	}

	salt := emb.Provider() + "\n" + emb.Model()

	desired := AllSections(set)
	desiredByKey := make(map[string]struct{}, len(desired))
	for _, s := range desired {
		desiredByKey[s.Key] = struct{}{}
	}

	existing, truncated, err := st.MemoryList(ctx, helpTenant, helpScope, HelpNamespaceScopeID, "", helpListLimit)
	if err != nil {
		return stats, fmt.Errorf("help index: list existing: %w", err)
	}
	if truncated {
		log.Printf("help index: existing-row list truncated at %d; prune may be incomplete", helpListLimit)
	}
	existingHash := make(map[string]string, len(existing))
	for _, e := range existing {
		var v indexValue
		if json.Unmarshal(e.Value, &v) == nil {
			existingHash[e.Key] = v.Hash
		} else {
			existingHash[e.Key] = "" // unparseable → force a rewrite
		}
	}

	// Collect the sections that actually changed, then embed them in ONE batch
	// (the driver batches internally). An unchanged corpus collects nothing, so
	// no Embed call is made at all.
	type pending struct {
		sec  Section
		hash string
	}
	var changed []pending
	for _, s := range desired {
		h := contentHash(salt, s.Text)
		if prev, ok := existingHash[s.Key]; ok && prev == h {
			stats.Unchanged++
			continue
		}
		changed = append(changed, pending{sec: s, hash: h})
	}

	if len(changed) > 0 {
		texts := make([]string, len(changed))
		for i := range changed {
			texts[i] = changed[i].sec.Text
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			return stats, fmt.Errorf("help index: embed %d section(s): %w", len(texts), err)
		}
		if len(vecs) != len(texts) {
			return stats, fmt.Errorf("help index: embed returned %d vectors, want %d", len(vecs), len(texts))
		}
		now := time.Now().UTC()
		for i, p := range changed {
			// Two-phase write so a section is NEVER hash-marked "done" without its
			// embedding. The base row is the FK parent of the embedding, so it must
			// exist first — but the two writes are not transactional, so if we
			// stored the real content hash on the base row up front and the
			// embed-set then failed, the hash would match on the next pass and the
			// section would be marked Unchanged forever with no vector/full-text row
			// (a silent precision loss; substring still finds it, so no outage).
			// Instead: phase 1 writes the base row with an EMPTY sentinel hash
			// (contentHash never returns "", so it always re-triggers a rewrite);
			// phase 2 records the real hash only after the embed-set also succeeds.
			base := indexValue{
				TopicSlug: p.sec.TopicSlug,
				Heading:   p.sec.Heading,
				Snippet:   p.sec.Snippet,
				Hash:      "", // withheld until BOTH writes succeed
			}
			pendingVal, mErr := json.Marshal(base)
			if mErr != nil {
				return stats, fmt.Errorf("help index: marshal %q: %w", p.sec.Key, mErr)
			}
			if err := st.MemorySet(ctx, helpTenant, helpScope, HelpNamespaceScopeID, p.sec.Key, pendingVal, 0); err != nil {
				return stats, fmt.Errorf("help index: set %q: %w", p.sec.Key, err)
			}
			if err := st.MemoryEmbedSet(ctx, helpTenant, helpScope, HelpNamespaceScopeID, p.sec.Key, store.MemoryEmbedding{
				Provider:  emb.Provider(),
				Model:     emb.Model(),
				Dimension: len(vecs[i]),
				Vector:    vecs[i],
				EmbedText: p.sec.Text,
				CreatedAt: now,
			}); err != nil {
				// Embed-set can fail transiently while the base write succeeds.
				// The section keeps its "" sentinel hash, so it is re-attempted on
				// the next reconcile; count it failed and keep indexing the rest
				// rather than aborting the whole index.
				log.Printf("help index: embed-set %q failed, will retry next reconcile: %v", p.sec.Key, err)
				stats.Failed++
				continue
			}
			// Phase 2: both writes landed — record the real content hash so the
			// section is recognized as Unchanged (zero work) next reconcile.
			base.Hash = p.hash
			finalVal, mErr := json.Marshal(base)
			if mErr != nil {
				return stats, fmt.Errorf("help index: marshal %q: %w", p.sec.Key, mErr)
			}
			if err := st.MemorySet(ctx, helpTenant, helpScope, HelpNamespaceScopeID, p.sec.Key, finalVal, 0); err != nil {
				return stats, fmt.Errorf("help index: finalize %q: %w", p.sec.Key, err)
			}
			stats.Written++
		}
	}

	// Prune sections that no longer exist. On Postgres the embedding row cascades
	// via its ON DELETE CASCADE FK; SQLite has no embeddings table (and thus no
	// vector support, so this path never runs there).
	for key := range existingHash {
		if _, ok := desiredByKey[key]; ok {
			continue
		}
		if _, err := st.MemoryDelete(ctx, helpTenant, helpScope, HelpNamespaceScopeID, key); err != nil {
			return stats, fmt.Errorf("help index: prune %q: %w", key, err)
		}
		stats.Pruned++
	}

	// RFC BL PR6: emit the reconcile outcome once at the successful end so a
	// downstream connector derives written/pruned/unchanged/failed counters +
	// the degraded gauge. No-op-safe when OTEL is unconfigured.
	lcotel.RecordHelpReconcile(ctx, stats.Written, stats.Pruned, stats.Unchanged, stats.Failed, stats.Degraded)
	return stats, nil
}

// QueryResult is one hit from QueryIndex — enough to decide whether to fetch the
// full topic via op=help topic=<topic_slug>.
type QueryResult struct {
	TopicSlug string  `json:"topic_slug"`
	Heading   string  `json:"heading"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
}

// QueryResults wraps the hits with the mode that produced them so a caller can
// tell a full-strength hybrid result from a degraded substring scan.
type QueryResults struct {
	Mode    string        `json:"mode"` // "hybrid" | "substring"
	Results []QueryResult `json:"results"`
}

// QueryIndex runs an LLM-free search over help sections and returns the top-k
// matches. When embeddings are available (an embedder + a vector-capable store)
// it runs the PR2 hybrid path — vector ∥ full-text fused by RRF — over the
// reserved namespace. Otherwise, or when the index is not yet populated / returns
// nothing, it degrades to a substring/keyword scan over the in-memory Set (which
// always exists). Either way query always returns useful results — just less
// precisely without the index.
func QueryIndex(ctx context.Context, set *Set, st IndexStore, emb providers.Embedder, query string, topK int) (QueryResults, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return QueryResults{}, errors.New("help query: empty query")
	}
	if topK <= 0 {
		topK = helpQueryDefaultTopK
	}
	if topK > helpQueryMaxTopK {
		topK = helpQueryMaxTopK
	}

	if st != nil && emb != nil && st.SupportsVectors() {
		hits, deadDropped, err := hybridQuery(ctx, set, st, emb, query, topK)
		if err != nil {
			// A hybrid failure must NOT hard-fail the caller — query's contract
			// is "always returns useful results". Two real cases hit this exactly
			// when the fallback should kick in: a transient embedder API error on
			// the query-embed, and the dimension-mismatch window during an
			// embedder swap (the provider+model-salted content hash forces a
			// re-embed, but until the advisory-lock-gated boot reconcile catches
			// up, MemoryEmbedSearch returns ErrDimensionMismatch for every query
			// against the stale rows). Warn and fall through to the substring scan,
			// exactly as the empty-index case below does.
			log.Printf("help query: hybrid path failed, degrading to substring: %v", err)
		} else {
			// Record dead-link drops even when every hit was dead (so the signal
			// is emitted before falling through to the substring scan). No-op-safe.
			lcotel.RecordDeadlinkDroppedCtx(ctx, deadDropped)
			if len(hits) > 0 {
				return QueryResults{Mode: "hybrid", Results: hits}, nil
			}
		}
		// Hybrid error OR empty index (reconcile not done / no match / all hits
		// were dead links) → fall through to the substring scan over the live
		// in-memory Set, which can never surface a dead link.
	}
	return QueryResults{Mode: "substring", Results: substringQuery(set, query, topK)}, nil
}

// hybridQuery embeds the query and fuses the vector + full-text legs via RRF over
// the reserved help namespace. Mirrors the in-process Memory backend's hybrid
// retrieval so help search behaves like memory search. It also applies the RFC
// BL §2.10 read-time dead-link guard (see the filter loop below) and returns the
// number of hits it dropped so the caller can emit the dead-link signal.
func hybridQuery(ctx context.Context, set *Set, st IndexStore, emb providers.Embedder, query string, topK int) ([]QueryResult, int, error) {
	vecs, err := emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, 0, fmt.Errorf("help query: embed: %w", err)
	}
	if len(vecs) != 1 {
		return nil, 0, fmt.Errorf("help query: embed returned %d vectors, want 1", len(vecs))
	}
	fetch := topK * 4
	if fetch < topK {
		fetch = topK
	}
	if fetch > 51 {
		fetch = 51 // the store's defensive per-search cap
	}
	vres, err := st.MemoryEmbedSearch(ctx, helpTenant, helpScope, HelpNamespaceScopeID, "", vecs[0], fetch)
	if err != nil {
		return nil, 0, fmt.Errorf("help query: vector leg: %w", err)
	}
	// (nil, nil) when the store has no full-text index — the fusion then
	// collapses to pure-vector.
	fres, err := st.MemoryFullTextSearch(ctx, helpTenant, helpScope, HelpNamespaceScopeID, "", query, fetch)
	if err != nil {
		return nil, 0, fmt.Errorf("help query: full-text leg: %w", err)
	}
	fused := memory.FuseRRF(vres, fres, memory.RRFDefaultK)
	if len(fused) > topK {
		fused = fused[:topK]
	}
	out := make([]QueryResult, 0, len(fused))
	dead := 0
	for _, e := range fused {
		var v indexValue
		if json.Unmarshal(e.Value, &v) != nil {
			continue // not one of our rows / unparseable — skip defensively
		}
		// RFC BL §2.10 read-time dead-link guard: an index row can outlive its
		// topic in the window between a new-binary boot that dropped the topic
		// from the go:embed corpus and the backgrounded reconcile pruning the
		// stale row. Drop any hit whose topic is no longer in the live in-memory
		// Set so a query never points an agent at a topic it can't fetch. The
		// reconcile prune remains the authoritative cleanup; this is the read-
		// time floor. Counting is the bounded, non-blocking cleanup signal — no
		// store write on the read path.
		if _, ok := set.Get(v.TopicSlug); !ok {
			dead++
			log.Printf("help query: dropped dead-link hit topic=%q heading=%q (topic no longer in corpus)", v.TopicSlug, v.Heading)
			continue
		}
		out = append(out, QueryResult{
			TopicSlug: v.TopicSlug,
			Heading:   v.Heading,
			Snippet:   v.Snippet,
			// SemanticScore carries the fused RRF value after FuseRRF (Score
			// stays the raw cosine, which is meaningless for a full-text-only hit).
			Score: e.SemanticScore,
		})
	}
	return out, dead, nil
}

// substringQuery scans the in-memory Set for sections containing the query terms
// (case-insensitive), scored by total term-occurrence count. This is the always-
// available fallback (the Set always exists); it also naturally searches any
// mermaid/code source in a section since Text is never stripped.
func substringQuery(set *Set, query string, topK int) []QueryResult {
	if set == nil {
		return nil
	}
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		r QueryResult
		s int
	}
	var hits []scored
	for _, sec := range AllSections(set) {
		lower := strings.ToLower(sec.Text)
		score := 0
		for _, t := range terms {
			score += strings.Count(lower, t)
		}
		if score == 0 {
			continue
		}
		hits = append(hits, scored{
			r: QueryResult{TopicSlug: sec.TopicSlug, Heading: sec.Heading, Snippet: sec.Snippet, Score: float64(score)},
			s: score,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].s != hits[j].s {
			return hits[i].s > hits[j].s
		}
		if hits[i].r.TopicSlug != hits[j].r.TopicSlug {
			return hits[i].r.TopicSlug < hits[j].r.TopicSlug
		}
		return hits[i].r.Heading < hits[j].r.Heading
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]QueryResult, len(hits))
	for i := range hits {
		out[i] = hits[i].r
	}
	return out
}
