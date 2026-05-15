// Package report aggregates per-(model, case) results into the
// capability matrix and renders it in three formats: markdown for
// humans, json for tooling, csv for spreadsheet drop-in.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/bench/internal/grader"
)

// CaseOutcome is one (provider, model, tier, case) row.
type CaseOutcome struct {
	Provider   string        `json:"provider"`
	Model      string        `json:"model"`
	Tier       string        `json:"tier"`
	CaseID     string        `json:"case_id"`
	Status     string        `json:"status"` // run status: "completed" | "failed" | "cancelled"
	Result     grader.Result `json:"result"`
	CostUSD    float64       `json:"cost_usd"`
	DurationMS int64         `json:"duration_ms"`
	Error      string        `json:"error,omitempty"`
}

// ModelSummary is the rolled-up verdict for one (provider, model)
// across all cases. The Verdict field is the operator-facing
// CAPABLE / MARGINAL / FAIL classification.
type ModelSummary struct {
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	Verdict        string  `json:"verdict"` // "CAPABLE" | "MARGINAL" | "FAIL" | "INCONCLUSIVE"
	CasesTotal     int     `json:"cases_total"`
	StructuralPass int     `json:"structural_pass"`
	FunctionalPass int     `json:"functional_pass"`
	SemanticAvg    float64 `json:"semantic_avg"` // 0..1
	OverallPass    int     `json:"overall_pass"`
	CostUSD        float64 `json:"cost_usd"`
	// CostPerPassUSD is CostUSD / OverallPass. Set to -1 when
	// OverallPass = 0 (avoids JSON serialising +Inf, which breaks
	// most consumers). Renderers print "n/a" for the negative
	// sentinel. Lets the operator pick the cheapest-per-success
	// model when multiple candidates are CAPABLE.
	CostPerPassUSD float64 `json:"cost_per_pass_usd"`
	DurationMS     int64   `json:"duration_ms"`
	Notes          string  `json:"notes,omitempty"`
}

// Matrix is the top-level report shape. Holds per-case outcomes and
// per-model summaries side by side so consumers can either inspect
// individual cases or skip to the bottom-line verdict.
type Matrix struct {
	GeneratedAt  time.Time      `json:"generated_at"`
	BenchVersion string         `json:"bench_version"`
	Outcomes     []CaseOutcome  `json:"outcomes"`
	Summaries    []ModelSummary `json:"summaries"`
}

// Verdict thresholds (per the plan):
//
//	CAPABLE  : ≥80% structural pass AND ≥80% functional pass AND ≥0.70 average semantic
//	FAIL     : <50% on any axis
//	MARGINAL : everything else
//	INCONCLUSIVE: any case run failed with a transport error (network, timeout)
//	             before it could be graded — operator decides whether
//	             to re-run.
const (
	verdictCapable      = "CAPABLE"
	verdictMarginal     = "MARGINAL"
	verdictFail         = "FAIL"
	verdictInconclusive = "INCONCLUSIVE"
)

// Build aggregates outcomes into a Matrix with per-model summaries.
func Build(outcomes []CaseOutcome, benchVersion string) Matrix {
	byModel := map[string][]CaseOutcome{}
	keys := []string{}
	for _, o := range outcomes {
		k := o.Provider + "::" + o.Model
		if _, ok := byModel[k]; !ok {
			keys = append(keys, k)
		}
		byModel[k] = append(byModel[k], o)
	}
	sort.Strings(keys)

	summaries := make([]ModelSummary, 0, len(keys))
	for _, k := range keys {
		summaries = append(summaries, summarize(byModel[k]))
	}
	return Matrix{
		GeneratedAt:  time.Now().UTC(),
		BenchVersion: benchVersion,
		Outcomes:     outcomes,
		Summaries:    summaries,
	}
}

