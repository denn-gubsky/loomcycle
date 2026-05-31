package memory

import (
	"encoding/json"
	"math"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DedupConfig is the per-search dedup block (RFC I MR-5 / Decision 2).
// After the hybrid ranker has ordered the candidate pool, near-duplicate
// entries are collapsed so the agent doesn't burn context on three
// phrasings of the same fact. Duplication is measured by the cosine
// similarity between two entries' stored embedding vectors.
//
// THRESHOLD INTERPRETATION (read this — the RFC wording is internally
// inconsistent). RFC Decision 2 says "drop entries with cosine DISTANCE <
// dedup.threshold ... Default threshold 0.92". Cosine distance is
// 1 − cosine_similarity, so a near-identical pair has distance ~0 and a
// merely-similar pair ~0.08. "distance < 0.92" would therefore drop almost
// every non-orthogonal entry — it cannot be the intent, and a 0.92 default
// only makes sense as a SIMILARITY floor (near-duplicates are HIGHLY
// similar). We implement the only self-consistent reading: two entries are
// duplicates when their cosine SIMILARITY >= Threshold (default 0.92).
// If a future RFC revision genuinely wants distance semantics, flip the
// comparison here and update DefaultDedupThreshold — the wire field name
// ("threshold") stays stable either way.
type DedupConfig struct {
	// Enabled turns dedup on. The zero value is disabled, so a search with
	// no `dedup` block behaves exactly as before (zero regression).
	Enabled bool `json:"enabled"`
	// Threshold is the cosine-SIMILARITY floor at/above which two entries
	// are considered duplicates. <= 0 falls back to DefaultDedupThreshold.
	// 1.0 means "only exact-direction duplicates collapse."
	Threshold float64 `json:"threshold"`
	// Mode selects what happens to a detected duplicate:
	//   "drop" (default) — skip it; only the highest-ranked of a cluster survives.
	//   "merge"          — drop it, but record its key+value under the
	//                      retained entry's value as merge provenance.
	//   "keep"           — retain it (no drop) but still count it as flagged,
	//                      so an operator can measure the duplication rate
	//                      without losing data.
	Mode string `json:"mode"`
}

// DefaultDedupThreshold is the cosine-similarity floor used when a dedup
// block is enabled but sets no (or a non-positive) threshold. Per RFC
// Decision 2's stated default, reinterpreted as a similarity floor.
const DefaultDedupThreshold = 0.92

const (
	dedupModeDrop  = "drop"
	dedupModeMerge = "merge"
	dedupModeKeep  = "keep"
)

// DedupResults collapses near-duplicate entries from an ALREADY-RANKED
// slice (RFC I MR-5 / Decision 2). It must run AFTER ranking and BEFORE
// the trim-to-TopK, so the highest-ranked member of a duplicate cluster is
// the one that survives. The input order is treated as the authority: the
// first time a vector is retained, every later entry within Threshold
// cosine-similarity of it is a duplicate.
//
// Returns the kept slice (order-preserved) and the count of entries that
// were dropped or flagged as duplicates. When cfg.Enabled is false the
// input is returned unchanged with dropped=0 — the zero-regression path.
//
// An entry with an empty Vector cannot be compared (the backend couldn't
// supply it — e.g. the Mem9 REST backend). Such an entry is NEVER treated
// as a duplicate: it is always kept, and it is never used as a comparison
// anchor for later entries. Dedup thus degrades to a no-op when vectors
// are unavailable, exactly as documented on store.MemorySearchEntry.Vector.
func DedupResults(ranked []store.MemorySearchEntry, cfg DedupConfig) (kept []store.MemorySearchEntry, dropped int) {
	if !cfg.Enabled || len(ranked) < 2 {
		return ranked, 0
	}
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = DefaultDedupThreshold
	}
	mode := cfg.Mode
	if mode == "" {
		mode = dedupModeDrop
	}

	kept = make([]store.MemorySearchEntry, 0, len(ranked))
	// anchors are the NON-duplicate survivors — the cluster representatives a
	// later candidate is tested against. This is tracked separately from
	// `kept` (the output) so that keep-mode, which retains flagged duplicates
	// in the output, does NOT let those flagged rows become anchors. If a
	// flagged duplicate could anchor, keep-mode would cascade-flag rows that
	// drop-mode keeps, and its `dropped` count would over-report what
	// drop-mode actually collapses — defeating the documented purpose of
	// keep-mode (measure the duplication rate without losing data). Indices
	// into anchors point at the merge target in `kept` for merge-mode.
	type anchorRef struct {
		entry   store.MemorySearchEntry
		keptIdx int // index in `kept` of this anchor (for merge provenance)
	}
	var anchors []anchorRef
	for i := range ranked {
		cand := ranked[i]
		dupOf := -1
		if len(cand.Vector) > 0 {
			// Compare only against retained cluster representatives (anchors),
			// highest-ranked first. The first match wins.
			for j := range anchors {
				if len(anchors[j].entry.Vector) == 0 {
					continue
				}
				if cosineSimilarity(cand.Vector, anchors[j].entry.Vector) >= threshold {
					dupOf = j
					break
				}
			}
		}
		if dupOf == -1 {
			// A fresh cluster representative: it's both kept AND an anchor.
			kept = append(kept, cand)
			anchors = append(anchors, anchorRef{entry: cand, keptIdx: len(kept) - 1})
			continue
		}
		// cand is a duplicate of the anchor at anchors[dupOf]. It is NEVER
		// added to anchors, so it can't seed further flags.
		dropped++
		switch mode {
		case dedupModeKeep:
			// Flagged but retained in the output (count it, keep the row).
			kept = append(kept, cand)
		case dedupModeMerge:
			ki := anchors[dupOf].keptIdx
			kept[ki].Value = mergeProvenance(kept[ki].Value, cand)
		default: // dedupModeDrop
			// skip cand entirely.
		}
	}
	return kept, dropped
}

