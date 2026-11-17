package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Render writes the readable report. Every number in it is also in the JSON.
func Render(r *Result) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("# RFC GM P0 measurement\n\n")
	if len(r.Notes) > 0 {
		p("## Notes\n\n")
		for _, n := range r.Notes {
			p("- %s\n", n)
		}
		p("\n")
	}
	renderCounts(p, r)
	renderQuality(p, r)
	renderAgreement(p, r)
	renderMaterial(p, r)
	renderSplits(p, r)
	renderDates(p, r)
	renderMisfits(p, r)
	return b.String()
}

type printer func(format string, a ...any)

func renderCounts(p printer, r *Result) {
	p("## Data read\n\n| what | count |\n|---|---:|\n")
	keys := make([]string, 0, len(r.Counts))
	for k := range r.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p("| %s | %d |\n", k, r.Counts[k])
	}
	p("\nNode sizes (documents on a concept or below it):\n\n| members | concepts |\n|---|---:|\n")
	for _, h := range r.SizeHisto {
		p("| %s | %d |\n", h.Range, h.Concepts)
	}
	p("\n")
}

func renderQuality(p printer, r *Result) {
	q := r.Quality
	p("## 0. Data quality\n\n")
	p("Current prompt version %d. A node is not gradable when more than %.0f%% of its members have no document annotation of the current prompt.\n\n",
		r.Options.CurrentPromptVersion, r.Options.NonCurrentLimit*100)
	p("| node | depth | members | with doc vector | without | no annotation | older prompt | not current | gradable |\n|---|---:|---:|---:|---:|---:|---:|---:|---|\n")
	row := func(n NodeQuality) {
		p("| %s | %d | %d | %d | %d | %d | %d | %.0f%% | %s |\n", n.Name, n.Depth, n.Members, n.WithDocVec,
			n.WithoutDocVec, n.NoAnnotation, n.NonCurrent, n.NonCurrentShare*100, yesNo(n.Gradable))
	}
	row(q.Overall)
	for _, n := range q.Nodes {
		row(n)
	}
	p("\n")
}

func renderAgreement(p printer, r *Result) {
	a := r.Agreement
	p("## 1. Agreement with the declaration (leave-one-out)\n\n")
	p("Arms: **a** every statement counts fully, unit = document vector; **b** unit = each section vector, inheriting its document's concepts (its whole document left out); **c** a document weighs 1/k in each of its k classes at the level. ")
	p("L2 pools every parent with two or more children. Only documents with a document vector take part in every arm.\n\n")
	p("Statement kinds read: %s\n\n", kv(a.Kinds))
	p("| level | arm | units | evaluable | top-1 | top-3 |\n|---|---|---:|---:|---:|---:|\n")
	for _, key := range []string{"L1", "L2"} {
		for _, arm := range arms {
			rt := a.Summary[key][arm]
			p("| %s | %s | %d | %d | %s | %s |\n", key, arm, rt.Units, rt.Evaluable, pct(rt.Top1Rate), pct(rt.Top3Rate))
		}
		if pm, ok := a.Permutation[key]; ok {
			p("| %s | labels permuted (arm a, %d perms) mean / p95 | | | %s / %s | %s / %s |\n", key, pm.N,
				pct(pm.Top1Mean), pct(pm.Top1P95), pct(pm.Top3Mean), pct(pm.Top3P95))
		}
		if d, ok := a.Definition[key]; ok {
			p("| %s | definition only | %d | %d | %s | %s |\n", key, d.Units, d.Evaluable, pct(d.Top1Rate), pct(d.Top3Rate))
		}
	}
	p("\n%s\n\n", a.DefNote)

	p("Per level (arm a / b / c, top-1). At a level of three classes or fewer top-3 is always a hit; read L2 top-3 with that in mind.\n\n")
	p("| level | classes | evaluable (a) | a | b | c |\n|---|---:|---:|---:|---:|---:|\n")
	names := make([]string, 0, len(a.ByLevel))
	for n := range a.ByLevel {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := a.ByLevel[n]
		p("| %s | %d | %d | %s | %s | %s |\n", n, a.Classes[n], l[ArmFull].Evaluable, pct(l[ArmFull].Top1Rate),
			pct(l[ArmSections].Top1Rate), pct(l[ArmWeighted].Top1Rate))
	}
	p("\nBy provenance (arm a, per statement: is the statement's own class the nearest / in the top 3):\n\n")
	p("| provenance | L1 n | L1 top-1 | L1 top-3 | L2 n | L2 top-1 | L2 top-3 |\n|---|---:|---:|---:|---:|---:|---:|\n")
	provs := make([]string, 0, len(a.Provenance))
	for k := range a.Provenance {
		provs = append(provs, k)
	}
	sort.Strings(provs)
	for _, k := range provs {
		l1, l2 := a.Provenance[k]["L1"], a.Provenance[k]["L2"]
		p("| %s | %d | %s | %s | %d | %s | %s |\n", k, l1.Evaluable, pct(l1.Top1Rate), pct(l1.Top3Rate),
			l2.Evaluable, pct(l2.Top1Rate), pct(l2.Top3Rate))
	}
	p("\n")
}

