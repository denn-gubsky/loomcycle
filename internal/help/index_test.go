package help

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ---- test fixtures ----

// newHelpTestSet builds a *Set directly from in-memory topics (white-box), so a
// test controls the exact section shape without the go:embed corpus.
func newHelpTestSet(topics ...*Topic) *Set {
	s := &Set{topics: map[string]*Topic{}}
	for _, t := range topics {
		if t.Source == "" {
			t.Source = "bundled"
		}
		s.topics[t.Name] = t
	}
	s.reindex()
	return s
}

// helpFakeEmbedder one-hot encodes tokens against a fixed vocab and counts calls
// + total texts embedded, so a test can assert reconcile made zero embed calls.
type helpFakeEmbedder struct {
	vocab      map[string]int
	embedCalls int
	embedTexts int
}

func newHelpFakeEmbedder(tokens ...string) *helpFakeEmbedder {
	v := map[string]int{}
	for i, t := range tokens {
		v[t] = i
	}
	return &helpFakeEmbedder{vocab: v}
}

func (f *helpFakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.embedCalls++
	f.embedTexts += len(texts)
	out := make([][]float32, len(texts))
	for i, txt := range texts {
		vec := make([]float32, len(f.vocab))
		for _, tok := range strings.FieldsFunc(strings.ToLower(txt), func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
		}) {
			if idx, ok := f.vocab[tok]; ok {
				vec[idx] = 1
			}
		}
		out[i] = vec
	}
	return out, nil
}

func (f *helpFakeEmbedder) Provider() string { return "fake" }
func (f *helpFakeEmbedder) Model() string    { return "fake-001" }
func (f *helpFakeEmbedder) Dimension() int   { return len(f.vocab) }

// helpFakeStore implements help.IndexStore in memory, mutex-guarded so the
// concurrency test is race-clean. It counts base/embedding writes + deletes.
type helpFakeStore struct {
	mu       sync.Mutex
	rows     map[string]*helpFakeRow // key -> row
	vectors  bool
	fullText bool

	setCount      int
	embedSetCount int
	deleteCount   int
}

type helpFakeRow struct {
	value     json.RawMessage
	vector    []float32
	embedText string
}

func newHelpFakeStore() *helpFakeStore {
	return &helpFakeStore{rows: map[string]*helpFakeRow{}, vectors: true, fullText: true}
}

func (s *helpFakeStore) SupportsVectors() bool  { return s.vectors }
func (s *helpFakeStore) SupportsFullText() bool { return s.fullText }

func (s *helpFakeStore) MemoryList(_ context.Context, _ string, _ store.MemoryScope, _, prefix string, _ int) ([]store.MemoryEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.MemoryEntry, 0, len(s.rows))
	for k, r := range s.rows {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		out = append(out, store.MemoryEntry{Key: k, Value: append(json.RawMessage(nil), r.value...)})
	}
	return out, false, nil
}

func (s *helpFakeStore) MemorySet(_ context.Context, _ string, _ store.MemoryScope, _, key string, value json.RawMessage, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCount++
	r := s.rows[key]
	if r == nil {
		r = &helpFakeRow{}
		s.rows[key] = r
	}
	r.value = append(json.RawMessage(nil), value...)
	return nil
}

func (s *helpFakeStore) MemoryEmbedSet(_ context.Context, _ string, _ store.MemoryScope, _, key string, e store.MemoryEmbedding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embedSetCount++
	r := s.rows[key]
	if r == nil {
		// Mirror the real FK: base row must exist first.
		r = &helpFakeRow{}
		s.rows[key] = r
	}
	r.vector = e.Vector
	r.embedText = e.EmbedText
	return nil
}

func (s *helpFakeStore) MemoryDelete(_ context.Context, _ string, _ store.MemoryScope, _, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCount++
	_, ok := s.rows[key]
	delete(s.rows, key) // cascades the embedding, as Postgres' FK does
	return ok, nil
}

