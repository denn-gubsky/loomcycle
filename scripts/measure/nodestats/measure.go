package main

import (
	"fmt"
	"sort"
	"time"
)

// Arms of RFC GM decision D3, as measured here.
const (
	ArmFull     = "a_full"     // every subject statement counts fully; unit = document vector
	ArmSections = "b_sections" // unit = each section vector, inheriting its document's concepts
	ArmWeighted = "c_weighted" // a document weighs 1/k in each of its k classes at the level
)

var arms = []string{ArmFull, ArmSections, ArmWeighted}

// Options are the measurement knobs.
type Options struct {
	CurrentPromptVersion int
	Permutations         int
	SplitSeeds           int
	MisfitMargins        []float64
	// NonCurrentLimit: a node with a larger share of non-current members is not
	// gradable.
	NonCurrentLimit float64
	// ImportedUntil is the last day of the corpus's bulk import; a document
	// created on or before it is dated by its import. Zero: no import day.
	ImportedUntil time.Time `json:",omitempty"`
}

// Result is the whole measurement, written as JSON and rendered as Markdown.
type Result struct {
	Options   Options         `json:"options"`
	Notes     []string        `json:"notes"`
	Counts    map[string]int  `json:"counts"`
	Quality   QualityReport   `json:"quality"`
	Agreement AgreementReport `json:"agreement"`
	Nodes     []NodeMaterial  `json:"nodes"`
	Splits    []SplitSummary  `json:"splits"`
	Unplaced  *SplitSummary   `json:"unplaced,omitempty"`
	Dates     DateReport      `json:"dates"`
	Misfits   []MisfitReport  `json:"misfits"`
	SizeHisto []HistoBin      `json:"node_size_histogram"`
}

// Measure runs every measurement over d. defVecs are the concepts' definition
// embeddings (nil when the embedder was not reachable; defNote says why).
func Measure(d *Data, opt Options, defVecs map[string][]float64, defNote string) (*Result, error) {
	tax, err := NewTaxonomy(d.Concepts)
	if err != nil {
		return nil, err
	}
	placed := d.Placed()
	members := tax.Members(placed)
	res := &Result{Options: opt, Notes: append([]string(nil), d.Notes...), Counts: d.Counts}
	res.Quality = Quality(tax, d.Docs, members, opt)
	res.Agreement, err = MeasureAgreement(tax, d, placed, opt, defVecs, defNote)
	if err != nil {
		return nil, err
	}
	if res.Nodes, err = GradingMaterial(tax, d.Docs, members); err != nil {
		return nil, err
	}
	if res.Splits, err = NodeSplits(tax, d.Docs, members, opt.SplitSeeds); err != nil {
		return nil, err
	}
	if res.Unplaced, err = UnplacedSplit(d.Docs, placed, d.NotLiveDocs, opt.SplitSeeds); err != nil {
		return nil, err
	}
	res.Dates = Dates(tax, d.Docs, placed, members, opt.ImportedUntil)
	if res.Misfits, err = MeasureMisfits(tax, d.Docs, members, opt.MisfitMargins); err != nil {
		return nil, err
	}
	res.SizeHisto = SizeHistogram(tax, members)
	return res, nil
}

// QualityReport is measurement 0.
type QualityReport struct {
	Overall NodeQuality   `json:"overall"`
	Nodes   []NodeQuality `json:"nodes"`
}

// NodeQuality counts one node's members (documents placed on it or below).
// NonCurrentShare counts a member with no document-level annotation as not
// current: it has no annotation of the current prompt.
type NodeQuality struct {
	ID              string  `json:"id,omitempty"`
	Name            string  `json:"name"`
	Depth           int     `json:"depth,omitempty"`
	Members         int     `json:"members"`
	WithDocVec      int     `json:"with_doc_vector"`
	WithoutDocVec   int     `json:"without_doc_vector"`
	NoAnnotation    int     `json:"no_annotation"`
	NonCurrent      int     `json:"non_current_prompt"`
	NonCurrentShare float64 `json:"non_current_share"`
	Gradable        bool    `json:"gradable"`
}

