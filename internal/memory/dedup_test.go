package memory

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// vecEntry builds a search entry with a key and a raw vector. Score and
// CreatedAt are irrelevant to dedup (it operates on the already-ranked
// order + vectors), so they're left zero.
func vecEntry(key string, vec []float32) store.MemorySearchEntry {
	e := store.MemorySearchEntry{}
	e.Key = key
	e.Value = json.RawMessage(`"` + key + `"`)
	e.Vector = vec
	return e
}

func TestDedupResults_NearDuplicateDropped(t *testing.T) {
	// Two nearly-identical vectors (cosine sim ~1.0) and one orthogonal.
	a := vecEntry("a", []float32{1, 0, 0})
	aDup := vecEntry("a2", []float32{0.999, 0.001, 0}) // ~identical direction
	c := vecEntry("c", []float32{0, 1, 0})             // orthogonal to a

	cfg := DedupConfig{Enabled: true} // default threshold 0.92, mode drop
	kept, dropped := DedupResults([]store.MemorySearchEntry{a, aDup, c}, cfg)

	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d entries, want 2", len(kept))
	}
	// The HIGHER-ranked member of the cluster (a, first in order) survives;
	// the orthogonal entry survives too.
	if kept[0].Key != "a" || kept[1].Key != "c" {
		t.Fatalf("kept keys = [%s %s], want [a c]", kept[0].Key, kept[1].Key)
	}
}

func TestDedupResults_DistinctEntriesKept(t *testing.T) {
	// Three mutually orthogonal vectors — none are duplicates.
	entries := []store.MemorySearchEntry{
		vecEntry("a", []float32{1, 0, 0}),
		vecEntry("b", []float32{0, 1, 0}),
		vecEntry("c", []float32{0, 0, 1}),
	}
	kept, dropped := DedupResults(entries, DedupConfig{Enabled: true})
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d, want 3", len(kept))
	}
}

func TestDedupResults_EmptyVectorAlwaysKept(t *testing.T) {
	// An entry with no vector can't be compared → never a duplicate, even
	// when its neighbours are near-identical. It must always be kept and
	// must not anchor a drop.
	a := vecEntry("a", []float32{1, 0, 0})
	noVec := vecEntry("novec", nil)
	aDup := vecEntry("a2", []float32{1, 0, 0})

	kept, dropped := DedupResults([]store.MemorySearchEntry{a, noVec, aDup}, DedupConfig{Enabled: true})
	if dropped != 1 { // only aDup collapses into a
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d, want 2 (a, novec)", len(kept))
	}
	if kept[0].Key != "a" || kept[1].Key != "novec" {
		t.Fatalf("kept = [%s %s], want [a novec]", kept[0].Key, kept[1].Key)
	}
}

func TestDedupResults_DisabledIsNoOp(t *testing.T) {
	a := vecEntry("a", []float32{1, 0, 0})
	aDup := vecEntry("a2", []float32{1, 0, 0})
	in := []store.MemorySearchEntry{a, aDup}
	kept, dropped := DedupResults(in, DedupConfig{Enabled: false})
	if dropped != 0 || len(kept) != 2 {
		t.Fatalf("disabled dedup mutated result: dropped=%d kept=%d", dropped, len(kept))
	}
}

func TestDedupResults_KeepModeFlagsButRetains(t *testing.T) {
	a := vecEntry("a", []float32{1, 0, 0})
	aDup := vecEntry("a2", []float32{1, 0, 0})
	kept, dropped := DedupResults([]store.MemorySearchEntry{a, aDup}, DedupConfig{Enabled: true, Mode: dedupModeKeep})
	if dropped != 1 {
		t.Fatalf("dropped(flagged) = %d, want 1", dropped)
	}
	if len(kept) != 2 {
		t.Fatalf("keep mode dropped a row: kept=%d, want 2", len(kept))
	}
}

// Regression: keep-mode must report the SAME dropped count as drop-mode for
// the same input. A flagged duplicate must NOT become a comparison anchor —
// otherwise a chain where A~B and B~C but A≁C would cascade-flag C in
// keep-mode (C compared against the retained-but-flagged B) while drop-mode
// keeps C (C compared only against the surviving anchor A). keep-mode is
// documented as a way to MEASURE the drop rate without losing data, so the
// two counts must agree.
func TestDedupResults_KeepModeCountMatchesDropMode_NoAnchorCascade(t *testing.T) {
	// Unit vectors at angles chosen so cos(A,B)=cos(B,C)≈0.93 ≥ 0.92 but
	// cos(A,C)≈0.74 < 0.92. B is a dup of A; C is a dup of B's-direction but
	// NOT of A — so only B should ever be flagged.
	const d2r = math.Pi / 180
	at := func(deg float64) []float32 {
		return []float32{float32(math.Cos(deg * d2r)), float32(math.Sin(deg * d2r))}
	}
	a := vecEntry("a", at(0))  // 0°
	b := vecEntry("b", at(21)) // 21° from A: cos≈0.934 ≥ 0.92 → dup of A
	c := vecEntry("c", at(42)) // 42° from A: cos≈0.743 < 0.92 → NOT a dup of A

	in := []store.MemorySearchEntry{a, b, c}

	_, dropDropped := DedupResults(in, DedupConfig{Enabled: true, Mode: dedupModeDrop})
	keptKeep, keepDropped := DedupResults(in, DedupConfig{Enabled: true, Mode: dedupModeKeep})

	if dropDropped != 1 {
		t.Fatalf("drop-mode dropped = %d, want 1 (only b collapses into a; c is distinct from a)", dropDropped)
	}
	if keepDropped != dropDropped {
		t.Fatalf("keep-mode dropped = %d, drop-mode dropped = %d — counts must match (flagged b must not anchor c)", keepDropped, dropDropped)
	}
	if len(keptKeep) != 3 {
		t.Fatalf("keep-mode retains all rows: kept=%d, want 3", len(keptKeep))
	}
}

