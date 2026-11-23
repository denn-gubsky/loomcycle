package builtin

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// defEmbedder embeds a text as the vector of the first rule whose word it holds
// (lower case), else a fixed default, and records every text it was asked. Its
// provider and model match newConceptFixture's fake embedder, so the annotation
// vectors a test sets are of its model.
type defEmbedder struct {
	mu      sync.Mutex
	model   string
	rules   []defRule
	def     []float32
	texts   []string
	failing bool
}

type defRule struct {
	word string
	vec  []float32
}

func newDefEmbedder(rules ...defRule) *defEmbedder {
	return &defEmbedder{model: "m1", rules: rules, def: []float32{0, 0, 0, 1}}
}

func (e *defEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failing {
		return nil, errInjectedEmbed
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		e.texts = append(e.texts, t)
		out[i] = e.def
		low := strings.ToLower(t)
		for _, r := range e.rules {
			if strings.Contains(low, r.word) {
				out[i] = r.vec
				break
			}
		}
	}
	return out, nil
}

func (e *defEmbedder) Model() string    { return e.model }
func (e *defEmbedder) Provider() string { return "fake" }
func (e *defEmbedder) Dimension() int   { return 4 }

// definitionTexts is the texts the embedder was asked that are concept definitions.
func (e *defEmbedder) definitionTexts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, t := range e.texts {
		if strings.HasPrefix(t, "[concept · ") {
			out = append(out, t)
		}
	}
	return out
}

func (e *defEmbedder) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = nil
}

type injectedEmbedErr struct{}

func (injectedEmbedErr) Error() string { return "injected embed failure" }

var errInjectedEmbed error = injectedEmbedErr{}

// conceptCount is how many concepts conceptOntologyMD declares.
const conceptCount = 6

// TestNodeDefinitions_RecomputedOnlyWhenTheDefinitionChanges — compute embeds every
// concept's definition once; a second compute embeds none; editing one concept's
// meaning re-embeds that one; a new embed model re-embeds them all.
func TestNodeDefinitions_RecomputedOnlyWhenTheDefinitionChanges(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	emb := newDefEmbedder()
	d.Embedder = emb

	first := nodeStatsRun(t, d, k.f.operator, computeAll)
	if first.Definitions == nil || first.Definitions.Embedded != conceptCount || first.Definitions.Fresh != 0 {
		t.Fatalf("first compute definitions = %+v; want %d embedded", first.Definitions, conceptCount)
	}
	if got := len(emb.definitionTexts()); got != conceptCount {
		t.Fatalf("embedded %d definition texts, want %d", got, conceptCount)
	}
	emb.reset()
	second := nodeStatsRun(t, d, k.f.operator, computeAll)
	if second.Definitions == nil || second.Definitions.Embedded != 0 || second.Definitions.Fresh != conceptCount {
		t.Errorf("second compute definitions = %+v; want all fresh", second.Definitions)
	}
	if got := emb.definitionTexts(); len(got) != 0 {
		t.Errorf("an unchanged definition was embedded again: %q", got)
	}

	g := k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": k.law})
	k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": k.law,
		"body": "Statutes and regulations in force.", "revision": g["revision"]})
	emb.reset()
	third := nodeStatsRun(t, d, k.f.operator, computeAll)
	texts := emb.definitionTexts()
	if third.Definitions.Embedded != 1 || len(texts) != 1 || !strings.Contains(texts[0], "Statutes and regulations") {
		t.Errorf("after a meaning edit: definitions %+v, texts %q; want only Law's re-embedded", third.Definitions, texts)
	}

	emb.model = "m2"
	emb.reset()
	fourth := nodeStatsRun(t, d, k.f.operator, computeAll)
	if fourth.Definitions.Embedded != conceptCount {
		t.Errorf("after a model change: %+v; want all re-embedded", fourth.Definitions)
	}
}

// TestNodeDefinitions_StoreWithoutVectorsKeepsNone — on a store without vectors the
// compute answer carries no definitions block and nothing is embedded.
func TestNodeDefinitions_StoreWithoutVectorsKeepsNone(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	emb := newDefEmbedder()
	d.Embedder = emb
	d.Store = k.f.c.vs.Store
	ans := nodeStatsRun(t, d, k.f.operator, computeAll)
	if ans.Definitions != nil || len(emb.definitionTexts()) != 0 {
		t.Errorf("definitions %+v, texts %q; want none on a store without vectors", ans.Definitions, emb.definitionTexts())
	}
}

// TestNodeDefinitions_EmbedderFailureKeepsTheCountsAndSaysSo — a failing embedder
// does not fail the pass: node statistics are still computed, the definitions are
// counted pending with the error, and the next pass embeds them.
func TestNodeDefinitions_EmbedderFailureKeepsTheCountsAndSaysSo(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newStatsCorpus(t, d, base)
	emb := newDefEmbedder()
	d.Embedder = emb
	emb.failing = true
	ans := nodeStatsRun(t, d, k.f.operator, computeAll)
	if ans.Computed == 0 || ans.Definitions == nil || ans.Definitions.Error == "" || ans.Definitions.Pending != conceptCount {
		t.Fatalf("answer = computed %d definitions %+v; want counts computed and the failure reported", ans.Computed, ans.Definitions)
	}
	emb.failing = false
	if again := nodeStatsRun(t, d, k.f.operator, computeAll); again.Definitions.Embedded != conceptCount {
		t.Errorf("next pass = %+v; want every definition embedded", again.Definitions)
	}
}

func TestNodeDefinitionText_NamesThePlaceMeaningAndAltLabels(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(conceptOntologyMD, "##### outsourcing\n\n", "##### outsourcing\nThird parties doing our work.\n@alt vendor management\n\n", 1)
	f := newConceptFixtureMD(t, d, base, md)
	read, _ := d.tenantOntologyRead(f.operator)
	var got string
	for _, def := range nodeDefinitionsOf(read, "", "m") {
		if def.id == f.concepts["topics/risk-management/outsourcing"] {
			got = def.text
		}
	}
	want := "[concept · topics] Risk Management › outsourcing\nThird parties doing our work.\nAlso: vendor management"
	if got != want {
		t.Errorf("text = %q\nwant   %q", got, want)
	}
}