func (s *helpFakeStore) MemoryEmbedSearch(_ context.Context, _ string, _ store.MemoryScope, _, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type scored struct {
		key string
		r   *helpFakeRow
		s   float64
	}
	var rows []scored
	for k, r := range s.rows {
		if keyPrefix != "" && !strings.HasPrefix(k, keyPrefix) {
			continue
		}
		rows = append(rows, scored{key: k, r: r, s: helpCosine(query, r.vector)})
	}
	sortScoredDesc(rows, func(i int) (float64, string) { return rows[i].s, rows[i].key })
	if len(rows) > topK {
		rows = rows[:topK]
	}
	out := make([]store.MemorySearchEntry, 0, len(rows))
	for _, sc := range rows {
		out = append(out, store.MemorySearchEntry{
			MemoryEntry: store.MemoryEntry{Key: sc.key, Value: append(json.RawMessage(nil), sc.r.value...)},
			Score:       sc.s,
		})
	}
	return out, nil
}

func (s *helpFakeStore) MemoryFullTextSearch(_ context.Context, _ string, _ store.MemoryScope, _, keyPrefix, queryText string, topK int) ([]store.MemorySearchEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fullText {
		return nil, nil
	}
	terms := strings.Fields(strings.ToLower(queryText))
	type scored struct {
		key string
		r   *helpFakeRow
		s   float64
	}
	var rows []scored
	for k, r := range s.rows {
		if keyPrefix != "" && !strings.HasPrefix(k, keyPrefix) {
			continue
		}
		lower := strings.ToLower(r.embedText)
		hits := 0
		for _, t := range terms {
			hits += strings.Count(lower, t)
		}
		if hits == 0 {
			continue
		}
		rows = append(rows, scored{key: k, r: r, s: float64(hits)})
	}
	sortScoredDesc(rows, func(i int) (float64, string) { return rows[i].s, rows[i].key })
	if len(rows) > topK {
		rows = rows[:topK]
	}
	out := make([]store.MemorySearchEntry, 0, len(rows))
	for _, sc := range rows {
		out = append(out, store.MemorySearchEntry{
			MemoryEntry: store.MemoryEntry{Key: sc.key, Value: append(json.RawMessage(nil), sc.r.value...)},
			Score:       sc.s,
		})
	}
	return out, nil
}

