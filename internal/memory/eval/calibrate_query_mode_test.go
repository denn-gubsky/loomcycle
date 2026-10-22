package eval

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers/embedtest"
)

// TestCalibrate_EmbedsBasesAsStoredTextAndProbesAsQueries — RFC EV §5.3. The
// merge pass compares a new fact (embedded by Memory recall, in query mode)
// with stored facts, so calibration must measure that same comparison: bases
// through Embed, probes through EmbedQuery. For an embedder without a query
// mode both are Embed and the report is unchanged.
func TestCalibrate_EmbedsBasesAsStoredTextAndProbesAsQueries(t *testing.T) {
	corpus, err := BuiltinCalibrationCorpus()
	if err != nil {
		t.Fatal(err)
	}
	rec := embedtest.New(NewDeterministicEmbedder(64))
	got, err := Calibrate(context.Background(), corpus, rec, ConfiguredBands{})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if docs := rec.Documents(); !slices.Equal(docs, corpus.Bases) {
		t.Errorf("stored-text embeddings = %d texts, want exactly the %d bases", len(docs), len(corpus.Bases))
	}
	var probes []string
	for _, p := range corpus.Duplicates {
		probes = append(probes, p.Text)
	}
	for _, p := range corpus.Related {
		probes = append(probes, p.Text)
	}
	if qs := rec.Queries(); !slices.Equal(qs, probes) {
		t.Errorf("query-mode embeddings = %d texts, want exactly the %d probes", len(qs), len(probes))
	}

	plain, err := Calibrate(context.Background(), corpus, NewDeterministicEmbedder(64), ConfiguredBands{})
	if err != nil {
		t.Fatalf("Calibrate (no query mode): %v", err)
	}
	if !reflect.DeepEqual(got, plain) {
		t.Error("the report changed with the mode split for an embedder whose two modes are the same")
	}
}
