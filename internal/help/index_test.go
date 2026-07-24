package help

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
// The counters are mutex-guarded so the concurrent-boot test is race-clean under
// `go test -race` (vocab is set once at construction and only read).
type helpFakeEmbedder struct {
	vocab map[string]int

	mu         sync.Mutex
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
	f.mu.Lock()
	f.embedCalls++
	f.embedTexts += len(texts)
	f.mu.Unlock()
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

	// Error injection for the degrade/atomic-write tests. When set, the
	// corresponding op returns the error instead of doing its work — modelling a
	// transient embedder/store fault (dimension-mismatch window, embed-set flake).
	embedSearchErr error
	embedSetErr    error
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
	if s.embedSetErr != nil {
		return s.embedSetErr // base row already written by the caller's MemorySet
	}
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
	if s.embedSearchErr != nil {
		return nil, s.embedSearchErr
	}
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

// TestHelpQuery_DegradesToSubstringOnHybridError asserts that when the hybrid
// path errors (e.g. a transient embedder failure, or the dimension-mismatch
// window during an embedder swap before the boot reconcile catches up), query
// does NOT hard-fail — it warns and degrades to the substring scan so the caller
// still gets useful results (query's "always returns something" contract).
func TestHelpQuery_DegradesToSubstringOnHybridError(t *testing.T) {
	topicA := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe frobnicator assembles widgets neatly.\n"}
	topicB := &Topic{Name: "beta", Description: "d", Content: "## Gadgets\nA sprocket drives gadgets.\n"}
	set := newHelpTestSet(topicA, topicB)
	st := newHelpFakeStore()
	emb := newHelpFakeEmbedder("frobnicator", "assembles", "widgets", "neatly", "sprocket", "drives", "gadgets")
	ctx := context.Background()

	if _, err := ReconcileIndex(ctx, set, st, emb); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Model the dimension-mismatch window: the vector leg errors for every query
	// until the boot reconcile re-embeds the stale rows.
	st.embedSearchErr = errors.New("memory: query embedding dimension 8 does not match stored rows' dimension 16")

	res, err := QueryIndex(ctx, set, st, emb, "frobnicator widgets", 5)
	if err != nil {
		t.Fatalf("query hard-failed on a hybrid error; want graceful degrade: %v", err)
	}
	if res.Mode != "substring" {
		t.Fatalf("mode = %q, want substring (degraded)", res.Mode)
	}
	if len(res.Results) == 0 || res.Results[0].TopicSlug != "alpha" || res.Results[0].Heading != "Widgets" {
		t.Fatalf("degraded results = %+v, want alpha#Widgets", res.Results)
	}
}

// TestHelpIndex_ReconcileEmbedFailureNotMarkedDone asserts the atomic-write
// invariant: when the embed-set fails after the base row is written, the section
// is NOT recorded as done (it keeps the empty sentinel hash), so the next
// reconcile re-attempts it rather than leaving it permanently un-embedded.
func TestHelpIndex_ReconcileEmbedFailureNotMarkedDone(t *testing.T) {
	topic := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe frobnicator assembles widgets.\n"}
	set := newHelpTestSet(topic)
	st := newHelpFakeStore()
	emb := newHelpFakeEmbedder("frobnicator", "assembles", "widgets")
	ctx := context.Background()

	total := len(AllSections(set)) // 1
	if total != 1 {
		t.Fatalf("expected 1 section, got %d", total)
	}

	// Reconcile #1: the embed-set fails. The section must be counted failed and
	// left WITHOUT a matching hash (so it is retried), not abort the whole index.
	st.embedSetErr = errors.New("simulated embed-set failure")
	stats, err := ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile #1 must not abort on a per-section embed failure: %v", err)
	}
	if stats.Written != 0 || stats.Failed != total {
		t.Fatalf("reconcile #1 stats = %+v, want Written=0 Failed=%d", stats, total)
	}
	// The base row exists (FK parent) but carries the "" sentinel hash — so the
	// next pass re-attempts it rather than treating it as Unchanged.
	st.mu.Lock()
	row, ok := st.rows["alpha#Widgets"]
	var stored json.RawMessage
	if ok {
		stored = append(json.RawMessage(nil), row.value...)
	}
	st.mu.Unlock()
	if !ok {
		t.Fatalf("base row missing after the phase-1 write")
	}
	var v indexValue
	if err := json.Unmarshal(stored, &v); err != nil {
		t.Fatalf("unmarshal stored row: %v", err)
	}
	if v.Hash != "" {
		t.Fatalf("stored hash = %q, want empty sentinel (a failed section must not be marked done)", v.Hash)
	}

	// Reconcile #2: embed-set now succeeds → the section is re-attempted (its
	// stored "" hash won't match the desired hash), written, and NOT Unchanged.
	st.embedSetErr = nil
	stats, err = ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}
	if stats.Written != total || stats.Unchanged != 0 || stats.Failed != 0 {
		t.Fatalf("reconcile #2 stats = %+v, want Written=%d Unchanged=0 Failed=0", stats, total)
	}

	// Reconcile #3: the real hash is now recorded → a clean no-op.
	stats, err = ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile #3: %v", err)
	}
	if stats.Unchanged != total || stats.Written != 0 {
		t.Fatalf("reconcile #3 stats = %+v, want Unchanged=%d Written=0", stats, total)
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

// ---- RFC BL PR6: read-time dead-link guard + reconcile OTEL metric ----

func withInMemoryExporter(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cleanup := lcotel.SetTracerProviderForTest(tp)
	t.Cleanup(func() {
		cleanup()
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

// TestHelpQuery_DeadLinkDroppedFromResults pins the RFC BL §2.10 read-time
// dead-link floor for the help case: an index row can outlive its topic in the
// window between a new-binary boot that dropped the topic from the corpus and
// the backgrounded reconcile pruning the stale row. A hybrid hit whose topic is
// no longer in the live in-memory Set is dropped from results; a live hit is
// kept. FAIL-BEFORE: without the guard the query returns the dropped topic's
// section, so the "alpha absent" assertion fails.
func TestHelpQuery_DeadLinkDroppedFromResults(t *testing.T) {
	// Both topics share the "sprocket" token so one query matches both.
	alpha := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe sprocket assembles widgets.\n"}
	beta := &Topic{Name: "beta", Description: "d", Content: "## Gadgets\nThe sprocket drives gadgets.\n"}
	full := newHelpTestSet(alpha, beta)
	st := newHelpFakeStore()
	emb := newHelpFakeEmbedder("sprocket", "assembles", "widgets", "drives", "gadgets")
	ctx := context.Background()

	// Index BOTH topics — the store now holds alpha's section rows.
	if _, err := ReconcileIndex(ctx, full, st, emb); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// A NEW binary boots with a corpus that dropped `alpha` (the reconcile that
	// would prune alpha's stale rows hasn't run yet). Query against the same
	// store with the reduced live Set.
	live := newHelpTestSet(beta)
	res, err := QueryIndex(ctx, live, st, emb, "sprocket", 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Mode != "hybrid" {
		t.Fatalf("mode = %q, want hybrid", res.Mode)
	}
	if len(res.Results) == 0 {
		t.Fatalf("no results; the live beta hit should survive")
	}
	for _, r := range res.Results {
		if r.TopicSlug == "alpha" {
			t.Fatalf("dead-link hit for dropped topic %q not filtered: %+v", "alpha", res.Results)
		}
	}
	var sawBeta bool
	for _, r := range res.Results {
		if r.TopicSlug == "beta" {
			sawBeta = true
		}
	}
	if !sawBeta {
		t.Fatalf("live hit for beta not kept: %+v", res.Results)
	}
}

// TestMetrics_HelpReconcileEmitted pins the RFC BL PR6 reconcile telemetry: a
// boot reconcile emits one loomcycle.help.reconcile span carrying the
// written/pruned/unchanged/failed counts + the degraded flag. FAIL-BEFORE:
// without RecordHelpReconcile no such span is emitted (spans==0).
func TestMetrics_HelpReconcileEmitted(t *testing.T) {
	exp := withInMemoryExporter(t)
	topic := &Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe sprocket assembles widgets.\n\n## Sprockets\nA sprocket spins.\n"}
	set := newHelpTestSet(topic)
	st := newHelpFakeStore()
	emb := newHelpFakeEmbedder("sprocket", "assembles", "widgets", "spins")
	ctx := context.Background()

	want := len(AllSections(set)) // 2
	stats, err := ReconcileIndex(ctx, set, st, emb)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Written != want {
		t.Fatalf("precondition: Written=%d, want %d", stats.Written, want)
	}

	var rec *tracetest.SpanStub
	for i := range exp.GetSpans() {
		s := exp.GetSpans()[i]
		if s.Name == lcotel.SpanHelpReconcile {
			rec = &s
			break
		}
	}
	if rec == nil {
		t.Fatalf("no %q span recorded; got %d spans", lcotel.SpanHelpReconcile, len(exp.GetSpans()))
	}
	ints := map[string]int64{}
	bools := map[string]bool{}
	for _, kv := range rec.Attributes {
		ints[string(kv.Key)] = kv.Value.AsInt64()
		bools[string(kv.Key)] = kv.Value.AsBool()
	}
	if ints[lcotel.AttrHelpWritten] != int64(want) {
		t.Errorf("%s = %d, want %d", lcotel.AttrHelpWritten, ints[lcotel.AttrHelpWritten], want)
	}
	if bools[lcotel.AttrHelpDegraded] {
		t.Errorf("%s = true, want false", lcotel.AttrHelpDegraded)
	}
}