// sortScoredDesc sorts a []scored (accessed via key(i)=(score,tiebreak)) by
// descending score, then ascending key for stability. Generic over the slice by
// index so both search legs reuse it.
func sortScoredDesc[T any](rows []T, key func(i int) (float64, string)) {
	// insertion sort is fine — the test corpora are tiny.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			sj, kj := key(j)
			sp, kp := key(j - 1)
			less := sj > sp || (sj == sp && kj < kp)
			if !less {
				break
			}
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

func helpCosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---- tests ----

// TestHelpIndex_ReconcileReembedsOnlyChanged asserts the content-hash gate: an
// unchanged corpus makes zero embed writes; a changed section re-writes ONLY
// itself; a removed section is pruned.
func TestHelpIndex_ReconcileReembedsOnlyChanged(t *testing.T) {
	topicA := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe frobnicator assembles widgets.\n\n## Sprockets\nA sprocket spins.\n"}
	topicB := &Topic{Name: "beta", Description: "d", Content: "## Gadgets\nA sprocket drives gadgets.\n"}
	set := newHelpTestSet(topicA, topicB)
	st := newHelpFakeStore()
	emb := newHelpFakeEmbedder("frobnicator", "assembles", "widgets", "sprocket", "spins", "drives", "gadgets")
	ctx := context.Background()

	total := len(AllSections(set)) // 3: alpha#Widgets, alpha#Sprockets, beta#Gadgets
	if total != 3 {
		t.Fatalf("expected 3 sections, got %d", total)
	}

	// Reconcile #1: full index.
	stats, err := ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile #1: %v", err)
	}
	if stats.Written != total || stats.Pruned != 0 || stats.Unchanged != 0 {
		t.Fatalf("reconcile #1 stats = %+v, want Written=%d Pruned=0 Unchanged=0", stats, total)
	}
	if st.embedSetCount != total {
		t.Fatalf("reconcile #1 embedSet calls = %d, want %d", st.embedSetCount, total)
	}

	// Reconcile #2: unchanged corpus => ZERO embed calls / writes.
	st.embedSetCount, st.setCount, st.deleteCount = 0, 0, 0
	emb.embedCalls, emb.embedTexts = 0, 0
	stats, err = ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}
	if stats.Written != 0 || stats.Unchanged != total || stats.Pruned != 0 {
		t.Fatalf("reconcile #2 stats = %+v, want Written=0 Unchanged=%d Pruned=0", stats, total)
	}
	if emb.embedCalls != 0 || emb.embedTexts != 0 || st.embedSetCount != 0 || st.setCount != 0 {
		t.Fatalf("reconcile #2 did work on an unchanged corpus: embedCalls=%d embedTexts=%d embedSet=%d set=%d",
			emb.embedCalls, emb.embedTexts, st.embedSetCount, st.setCount)
	}

	// Change ONE section's body (same heading => same key). Only it re-writes.
	topicA.Content = "## Widgets\nThe frobnicator now assembles many widgets carefully.\n\n## Sprockets\nA sprocket spins.\n"
	st.embedSetCount, st.setCount, st.deleteCount = 0, 0, 0
	emb.embedCalls, emb.embedTexts = 0, 0
	stats, err = ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile #3: %v", err)
	}
	if stats.Written != 1 || stats.Unchanged != total-1 || stats.Pruned != 0 {
		t.Fatalf("reconcile #3 stats = %+v, want Written=1 Unchanged=%d Pruned=0", stats, total-1)
	}
	if emb.embedTexts != 1 || st.embedSetCount != 1 {
		t.Fatalf("reconcile #3 re-embedded more than the changed section: embedTexts=%d embedSet=%d", emb.embedTexts, st.embedSetCount)
	}

	// Remove a section (drop alpha#Sprockets) => it is pruned.
	topicA.Content = "## Widgets\nThe frobnicator now assembles many widgets carefully.\n"
	st.embedSetCount, st.setCount, st.deleteCount = 0, 0, 0
	emb.embedCalls, emb.embedTexts = 0, 0
	stats, err = ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile #4: %v", err)
	}
	if stats.Pruned != 1 || stats.Written != 0 {
		t.Fatalf("reconcile #4 stats = %+v, want Pruned=1 Written=0", stats)
	}
	if st.deleteCount != 1 {
		t.Fatalf("reconcile #4 delete calls = %d, want 1", st.deleteCount)
	}
	if _, ok := st.rows["alpha#Sprockets"]; ok {
		t.Fatalf("alpha#Sprockets was not pruned from the store")
	}
}

