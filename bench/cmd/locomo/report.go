package main

// report.go — the run's output: a human-readable matrix and the JSON behind it.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Report is the whole run, written as report.json alongside matrix.md.
type Report struct {
	Tool            string   `json:"tool"`
	StartedAt       string   `json:"started_at"`
	Instance        string   `json:"instance"`
	Tenant          string   `json:"tenant"`
	Subject         string   `json:"subject"`
	Scope           string   `json:"scope"`
	TopK            int      `json:"top_k"`
	Categories      []int    `json:"categories_included"`
	Conversations   int      `json:"conversations"`
	CorpusRows      int      `json:"corpus_rows"`
	Queries         int      `json:"queries"`
	EmbeddingDim    int      `json:"query_embedding_dim"`
	Overall         Stats    `json:"overall"`
	PerCategory     []Stats  `json:"per_category"`
	PerConversation []Stats  `json:"per_conversation"`
	Defects         *Defects `json:"defects,omitempty"`
	Notes           []string `json:"notes,omitempty"`
	// Results is the full per-query detail, so a bad number can be traced to
	// the query that produced it rather than re-run to find out.
	Results []QueryResult `json:"results,omitempty"`
}

// Write persists report.json + matrix.md into dir.
func (r Report) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	var md strings.Builder
	r.Matrix(&md)
	return os.WriteFile(filepath.Join(dir, "matrix.md"), []byte(md.String()), 0o644)
}

func statsRow(w io.Writer, s Stats) {
	fmt.Fprintf(w, "| %s | %d | %.4f | %.4f | %.4f | %.4f | %.1f | %.1f |\n",
		s.Label, s.Queries, s.RecallAtK, s.PrecisionAtK, s.MRR, s.HitRate,
		s.LatencyP50Ms, s.LatencyP99Ms)
}

const statsHeader = "| slice | queries | recall@k | precision@k | MRR | hit-rate | p50 ms | p99 ms |\n" +
	"|---|---|---|---|---|---|---|---|\n"

// Matrix renders the human-readable report.
func (r Report) Matrix(w io.Writer) {
	fmt.Fprintf(w, "# LoCoMo memory retrieval — %s\n\n", r.StartedAt)
	fmt.Fprintf(w, "- instance: `%s`\n", r.Instance)
	fmt.Fprintf(w, "- tenant: `%s`  subject: `%s`  scope: `%s`\n", r.Tenant, r.Subject, r.Scope)
	fmt.Fprintf(w, "- conversations: %d (one memory scope_id each)\n", r.Conversations)
	fmt.Fprintf(w, "- corpus rows: %d   queries scored: %d   top_k: %d\n", r.CorpusRows, r.Queries, r.TopK)
	if r.EmbeddingDim > 0 {
		fmt.Fprintf(w, "- query embedding dimension: %d\n", r.EmbeddingDim)
	}
	// The filter is printed on every report because differing filters are the
	// reason published LoCoMo numbers cannot be compared with one another.
	cats := make([]string, 0, len(r.Categories))
	for _, c := range r.Categories {
		cats = append(cats, fmt.Sprintf("%d (%s)", c, CategoryName(c)))
	}
	fmt.Fprintf(w, "- **categories included: %s**\n", strings.Join(cats, ", "))

	fmt.Fprintf(w, "\n## Overall\n\n%s", statsHeader)
	statsRow(w, r.Overall)

	if len(r.PerCategory) > 0 {
		fmt.Fprintf(w, "\n## By category\n\n%s", statsHeader)
		for _, s := range r.PerCategory {
			statsRow(w, s)
		}
	}
	if len(r.PerConversation) > 0 {
		fmt.Fprintf(w, "\n## By conversation\n\n%s", statsHeader)
		for _, s := range r.PerConversation {
			statsRow(w, s)
		}
	}
	if r.Defects != nil && r.Defects.Any() {
		fmt.Fprintf(w, "\n## Dataset defects (excluded from the numbers above)\n\n")
		fmt.Fprintf(w, "- unreadable evidence fragments: %d\n", r.Defects.MalformedEvidenceFragments)
		fmt.Fprintf(w, "- evidence ids naming no turn in their own conversation: %d\n", r.Defects.UnresolvedEvidenceIDs)
		fmt.Fprintf(w, "- questions dropped for having no usable evidence: %d\n", r.Defects.QueriesWithoutEvidence)
		fmt.Fprintf(w, "- sessions declared with a timestamp but no turns: %d\n", r.Defects.SessionsDeclaredWithoutTurns)
		fmt.Fprintf(w, "- questions excluded by the category filter: %d\n", r.Defects.QueriesFilteredByCategory)
		if len(r.Defects.Examples) > 0 {
			fmt.Fprintf(w, "\nExamples:\n")
			for _, e := range r.Defects.Examples {
				fmt.Fprintf(w, "- %s\n", e)
			}
		}
	}
	if len(r.Notes) > 0 {
		fmt.Fprintf(w, "\n## Notes\n\n")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "- %s\n", n)
		}
	}
}

// DefaultOutDir names a run directory. The caller supplies the clock so the
// path is testable.
func DefaultOutDir(now time.Time) string {
	return filepath.Join("bench", "results", "locomo-"+now.UTC().Format("2006-01-02-1504"))
}