// mergeProvenance records a dropped duplicate's key+value under the
// retained entry's value, so "merge" mode preserves the dropped text
// instead of discarding it. The shape is deliberately simple and stable:
// the retained value is wrapped (once) as
//
//	{"value": <original retained value>, "merged_from": [{"key":..,"value":..}, ...]}
//
// On a second merge into the same retained entry we append to the existing
// merged_from array rather than re-wrapping. We only treat retained as an
// existing envelope when merged_from is present, so an agent value that
// merely has a "value" field isn't misread. If marshalling the envelope
// ever fails (it won't — both sides are valid JSON), we leave the retained
// value unchanged so a malformed merge can never corrupt the surviving row.
func mergeProvenance(retained json.RawMessage, dup store.MemorySearchEntry) json.RawMessage {
	type provEntry struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	type envelope struct {
		Value      json.RawMessage `json:"value"`
		MergedFrom []provEntry     `json:"merged_from"`
	}

	newProv := provEntry{Key: dup.Key, Value: dup.Value}

	var existing envelope
	if err := json.Unmarshal(retained, &existing); err == nil && existing.MergedFrom != nil {
		existing.MergedFrom = append(existing.MergedFrom, newProv)
		if b, err := json.Marshal(existing); err == nil {
			return b
		}
		return retained
	}

	env := envelope{Value: retained, MergedFrom: []provEntry{newProv}}
	if b, err := json.Marshal(env); err == nil {
		return b
	}
	return retained
}

// cosineSimilarity returns the cosine of the angle between a and b in
// [-1, 1]; for embedding vectors in practice [0, 1]. Zero-norm vectors and
// mismatched dimensions return 0 (treated as orthogonal / not-similar) so a
// degenerate vector can never produce a NaN that would corrupt the >=
// comparison. This is the memory package's own copy of the cosine math:
// the dedup logic must not depend on a store backend's unexported helper.
func cosineSimilarity(a, b []float32) float64 {
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