func TestDedupResults_MergeModeRecordsProvenance(t *testing.T) {
	a := vecEntry("a", []float32{1, 0, 0})
	aDup := vecEntry("a2", []float32{1, 0, 0})
	kept, dropped := DedupResults([]store.MemorySearchEntry{a, aDup}, DedupConfig{Enabled: true, Mode: dedupModeMerge})
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(kept) != 1 {
		t.Fatalf("merge mode kept %d, want 1", len(kept))
	}
	// The retained value must now be a merge envelope carrying the dropped
	// entry's key under merged_from.
	var env struct {
		Value      json.RawMessage `json:"value"`
		MergedFrom []struct {
			Key string `json:"key"`
		} `json:"merged_from"`
	}
	if err := json.Unmarshal(kept[0].Value, &env); err != nil {
		t.Fatalf("retained value is not a merge envelope: %v", err)
	}
	if len(env.MergedFrom) != 1 || env.MergedFrom[0].Key != "a2" {
		t.Fatalf("merged_from = %+v, want one entry keyed a2", env.MergedFrom)
	}
}

func TestDedupResults_MergeModeAppendsOnSecondDuplicate(t *testing.T) {
	a := vecEntry("a", []float32{1, 0, 0})
	dup1 := vecEntry("a2", []float32{1, 0, 0})
	dup2 := vecEntry("a3", []float32{1, 0, 0})
	kept, dropped := DedupResults([]store.MemorySearchEntry{a, dup1, dup2}, DedupConfig{Enabled: true, Mode: dedupModeMerge})
	if dropped != 2 || len(kept) != 1 {
		t.Fatalf("dropped=%d kept=%d, want 2 and 1", dropped, len(kept))
	}
	var env struct {
		MergedFrom []struct {
			Key string `json:"key"`
		} `json:"merged_from"`
	}
	if err := json.Unmarshal(kept[0].Value, &env); err != nil {
		t.Fatalf("not a merge envelope: %v", err)
	}
	if len(env.MergedFrom) != 2 {
		t.Fatalf("merged_from len = %d, want 2 (a2, a3)", len(env.MergedFrom))
	}
}

func TestDedupResults_ThresholdBoundary(t *testing.T) {
	// Build two vectors with a KNOWN cosine similarity, then set the
	// threshold just above and just below it to verify the >= boundary.
	// v1=(1,0), v2=(cosθ, sinθ): cosine similarity == cosθ.
	const cos = 0.95
	sin := math.Sqrt(1 - cos*cos)
	v1 := vecEntry("a", []float32{1, 0})
	v2 := vecEntry("b", []float32{float32(cos), float32(sin)})

	// Threshold below the pair's similarity → treated as duplicate.
	_, dropped := DedupResults([]store.MemorySearchEntry{v1, v2}, DedupConfig{Enabled: true, Threshold: 0.90})
	if dropped != 1 {
		t.Fatalf("threshold 0.90 (< sim %.3f): dropped=%d, want 1", cos, dropped)
	}
	// Threshold above the pair's similarity → NOT a duplicate.
	_, dropped = DedupResults([]store.MemorySearchEntry{v1, v2}, DedupConfig{Enabled: true, Threshold: 0.99})
	if dropped != 0 {
		t.Fatalf("threshold 0.99 (> sim %.3f): dropped=%d, want 0", cos, dropped)
	}
}

func TestDedupResults_ZeroThresholdUsesDefault(t *testing.T) {
	// Threshold <= 0 must fall back to DefaultDedupThreshold (0.92), not
	// collapse everything (which a literal threshold of 0 would do).
	v1 := vecEntry("a", []float32{1, 0, 0})
	v2 := vecEntry("b", []float32{0, 1, 0}) // orthogonal, sim 0
	_, dropped := DedupResults([]store.MemorySearchEntry{v1, v2}, DedupConfig{Enabled: true, Threshold: 0})
	if dropped != 0 {
		t.Fatalf("zero-threshold default collapsed orthogonal vectors: dropped=%d, want 0", dropped)
	}
}

func TestCosineSimilarity_ZeroNormIsSafe(t *testing.T) {
	// A zero vector has zero norm: similarity must be 0, never NaN.
	got := cosineSimilarity([]float32{0, 0, 0}, []float32{1, 2, 3})
	if got != 0 {
		t.Fatalf("zero-norm similarity = %v, want 0", got)
	}
	if math.IsNaN(got) {
		t.Fatal("zero-norm similarity produced NaN")
	}
}

func TestCosineSimilarity_MismatchedDimsAreZero(t *testing.T) {
	got := cosineSimilarity([]float32{1, 0}, []float32{1, 0, 0})
	if got != 0 {
		t.Fatalf("mismatched-dim similarity = %v, want 0", got)
	}
}

func TestCosineSimilarity_IdenticalIsOne(t *testing.T) {
	got := cosineSimilarity([]float32{1, 2, 3}, []float32{1, 2, 3})
	if math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("identical similarity = %v, want 1.0", got)
	}
}