func renderMaterial(p printer, r *Result) {
	p("## 4. Grading material\n\nOne row per concept; grade each. Spread is 1 - |mean of unit vectors|.\n\n")
	p("| concept | n | spread | exemplars | nearest siblings | distinctive terms | grade |\n|---|---:|---:|---|---|---|---|\n")
	for _, n := range r.Nodes {
		sp := ""
		if n.Spread != nil {
			sp = fmt.Sprintf("%.3f", math.Max(0, *n.Spread)) // a one-member node reads -0.000
		}
		var ex []string
		for _, e := range n.Exemplars {
			s := "**" + cell(e.Title) + "**"
			if e.Brief != "" {
				s += ": " + cell(e.Brief)
			}
			ex = append(ex, s)
		}
		var sib []string
		for _, s := range n.Siblings {
			sib = append(sib, fmt.Sprintf("%s (%.2f)", s.Name, s.Similarity))
		}
		p("| %s | %d | %s | %s | %s | %s | |\n", n.Path, n.Count, sp, strings.Join(ex, "<br>"),
			strings.Join(sib, ", "), strings.Join(n.Terms, ", "))
	}
	p("\n")
}

func renderSplit(p printer, s SplitSummary) {
	p("### %s (%d members)\n\n", s.Name, s.Members)
	if len(s.Runs) == 0 {
		p("Below the split size; not clustered.\n\n")
		return
	}
	ks := make([]string, 0, len(s.KCounts))
	for k, n := range s.KCounts {
		ks = append(ks, fmt.Sprintf("k=%d x%d", k, n))
	}
	sort.Strings(ks)
	p("Chosen k over seeds: %s. Mean silhouette %.3f, mean stability %.3f.\n\n", strings.Join(ks, ", "), s.MeanSilhouette, s.MeanStability)
	p("| seed | k | silhouette | stability | sizes | dropped |\n|---:|---:|---:|---:|---|---:|\n")
	for _, run := range s.Runs {
		p("| %d | %d | %.3f | %.3f | %v | %d |\n", run.Seed, run.K, run.Silhouette, run.Stability, run.Sizes, run.Dropped)
	}
	p("\nGroups (seed 1):\n\n| size | terms | titles |\n|---:|---|---|\n")
	for _, g := range s.Groups {
		p("| %d | %s | %s |\n", g.Size, strings.Join(g.Terms, ", "), cell(strings.Join(g.Titles, " · ")))
	}
	p("\n")
}

func renderSplits(p printer, r *Result) {
	p("## 5. Split candidates and unplaced documents\n\n")
	for _, s := range r.Splits {
		renderSplit(p, s)
	}
	if r.Unplaced != nil {
		p("Unplaced: content documents with a document vector and no live statement into the scheme; %d of them have only statements that are not live (proposed, rejected, withheld or retired).\n\n",
			r.Unplaced.WithNotLive)
		renderSplit(p, *r.Unplaced)
	}
}

func renderDates(p printer, r *Result) {
	d := r.Dates
	p("## 6. Dates\n\nSource of each document's date (root valid_at, else import source_updated_at, else created_at; a created_at on or before the bulk import day %s counts as the import's):\n\n", importDay(r.Options.ImportedUntil))
	p("| source | all documents | placed documents |\n|---|---:|---:|\n")
	for _, s := range []string{DateValidAt, DateSourceUpd, DateCreatedAt, DateImport} {
		p("| %s | %d | %d |\n", s, d.All[s], d.Placed[s])
	}
	p("\nMonths count only members not dated by the import.\n\n| concept | members | not import-dated | months | months with >= %d | first | last |\n|---|---:|---:|---:|---:|---|---|\n", MinPeriodMembers)
	for _, n := range d.Nodes {
		p("| %s | %d | %d | %d | %d | %s | %s |\n", n.Name, n.Members, n.Dated, n.Periods, n.Usable, n.First, n.Last)
	}
	p("\n")
}

func importDay(t time.Time) string {
	if t.IsZero() {
		return "(none given)"
	}
	return t.UTC().Format("2006-01-02")
}

func renderMisfits(p printer, r *Result) {
	p("## Misfits\n\n| set | margin | count |\n|---|---:|---:|\n")
	for _, m := range r.Misfits {
		p("| %s | %.2f | %d |\n", m.Set, m.Margin, m.Count)
	}
	for _, m := range r.Misfits {
		if m.Margin != 0 || len(m.Examples) == 0 {
			continue
		}
		p("\nTop examples, %s, margin 0 (largest gap first):\n\n| document | in | own (LOO) | nearer | its sim |\n|---|---|---:|---|---:|\n", m.Set)
		for _, e := range m.Examples {
			p("| %s | %s | %.3f | %s | %.3f |\n", cell(e.Title), e.Node, e.Own, e.Nearest, e.NearSim)
		}
	}
	p("\n")
}

func pct(f float64) string { return fmt.Sprintf("%.1f%%", f*100) }

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// cell keeps a value on one table row.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}

func kv(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
