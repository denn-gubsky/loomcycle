package memory

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func entry(key string, score float64, ageHours float64, now time.Time) store.MemorySearchEntry {
	var e store.MemorySearchEntry
	e.Key = key
	e.Score = score
	e.CreatedAt = now.Add(-time.Duration(ageHours) * time.Hour)
	return e
}

// Default config (pure semantic) must leave the store's cosine ordering
// exactly as-is — the zero-regression guarantee.
func TestRankCandidates_DefaultIsPureSemanticNoReorder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	in := []store.MemorySearchEntry{
		entry("a", 0.9, 100, now), // high cosine, old
		entry("b", 0.8, 1, now),   // lower cosine, fresh
		entry("c", 0.7, 0, now),
	}
	out := RankCandidates(in, DefaultRankConfig(), now)
	got := []string{out[0].Key, out[1].Key, out[2].Key}
	want := []string{"a", "b", "c"} // unchanged cosine order
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default rank reordered results: got %v, want %v", got, want)
		}
	}
}

// With a heavy recency weight, a fresher-but-slightly-less-similar entry
// is promoted above an older high-cosine one.
func TestRankCandidates_RecencyPromotesFreshEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	in := []store.MemorySearchEntry{
		entry("old-similar", 0.90, 240, now), // 10 days old
		entry("fresh", 0.80, 1, now),         // 1 hour old
	}
	cfg := RankConfig{SemanticWeight: 1.0, RecencyWeight: 1.0, RecencyHalfLifeHours: 24}
	out := RankCandidates(in, cfg, now)
	// old-similar: 0.90 + exp(-240·ln2/24)=~0.90+~0.001 ≈ 0.901
	// fresh:       0.80 + exp(-1·ln2/24)  =~0.80+~0.971 ≈ 1.771
	if out[0].Key != "fresh" {
		t.Fatalf("recency weight did not promote the fresh entry: got order %s,%s", out[0].Key, out[1].Key)
	}
}

func TestRecencyDecay_HalfLifeAndGuards(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// at one half-life → 0.5
	if d := recencyDecay(now.Add(-24*time.Hour), now, 24); d < 0.49 || d > 0.51 {
		t.Errorf("decay at one half-life = %v, want ~0.5", d)
	}
	// age 0 → 1.0
	if d := recencyDecay(now, now, 24); d < 0.999 {
		t.Errorf("decay at age 0 = %v, want ~1.0", d)
	}
	// non-positive half-life disables the term (no divide-by-zero, no reward)
	if d := recencyDecay(now.Add(-time.Hour), now, 0); d != 0 {
		t.Errorf("decay with zero half-life = %v, want 0", d)
	}
	// future timestamp (clock skew) is clamped to "now" → 1.0, not >1
	if d := recencyDecay(now.Add(time.Hour), now, 24); d < 0.999 || d > 1.0001 {
		t.Errorf("decay for future timestamp = %v, want ~1.0 (clamped)", d)
	}
}

func TestRankConfig_PureSemanticAndReservedFlags(t *testing.T) {
	if !DefaultRankConfig().IsPureSemantic() {
		t.Error("default config should be pure semantic")
	}
	if (RankConfig{SemanticWeight: 1, RecencyWeight: 0.5}).IsPureSemantic() {
		t.Error("recency weight set should not be pure semantic")
	}
	if !(RankConfig{SemanticWeight: 1, SourceWeight: 0.3}).SourceFrequencyReserved() {
		t.Error("source weight set should flag reserved")
	}
	if !(RankConfig{SemanticWeight: 1, FrequencyWeight: 0.2}).SourceFrequencyReserved() {
		t.Error("frequency weight set should flag reserved")
	}
	if DefaultRankConfig().SourceFrequencyReserved() {
		t.Error("default config should not flag reserved")
	}
}

// Stable sort: equal hybrid scores keep input (cosine) order.
func TestRankCandidates_StableOnTies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	in := []store.MemorySearchEntry{
		entry("first", 0.5, 0, now),
		entry("second", 0.5, 0, now),
	}
	// recency weight active so it doesn't take the pure-semantic short-circuit,
	// but both entries score identically → order preserved.
	cfg := RankConfig{SemanticWeight: 1, RecencyWeight: 1, RecencyHalfLifeHours: 24}
	out := RankCandidates(in, cfg, now)
	if out[0].Key != "first" || out[1].Key != "second" {
		t.Fatalf("tie ordering not stable: got %s,%s", out[0].Key, out[1].Key)
	}
}
