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
	Tool                string   `json:"tool"`
	StartedAt           string   `json:"started_at"`
	Instance            string   `json:"instance"`
	Tenant              string   `json:"tenant"`
	Subject             string   `json:"subject"`
	Scope               string   `json:"scope"`
	TopK                int      `json:"top_k"`
	Categories          []int    `json:"categories_included"`
	Conversations       int      `json:"conversations"`
	ConversationsInFile int      `json:"conversations_in_file"`
	CorpusRows          int      `json:"corpus_rows"`
	Queries             int      `json:"queries"`
	EmbeddingDim        int      `json:"query_embedding_dim"`
	Overall             Stats    `json:"overall"`
	PerCategory         []Stats  `json:"per_category"`
	PerConversation     []Stats  `json:"per_conversation"`
	Defects             *Defects `json:"defects,omitempty"`
	Notes               []string `json:"notes,omitempty"`
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
		pop := fmt.Sprintf("all %d conversations in the file", r.ConversationsInFile)
		if r.ConversationsInFile == 0 || r.ConversationsInFile == r.Conversations {
			pop = "the conversations scored above"
		}
		fmt.Fprintf(w, "\n## Dataset defects (counted across %s; excluded from the numbers above)\n\n", pop)
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

// AnswerReport is the answer axis's output.
type AnswerReport struct {
	Tool          string `json:"tool"`
	Axis          string `json:"axis"`
	StartedAt     string `json:"started_at"`
	Instance      string `json:"instance"`
	Tenant        string `json:"tenant"`
	Subject       string `json:"subject"`
	Answerer      string `json:"answerer"`
	Judge         string `json:"judge"`
	Categories    []int  `json:"categories_included"`
	Conversations int    `json:"conversations"`
	Turns         int    `json:"turns_ingested"`
	Sessions      int    `json:"sessions_ingested"`
	FactsWritten  int    `json:"facts_written"`
	// SeededTurns counts turn rows written straight into the answerer's own
	// partition (-seed-turns). It is the difference between "answered from
	// distilled facts" and "answered from conversation content", so a report
	// without it cannot be compared against one with it.
	SeededTurns int            `json:"seeded_turn_rows"`
	Sampled     int            `json:"sampled_questions"`
	Overall     AnswerStats    `json:"overall"`
	PerCategory []AnswerStats  `json:"per_category"`
	Defects     *Defects       `json:"defects,omitempty"`
	Notes       []string       `json:"notes,omitempty"`
	Results     []AnswerResult `json:"results,omitempty"`
}

const answerHeader = "| slice | asked | graded | accuracy | correct | partial | wrong | unparsed | NOT_FOUND | p50 ms |\n" +
	"|---|---|---|---|---|---|---|---|---|---|\n"

func answerRow(w io.Writer, s AnswerStats) {
	fmt.Fprintf(w, "| %s | %d | %d | %.4f | %d | %d | %d | %d | %.3f | %.0f |\n",
		s.Label, s.Queries, s.Graded, s.Accuracy, s.Correct, s.Partial, s.Wrong,
		s.Unparsed, s.NotFoundRate, s.LatencyP50Ms)
}

// Matrix renders the answer-axis report.
func (r AnswerReport) Matrix(w io.Writer) {
	fmt.Fprintf(w, "# LoCoMo answer axis — %s\n\n", r.StartedAt)
	fmt.Fprintf(w, "- instance: `%s`   tenant: `%s`   memory subject: `%s`\n", r.Instance, r.Tenant, r.Subject)
	fmt.Fprintf(w, "- answerer: `%s`   judge: `%s`\n", r.Answerer, r.Judge)
	// The answerer's store is stated FIRST, because it is the single fact that
	// decides what the accuracy below means: answering from 29 distilled facts and
	// answering from 419 conversation turns are different measurements.
	if r.SeededTurns > 0 {
		fmt.Fprintf(w, "- **answerer's store: %d embedded TURN rows + %d consolidated facts** (-seed-turns)\n",
			r.SeededTurns, r.FactsWritten)
	} else {
		fmt.Fprintf(w, "- **answerer's store: %d consolidated facts only** (no -seed-turns; the raw turns are not in the partition it reads)\n",
			r.FactsWritten)
	}
	fmt.Fprintf(w, "- ingested: %d turns across %d sessions from %d conversation(s); %d facts written by consolidation\n",
		r.Turns, r.Sessions, r.Conversations, r.FactsWritten)
	cats := make([]string, 0, len(r.Categories))
	for _, c := range r.Categories {
		cats = append(cats, fmt.Sprintf("%d (%s)", c, CategoryName(c)))
	}
	fmt.Fprintf(w, "- **categories included: %s**\n", strings.Join(cats, ", "))
	fmt.Fprintf(w, "- questions asked: %d\n", r.Sampled)

	fmt.Fprintf(w, "\n## Overall\n\n%s", answerHeader)
	answerRow(w, r.Overall)
	if len(r.PerCategory) > 0 {
		fmt.Fprintf(w, "\n## By category\n\n%s", answerHeader)
		for _, s := range r.PerCategory {
			answerRow(w, s)
		}
	}
	fmt.Fprintf(w, "\nAccuracy is the LoCoMo convention: correct 1, partial 0.5, wrong 0, averaged over GRADED "+
		"answers. Unparsed verdicts are excluded from accuracy and counted separately — grading a judge "+
		"malfunction as a memory miss would understate the system under test. NOT_FOUND is the answerer's "+
		"abstention rate; every category here is answerable, so an abstention is scored wrong.\n")
	if len(r.Notes) > 0 {
		fmt.Fprintf(w, "\n## Notes\n\n")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "- %s\n", n)
		}
	}
}

// Write persists the answer report.
func (r AnswerReport) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "answer-report.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	var md strings.Builder
	r.Matrix(&md)
	return os.WriteFile(filepath.Join(dir, "answer-matrix.md"), []byte(md.String()), 0o644)
}