func Quality(tax *Taxonomy, docs map[string]*Doc, members map[string][]string, opt Options) QualityReport {
	q := func(name string, ids []string) NodeQuality {
		n := NodeQuality{Name: name, Members: len(ids)}
		for _, id := range ids {
			d := docs[id]
			if d == nil {
				continue
			}
			if d.DocVec != nil {
				n.WithDocVec++
			} else {
				n.WithoutDocVec++
			}
			switch {
			case !d.HasAnnotation:
				n.NoAnnotation++
			case d.PromptVersion != opt.CurrentPromptVersion:
				n.NonCurrent++
			}
		}
		if n.Members > 0 {
			n.NonCurrentShare = float64(n.NonCurrent+n.NoAnnotation) / float64(n.Members)
		}
		n.Gradable = n.Members > 0 && n.NonCurrentShare <= opt.NonCurrentLimit
		return n
	}
	var r QualityReport
	all := map[string]bool{}
	for _, id := range tax.Order() {
		c := tax.Get(id)
		nq := q(c.Name, members[id])
		nq.ID, nq.Depth = id, c.Depth
		r.Nodes = append(r.Nodes, nq)
		for _, m := range members[id] {
			all[m] = true
		}
	}
	r.Overall = q("all placed documents", sortedKeys(all))
	return r
}

// AgreementReport is measurement 1.
type AgreementReport struct {
	// ByLevel: level name -> arm -> rates.
	ByLevel map[string]map[string]Rates `json:"by_level"`
	// Summary: "L1" / "L2" (all L2 levels pooled) -> arm -> rates.
	Summary     map[string]map[string]Rates   `json:"summary"`
	Permutation map[string]PermutationSummary `json:"permutation_baseline"`
	Definition  map[string]Rates              `json:"definition_baseline,omitempty"`
	DefNote     string                        `json:"definition_baseline_note,omitempty"`
	Provenance  map[string]map[string]Rates   `json:"by_provenance"`
	Kinds       map[string]int                `json:"statement_kinds"`
	// Classes is each level's number of classes: top-3 is no test at a level
	// of three classes.
	Classes map[string]int `json:"level_classes"`
}

func levelKey(lv Level) string {
	if lv.Parent == "" {
		return "L1"
	}
	return "L2"
}

// buildInput assembles one level of one arm. Only documents with a document
// vector take part, in every arm, so the arms measure the same documents.
func buildInput(lv Level, docs map[string]*Doc, arm string) AgreementInput {
	in := AgreementInput{Classes: lv.Classes, Weights: map[string]map[string]float64{}}
	ids := make([]string, 0, len(lv.Labels))
	for id := range lv.Labels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := docs[id]
		if d == nil || d.DocUnit == nil {
			continue
		}
		labels := lv.Labels[id]
		w := map[string]float64{}
		for _, c := range labels {
			w[c] = 1
			if arm == ArmWeighted {
				w[c] = 1 / float64(len(labels))
			}
		}
		if arm == ArmSections {
			if len(d.SecUnits) == 0 {
				continue
			}
			for _, u := range d.SecUnits {
				in.Units = append(in.Units, Unit{Group: id, Vec: u})
			}
		} else {
			in.Units = append(in.Units, Unit{Group: id, Vec: d.DocUnit})
		}
		in.Weights[id] = w
	}
	return in
}

