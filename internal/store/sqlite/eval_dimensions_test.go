package sqlite

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestEvaluationGet_MalformedDimensionsLogged is the exp7 regression: the
// evaluation scan path previously discarded the dimensions json.Unmarshal
// error (`_ = json.Unmarshal(...)`), so a corrupt row read back with empty
// Dimensions and no trace of why. The scan now logs the parse failure and
// still returns the row. Fail-before: the unfixed code emits no log line, so
// the buffer is empty and this assertion fails.
func TestEvaluationGet_MalformedDimensionsLogged(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.EvaluationSubmit(ctx, store.EvaluationRow{
		EvalID:      "eval_bad_dims",
		RunID:       "run_1",
		EmitterRole: "judge",
		Dimensions:  map[string]float64{"clarity": 0.9},
	}); err != nil {
		t.Fatalf("EvaluationSubmit: %v", err)
	}

	// Corrupt the stored dimensions JSON directly — the write path marshals
	// valid JSON, so a malformed value can only arrive via a hand-edited row.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE evaluations SET dimensions = ? WHERE eval_id = ?`,
		`{not valid json`, "eval_bad_dims",
	); err != nil {
		t.Fatalf("corrupt dimensions: %v", err)
	}

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	got, err := s.EvaluationGet(ctx, "eval_bad_dims")
	if err != nil {
		t.Fatalf("EvaluationGet returned error for a malformed-dimensions row: %v", err)
	}
	if got.EvalID != "eval_bad_dims" {
		t.Errorf("EvalID = %q, want eval_bad_dims", got.EvalID)
	}
	if len(got.Dimensions) != 0 {
		t.Errorf("Dimensions = %v, want empty after a failed parse", got.Dimensions)
	}
	if !strings.Contains(buf.String(), "dimensions JSON parse failed") {
		t.Errorf("parse failure was not logged; log=%q", buf.String())
	}
}