func summarize(rows []CaseOutcome) ModelSummary {
	s := ModelSummary{
		Provider:   rows[0].Provider,
		Model:      rows[0].Model,
		CasesTotal: len(rows),
	}
	var semSum float64
	var semDenom int
	for _, r := range rows {
		// Inconclusive trumps everything: any transport failure means
		// the operator doesn't have enough evidence on this model.
		if r.Status != "completed" {
			s.Verdict = verdictInconclusive
		}
		if r.Result.Structural.Pass {
			s.StructuralPass++
		}
		if r.Result.Functional.Pass {
			s.FunctionalPass++
		}
		semSum += r.Result.Semantic.Score
		semDenom++
		if r.Result.Passed() {
			s.OverallPass++
		}
		s.CostUSD += r.CostUSD
		s.DurationMS += r.DurationMS
	}
	if semDenom > 0 {
		s.SemanticAvg = semSum / float64(semDenom)
	}
	if s.Verdict == "" {
		structRatio := float64(s.StructuralPass) / float64(s.CasesTotal)
		funcRatio := float64(s.FunctionalPass) / float64(s.CasesTotal)
		switch {
		case structRatio < 0.5 || funcRatio < 0.5 || s.SemanticAvg < 0.5:
			s.Verdict = verdictFail
		case structRatio >= 0.8 && funcRatio >= 0.8 && s.SemanticAvg >= 0.7:
			s.Verdict = verdictCapable
		default:
			s.Verdict = verdictMarginal
		}
	}
	if s.OverallPass > 0 {
		s.CostPerPassUSD = s.CostUSD / float64(s.OverallPass)
	} else {
		s.CostPerPassUSD = -1 // sentinel for n/a — see CostPerPassUSD field doc
	}
	return s
}

// WriteAll writes matrix.md, matrix.json, matrix.csv into dir.
// Returns the first error encountered.
func WriteAll(m Matrix, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "matrix.md"), func(w io.Writer) error { return WriteMarkdown(w, m) }); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "matrix.json"), func(w io.Writer) error { return WriteJSON(w, m) }); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "matrix.csv"), func(w io.Writer) error { return WriteCSV(w, m) }); err != nil {
		return err
	}
	return nil
}

func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return write(f)
}

// WriteMarkdown renders the matrix as a human-friendly markdown
// document — a per-model verdict table at the top and per-case rows
// beneath it.
func WriteMarkdown(w io.Writer, m Matrix) error {
	fmt.Fprintf(w, "# Capability matrix — %s\n\n", m.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "Bench version: `%s`\n\n", m.BenchVersion)

	fmt.Fprintln(w, "## Verdicts")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "| Provider | Model | Verdict | Struct% | Func% | Sem avg | Overall | Cost (USD) | $/pass | Avg s/case |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|")
	for _, s := range m.Summaries {
		avgSecPerCase := 0.0
		if s.CasesTotal > 0 {
			avgSecPerCase = float64(s.DurationMS) / float64(s.CasesTotal) / 1000.0
		}
		fmt.Fprintf(w, "| %s | `%s` | **%s** | %d/%d | %d/%d | %.2f | %d/%d | $%.4f | %s | %.1f |\n",
			s.Provider, s.Model, s.Verdict,
			s.StructuralPass, s.CasesTotal,
			s.FunctionalPass, s.CasesTotal,
			s.SemanticAvg,
			s.OverallPass, s.CasesTotal,
			s.CostUSD,
			formatCostPerPass(s.CostPerPassUSD),
			avgSecPerCase,
		)
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Verdict legend")
	fmt.Fprintln(w, "- **CAPABLE**: ≥80% structural pass AND ≥80% functional pass AND ≥0.70 average semantic.")
	fmt.Fprintln(w, "- **MARGINAL**: between FAIL and CAPABLE — operator decides per-tier.")
	fmt.Fprintln(w, "- **FAIL**: <50% on at least one axis.")
	fmt.Fprintln(w, "- **INCONCLUSIVE**: a case run errored at transport level (network, timeout); insufficient evidence.")

	writeSpeedRanking(w, m)

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Per-case outcomes")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "| Provider | Model | Case | Status | S | F | Sem | Time (s) | Reasons |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|")
	for _, o := range m.Outcomes {
		fmt.Fprintf(w, "| %s | `%s` | `%s` | %s | %s | %s | %.2f | %.1f | %s |\n",
			o.Provider, o.Model, o.CaseID, o.Status,
			passMark(o.Result.Structural.Pass),
			passMark(o.Result.Functional.Pass),
			o.Result.Semantic.Score,
			float64(o.DurationMS)/1000.0,
			joinReasons(o.Result),
		)
	}
	return nil
}