// MeasureAgreement runs the three arms on every level, the permutation baseline
// (arm a, opt.Permutations shuffles), the definition baseline, and arm a's
// statement-level agreement split by provenance.
func MeasureAgreement(tax *Taxonomy, d *Data, placed map[string][]string, opt Options,
	defVecs map[string][]float64, defNote string) (AgreementReport, error) {
	r := AgreementReport{ByLevel: map[string]map[string]Rates{}, Summary: map[string]map[string]Rates{},
		Permutation: map[string]PermutationSummary{}, Provenance: map[string]map[string]Rates{},
		Kinds: map[string]int{}, Classes: map[string]int{}, DefNote: defNote}
	for _, s := range d.Statements {
		r.Kinds[s.Kind]++
	}
	if defVecs != nil {
		r.Definition = map[string]Rates{}
	}
	permTop1 := map[string][][2]int{} // L1/L2 -> per permutation (top1, evaluable) pooled
	permTop3 := map[string][][2]int{}
	for _, lv := range tax.Levels(placed) {
		key := levelKey(lv)
		r.ByLevel[lv.Name] = map[string]Rates{}
		r.Classes[lv.Name] = len(lv.Classes)
		if r.Summary[key] == nil {
			r.Summary[key] = map[string]Rates{}
		}
		for _, arm := range arms {
			in := buildInput(lv, d.Docs, arm)
			ranks, rates, err := Agreement(in)
			if err != nil {
				return r, fmt.Errorf("%s %s: %w", lv.Name, arm, err)
			}
			r.ByLevel[lv.Name][arm] = rates
			sum := r.Summary[key][arm]
			sum.Add(rates)
			r.Summary[key][arm] = sum
			if arm == ArmFull {
				addProvenance(r.Provenance, key, tax, lv, ranks, d.Statements)
			}
		}
		in := buildInput(lv, d.Docs, ArmFull)
		if defVecs != nil {
			fr := FixedAgreement(in, defVecs)
			sum := r.Definition[key]
			sum.Add(fr)
			r.Definition[key] = sum
		}
		for p := 0; p < opt.Permutations; p++ {
			pin := in
			pin.Weights = Permute(in.Weights, uint64(p+1))
			_, pr, err := Agreement(pin)
			if err != nil {
				return r, err
			}
			if len(permTop1[key]) <= p {
				permTop1[key] = append(permTop1[key], [2]int{})
				permTop3[key] = append(permTop3[key], [2]int{})
			}
			permTop1[key][p][0] += pr.Top1
			permTop1[key][p][1] += pr.Evaluable
			permTop3[key][p][0] += pr.Top3
			permTop3[key][p][1] += pr.Evaluable
		}
	}
	for key := range permTop1 {
		var t1, t3 []float64
		for p := range permTop1[key] {
			t1 = append(t1, ratio(permTop1[key][p]))
			t3 = append(t3, ratio(permTop3[key][p]))
		}
		r.Permutation[key] = PermutationSummary{N: len(t1), Top1Mean: mean(t1), Top1P95: Percentile(t1, 95),
			Top3Mean: mean(t3), Top3P95: Percentile(t3, 95)}
	}
	return r, nil
}

// addProvenance scores each statement on its own: does the document's nearest
// class (top 1) or top 3 at this level hold the class the STATEMENT names? Only
// documents evaluable at the level count, and only when that class was a
// candidate.
func addProvenance(out map[string]map[string]Rates, key string, tax *Taxonomy, lv Level,
	ranks []UnitRank, stmts []Statement) {
	byDoc := map[string]UnitRank{}
	for _, u := range ranks {
		if u.Evaluable {
			byDoc[u.Group] = u
		}
	}
	depth := 0
	if lv.Parent != "" {
		depth = tax.Get(lv.Parent).Depth
	}
	for _, s := range stmts {
		u, ok := byDoc[s.DocID]
		if !ok {
			continue
		}
		path := tax.Path(s.ConceptID)
		if depth >= len(path) || (lv.Parent != "" && path[depth-1] != lv.Parent) {
			continue
		}
		class := path[depth]
		pos := indexOf(u.Ranked, class)
		if pos < 0 {
			continue
		}
		if out[s.Provenance] == nil {
			out[s.Provenance] = map[string]Rates{}
		}
		rt := out[s.Provenance][key]
		rt.Units++
		rt.Evaluable++
		if pos == 0 {
			rt.Top1++
		}
		if pos < 3 {
			rt.Top3++
		}
		rt.finish()
		out[s.Provenance][key] = rt
	}
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func ratio(p [2]int) float64 {
	if p[1] == 0 {
		return 0
	}
	return float64(p[0]) / float64(p[1])
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// HistoBin counts concepts by member count.
type HistoBin struct {
	Range    string `json:"range"`
	Concepts int    `json:"concepts"`
}

var histoEdges = []int{0, 1, 3, 5, 10, 20, 50, 100}

// SizeHistogram bins every concept by its member count (documents on or below).
func SizeHistogram(tax *Taxonomy, members map[string][]string) []HistoBin {
	bins := make([]HistoBin, len(histoEdges))
	for i, lo := range histoEdges {
		if i+1 < len(histoEdges) {
			bins[i].Range = fmt.Sprintf("%d-%d", lo, histoEdges[i+1]-1)
		} else {
			bins[i].Range = fmt.Sprintf("%d+", lo)
		}
	}
	for _, id := range tax.Order() {
		n := len(members[id])
		for i := len(histoEdges) - 1; i >= 0; i-- {
			if n >= histoEdges[i] {
				bins[i].Concepts++
				break
			}
		}
	}
	return bins
}