// TestHelpQuery_HybridSurfacesSection asserts the hybrid (vector ∥ full-text →
// RRF) path surfaces the right topic/section.
func TestHelpQuery_HybridSurfacesSection(t *testing.T) {
	topicA := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe frobnicator assembles widgets neatly.\n"}
	topicB := &Topic{Name: "beta", Description: "d", Content: "## Gadgets\nA sprocket drives gadgets.\n"}
	set := newHelpTestSet(topicA, topicB)
	st := newHelpFakeStore()
	emb := newHelpFakeEmbedder("frobnicator", "assembles", "widgets", "neatly", "sprocket", "drives", "gadgets")
	ctx := context.Background()

	if _, err := ReconcileIndex(ctx, set, st, emb); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	res, err := QueryIndex(ctx, set, st, emb, "frobnicator widgets", 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Mode != "hybrid" {
		t.Fatalf("mode = %q, want hybrid", res.Mode)
	}
	if len(res.Results) == 0 {
		t.Fatalf("no results")
	}
	if res.Results[0].TopicSlug != "alpha" || res.Results[0].Heading != "Widgets" {
		t.Fatalf("top hit = %+v, want alpha#Widgets", res.Results[0])
	}
	if res.Results[0].Snippet == "" {
		t.Fatalf("top hit has empty snippet")
	}
}

// TestHelpQuery_DegradesToSubstringWithoutIndex asserts query still works with no
// embedder/store — a substring scan over the in-memory Set.
func TestHelpQuery_DegradesToSubstringWithoutIndex(t *testing.T) {
	topicA := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe frobnicator assembles widgets neatly.\n"}
	topicB := &Topic{Name: "beta", Description: "d", Content: "## Gadgets\nA sprocket drives gadgets.\n"}
	set := newHelpTestSet(topicA, topicB)

	// No store, no embedder → degrade path.
	res, err := QueryIndex(context.Background(), set, nil, nil, "frobnicator", 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Mode != "substring" {
		t.Fatalf("mode = %q, want substring", res.Mode)
	}
	if len(res.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(res.Results))
	}
	if res.Results[0].TopicSlug != "alpha" || res.Results[0].Heading != "Widgets" {
		t.Fatalf("hit = %+v, want alpha#Widgets", res.Results[0])
	}
}

// TestHelpQuery_MermaidSourceSearchable asserts a mermaid diagram's SOURCE (a
// node label) is indexed as text and thus searchable (RFC BL §2.9), and that a
// `## ` inside a fenced block does NOT spuriously split a section.
func TestHelpQuery_MermaidSourceSearchable(t *testing.T) {
	content := "## Flow\n" +
		"Here is the pipeline:\n\n" +
		"```mermaid\n" +
		"graph TD\n" +
		"  A[\"Frobnicator Pipeline\"] --> B[Sink]\n" +
		"```\n\n" +
		"Example config:\n\n" +
		"```yaml\n" +
		"## NotAHeading inside a fence\n" +
		"key: value\n" +
		"```\n"
	topic := &Topic{Name: "diagrams", Description: "d", Content: content}
	set := newHelpTestSet(topic)

	// Section splitter: exactly one `##` section, its Text keeps the mermaid
	// source, and the fenced `## NotAHeading` did not create a section.
	secs := sectionsForTopic(topic)
	if len(secs) != 1 {
		t.Fatalf("expected 1 section, got %d: %+v", len(secs), secs)
	}
	if secs[0].Heading != "Flow" {
		t.Fatalf("heading = %q, want Flow", secs[0].Heading)
	}
	if !strings.Contains(secs[0].Text, "Frobnicator Pipeline") {
		t.Fatalf("mermaid node label was stripped from section text")
	}

	// Query (degrade path) matches the mermaid label.
	res, err := QueryIndex(context.Background(), set, nil, nil, "Frobnicator", 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Results) != 1 || res.Results[0].TopicSlug != "diagrams" || res.Results[0].Heading != "Flow" {
		t.Fatalf("results = %+v, want one diagrams#Flow hit", res.Results)
	}
}

// TestHelpIndex_SingletonUnderConcurrentBoot runs two reconciles concurrently
// (exercised under `go test -race`) and asserts the store ends in the exact
// desired state — no duplicate/racy writes. (Cluster-wide single-runner-ness is
// the advisory lock's job, integration-tested with Postgres; this asserts the
// reconcile itself is internally race-safe + idempotent.)
func TestHelpIndex_SingletonUnderConcurrentBoot(t *testing.T) {
	topicA := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe frobnicator assembles widgets.\n\n## Sprockets\nA sprocket spins.\n"}
	topicB := &Topic{Name: "beta", Description: "d", Content: "## Gadgets\nA sprocket drives gadgets.\n"}
	set := newHelpTestSet(topicA, topicB)
	st := newHelpFakeStore()
	emb := newHelpFakeEmbedder("frobnicator", "assembles", "widgets", "sprocket", "spins", "drives", "gadgets")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ReconcileIndex(ctx, set, st, emb); err != nil {
				t.Errorf("concurrent reconcile: %v", err)
			}
		}()
	}
	wg.Wait()

	want := AllSections(set)
	st.mu.Lock()
	got := len(st.rows)
	st.mu.Unlock()
	if got != len(want) {
		t.Fatalf("store has %d rows, want exactly %d (no duplicates)", got, len(want))
	}
	for _, s := range want {
		st.mu.Lock()
		_, ok := st.rows[s.Key]
		st.mu.Unlock()
		if !ok {
			t.Fatalf("desired section %q missing from store", s.Key)
		}
	}
}