// writeSpeedRanking renders a speed-ranked section: per-model average
// seconds per case, sorted fastest-first. Useful for picking between
// two models that have the same verdict (e.g., two MARGINALs) when
// throughput matters. Local Ollama on a single GPU is the dramatic
// case — its inference rate can be 5-20× slower than a cloud Pro
// model, which can flip a "cost-floor candidate" verdict into "too
// slow for production load".
func writeSpeedRanking(w io.Writer, m Matrix) {
	if len(m.Summaries) <= 1 {
		return
	}
	rows := make([]costPerPassRow, 0, len(m.Summaries))
	for _, s := range m.Summaries {
		avg := 0.0
		if s.CasesTotal > 0 {
			avg = float64(s.DurationMS) / float64(s.CasesTotal) / 1000.0
		}
		rows = append(rows, costPerPassRow{s: s, avgSecPerCase: avg})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].avgSecPerCase < rows[j].avgSecPerCase
	})
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Speed ranking (fastest first)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "| Rank | Provider | Model | Avg s/case | Total s | Cost (USD) | $/pass | Verdict |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|")
	for i, r := range rows {
		fmt.Fprintf(w, "| %d | %s | `%s` | %.2f | %.1f | $%.4f | %s | %s |\n",
			i+1, r.s.Provider, r.s.Model,
			r.avgSecPerCase,
			float64(r.s.DurationMS)/1000.0,
			r.s.CostUSD,
			formatCostPerPass(r.s.CostPerPassUSD),
			r.s.Verdict,
		)
	}
	writeCostPerPassRanking(w, rows)
}

// writeCostPerPassRanking emits a separate section ranked by
// cost-per-pass. The speed-ranking and cost-per-pass-ranking tables
// share row data; we shouldn't conflate them in a single sort.
// Cost-per-pass is the cleanest signal for "cheapest model that
// produces a working result on this battery".
func writeCostPerPassRanking(w io.Writer, rows []costPerPassRow) {
	type costRow struct {
		s      ModelSummary
		costPP float64
		avgSec float64
	}
	cr := make([]costRow, 0, len(rows))
	for _, r := range rows {
		// Skip models that scored 0 overall pass — they have no
		// pass to compute cost-per-pass for, and would dominate the
		// "cheapest" end of the table with $0 noise.
		if r.s.OverallPass == 0 {
			continue
		}
		cr = append(cr, costRow{s: r.s, costPP: r.s.CostPerPassUSD, avgSec: r.avgSecPerCase})
	}
	if len(cr) <= 1 {
		return
	}
	sort.Slice(cr, func(i, j int) bool {
		return cr[i].costPP < cr[j].costPP
	})
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "## Cost-per-pass ranking (cheapest passing first)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Models with zero overall-pass excluded (n/a cost-per-pass).")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "| Rank | Provider | Model | $/pass | Overall | Total cost | Avg s/case | Verdict |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|")
	for i, r := range cr {
		fmt.Fprintf(w, "| %d | %s | `%s` | %s | %d/%d | $%.4f | %.2f | %s |\n",
			i+1, r.s.Provider, r.s.Model,
			formatCostPerPass(r.costPP),
			r.s.OverallPass, r.s.CasesTotal,
			r.s.CostUSD,
			r.avgSec,
			r.s.Verdict,
		)
	}
}

// costPerPassRow is the shared row type between speed + cost-per-pass
// rankings — pre-computed avg-seconds-per-case avoids recomputing it
// in both renderers.
type costPerPassRow struct {
	s             ModelSummary
	avgSecPerCase float64
}

func passMark(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

// formatCostPerPass renders the cost-per-pass column. Negative sentinel
// (set when overall_pass = 0) renders as "n/a" — avoids the misleading
// $0.0000 a naive zero-division-suppressing format would produce.
func formatCostPerPass(v float64) string {
	if v < 0 {
		return "n/a"
	}
	return fmt.Sprintf("$%.4f", v)
}

func joinReasons(r grader.Result) string {
	all := append([]string{}, r.Structural.Reasons...)
	all = append(all, r.Functional.Reasons...)
	all = append(all, r.Semantic.Reasons...)
	out := strings.Join(all, "; ")
	if len(out) > 200 {
		out = out[:200] + "…"
	}
	// Escape pipe characters so the markdown table doesn't break.
	return strings.ReplaceAll(out, "|", "\\|")
}

// WriteJSON dumps the full matrix as pretty-printed JSON.
func WriteJSON(w io.Writer, m Matrix) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// WriteCSV writes one row per (provider, model, case).
func WriteCSV(w io.Writer, m Matrix) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{
		"provider", "model", "tier", "case_id", "status",
		"structural_pass", "functional_pass", "semantic_score",
		"cost_usd", "duration_ms", "reasons",
	}); err != nil {
		return err
	}
	for _, o := range m.Outcomes {
		if err := cw.Write([]string{
			o.Provider, o.Model, o.Tier, o.CaseID, o.Status,
			fmt.Sprintf("%t", o.Result.Structural.Pass),
			fmt.Sprintf("%t", o.Result.Functional.Pass),
			fmt.Sprintf("%.2f", o.Result.Semantic.Score),
			fmt.Sprintf("%.4f", o.CostUSD),
			fmt.Sprintf("%d", o.DurationMS),
			joinReasons(o.Result),
		}); err != nil {
			return err
		}
	}
	return nil
}
