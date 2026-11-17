package main

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
)

// NodeMaterial is measurement 4: what the owner grades a node by.
type NodeMaterial struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Depth     int        `json:"depth"`
	Count     int        `json:"count"` // members with a document vector
	Spread    *float64   `json:"spread,omitempty"`
	Exemplars []Exemplar `json:"exemplars,omitempty"`
	Siblings  []Near     `json:"nearest_siblings,omitempty"`
	Terms     []string   `json:"distinctive_terms,omitempty"`
}

// Exemplar is a document near a centroid.
type Exemplar struct {
	DocID      string  `json:"doc_id"`
	Title      string  `json:"title"`
	Brief      string  `json:"brief,omitempty"`
	Similarity float64 `json:"similarity"`
}

// Near is a node near another, by centroid cosine.
type Near struct {
	Name       string  `json:"name"`
	Similarity float64 `json:"similarity"`
}

// vectorMembers are the members with a document vector, as nodestats members.
func vectorMembers(docs map[string]*Doc, ids []string) []nodestats.Member {
	var out []nodestats.Member
	for _, id := range ids {
		if d := docs[id]; d != nil && d.DocVec != nil {
			out = append(out, nodestats.Member{ID: id, Vec: d.DocVec})
		}
	}
	return out
}

// texts are the members' document-level briefs and abstracts.
func texts(docs map[string]*Doc, ids []string) []string {
	var out []string
	for _, id := range ids {
		if d := docs[id]; d != nil && d.HasAnnotation {
			out = append(out, d.Brief, d.Abstract)
		}
	}
	return out
}

func exemplars(docs map[string]*Doc, c nodestats.Centroid, ms []nodestats.Member) ([]Exemplar, error) {
	ex, err := nodestats.Exemplars(c, ms, nodestats.KExemplars)
	if err != nil {
		return nil, err
	}
	out := make([]Exemplar, len(ex))
	for i, e := range ex {
		d := docs[e.ID]
		out[i] = Exemplar{DocID: e.ID, Title: d.Title, Brief: d.Brief, Similarity: e.Score}
	}
	return out, nil
}

func centroid(ms []nodestats.Member) (nodestats.Centroid, error) {
	vs := make([]nodestats.Vector, len(ms))
	for i, m := range ms {
		vs[i] = m.Vec
	}
	return nodestats.CentroidOf(vs)
}

// GradingMaterial computes count, spread, exemplars, nearest siblings and
// distinctive terms for every concept. Siblings are the concepts under the
// same parent (the top level for a top-level concept).
func GradingMaterial(tax *Taxonomy, docs map[string]*Doc, members map[string][]string) ([]NodeMaterial, error) {
	cents := map[string]nodestats.Centroid{}
	for _, id := range tax.Order() {
		ms := vectorMembers(docs, members[id])
		if len(ms) == 0 {
			continue
		}
		c, err := centroid(ms)
		if errors.Is(err, nodestats.ErrDegenerate) {
			// Members that cancel out give no direction: the node is
			// reported without spread, exemplars or siblings, as Misfits
			// skips it, rather than failing the whole measurement.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("concept %s: %w", id, err)
		}
		cents[id] = c
	}
	var out []NodeMaterial
	for _, id := range tax.Order() {
		c := tax.Get(id)
		nm := NodeMaterial{ID: id, Name: c.Name, Depth: c.Depth, Path: pathName(tax, id)}
		ms := vectorMembers(docs, members[id])
		nm.Count = len(ms)
		if cen, ok := cents[id]; ok {
			sp := cen.Spread()
			nm.Spread = &sp
			ex, err := exemplars(docs, cen, ms)
			if err != nil {
				return nil, fmt.Errorf("concept %s exemplars: %w", id, err)
			}
			nm.Exemplars = ex
			var sibs []nodestats.NodeCentroid
			for _, s := range tax.Siblings(id) {
				if sc, ok := cents[s]; ok {
					sibs = append(sibs, nodestats.NodeCentroid{ID: s, Centroid: sc})
				}
			}
			near, err := nodestats.NearestSiblings(nodestats.NodeCentroid{ID: id, Centroid: cen}, sibs, nodestats.KSiblings)
			if err != nil {
				return nil, fmt.Errorf("concept %s siblings: %w", id, err)
			}
			for _, n := range near {
				nm.Siblings = append(nm.Siblings, Near{Name: tax.Get(n.ID).Name, Similarity: n.Score})
			}
		}
		var sibTexts [][]string
		for _, s := range tax.Siblings(id) {
			sibTexts = append(sibTexts, texts(docs, members[s]))
		}
		terms, err := nodestats.DistinctiveTerms(texts(docs, members[id]), sibTexts, nodestats.TopTerms)
		if err != nil {
			return nil, fmt.Errorf("concept %s terms: %w", id, err)
		}
		for _, t := range terms {
			nm.Terms = append(nm.Terms, t.Term)
		}
		out = append(out, nm)
	}
	return out, nil
}

func pathName(tax *Taxonomy, id string) string {
	s := ""
	for i, p := range tax.Path(id) {
		if i > 0 {
			s += "/"
		}
		s += tax.Get(p).Name
	}
	return s
}

// SplitSummary is measurement 5 for one member set: Split over seeds 1..n,
// and the groups of the first seed with their terms and exemplar titles.
type SplitSummary struct {
	Name    string `json:"name"`
	Members int    `json:"members"`
	// WithNotLive (unplaced only): members whose only statements into the
	// scheme are not live.
	WithNotLive    int          `json:"with_only_not_live_statements,omitempty"`
	Runs           []SplitRun   `json:"runs"`
	KCounts        map[int]int  `json:"k_counts"`
	MeanSilhouette float64      `json:"mean_silhouette"`
	MeanStability  float64      `json:"mean_stability"`
	Groups         []SplitGroup `json:"groups_seed1"`
}

// SplitRun is one seed's outcome.
type SplitRun struct {
	Seed       uint64  `json:"seed"`
	K          int     `json:"k"`
	Silhouette float64 `json:"silhouette"`
	Stability  float64 `json:"stability"`
	Sizes      []int   `json:"sizes"`
	Dropped    int     `json:"dropped"`
}

// SplitGroup is one proposed group.
type SplitGroup struct {
	Size   int      `json:"size"`
	Terms  []string `json:"terms"`
	Titles []string `json:"titles"`
}

// SplitSet runs Split on a member set. It returns nil for a set below
// nodestats.SplitMinMembers.
func SplitSet(name string, docs map[string]*Doc, ids []string, seeds int) (*SplitSummary, error) {
	ms := vectorMembers(docs, ids)
	if len(ms) < nodestats.SplitMinMembers {
		return nil, nil
	}
	s := &SplitSummary{Name: name, Members: len(ms), KCounts: map[int]int{}}
	var first nodestats.SplitResult
	var sil, stab []float64
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		res, err := nodestats.Split(ms, nodestats.DefaultSplitOptions(seed))
		if err != nil {
			return nil, fmt.Errorf("%s seed %d: %w", name, seed, err)
		}
		if seed == 1 {
			first = res
		}
		run := SplitRun{Seed: seed, K: res.K, Silhouette: res.Silhouette, Stability: res.Stability, Dropped: res.Dropped}
		for _, g := range res.Groups {
			run.Sizes = append(run.Sizes, len(g.Members))
		}
		s.Runs = append(s.Runs, run)
		s.KCounts[res.K]++
		sil = append(sil, res.Silhouette)
		stab = append(stab, res.Stability)
	}
	s.MeanSilhouette, s.MeanStability = mean(sil), mean(stab)
	groups, err := describeGroups(docs, first.Groups)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	s.Groups = groups
	return s, nil
}

func describeGroups(docs map[string]*Doc, gs []nodestats.Group) ([]SplitGroup, error) {
	var out []SplitGroup
	for i, g := range gs {
		var others [][]string
		for j, o := range gs {
			if j != i {
				others = append(others, texts(docs, o.Members))
			}
		}
		terms, err := nodestats.DistinctiveTerms(texts(docs, g.Members), others, nodestats.TopTerms)
		if err != nil {
			return nil, err
		}
		sg := SplitGroup{Size: len(g.Members)}
		for _, t := range terms {
			sg.Terms = append(sg.Terms, t.Term)
		}
		ex, err := exemplars(docs, g.Centroid, vectorMembers(docs, g.Members))
		if err != nil {
			return nil, err
		}
		for _, e := range ex {
			sg.Titles = append(sg.Titles, e.Title)
		}
		out = append(out, sg)
	}
	return out, nil
}

// NodeSplits runs SplitSet on every concept large enough.
func NodeSplits(tax *Taxonomy, docs map[string]*Doc, members map[string][]string, seeds int) ([]SplitSummary, error) {
	var out []SplitSummary
	for _, id := range tax.Order() {
		s, err := SplitSet(pathName(tax, id), docs, members[id], seeds)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

// Unplaced are the content documents with a document vector and no live
// statement into the scheme.
func Unplaced(docs map[string]*Doc, placed map[string][]string) []string {
	var out []string
	for id, d := range docs {
		if d.Ontology || d.DocVec == nil || len(placed[id]) > 0 {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// UnplacedSplit groups the unplaced documents as a node's members are grouped.
// Below the split size it still reports their count.
func UnplacedSplit(docs map[string]*Doc, placed map[string][]string, notLive map[string]bool, seeds int) (*SplitSummary, error) {
	ids := Unplaced(docs, placed)
	s, err := SplitSet("unplaced", docs, ids, seeds)
	if err != nil {
		return nil, err
	}
	if s == nil {
		s = &SplitSummary{Name: "unplaced", Members: len(ids)}
	}
	for _, id := range ids {
		if notLive[id] {
			s.WithNotLive++
		}
	}
	return s, nil
}

// Date sources, in the RFC's order of preference.
const (
	DateValidAt   = "valid_at"
	DateSourceUpd = "source_updated_at"
	DateCreatedAt = "created_at"
	// DateImport is a created_at on or before the corpus's bulk import: the
	// time the document was loaded, not when it was written, so it carries no
	// signal for a time series.
	DateImport = "created_at (bulk import)"
)

// DocDate is a document's date by the RFC rule: the root chunk's valid_at, else
// its import provenance's source_updated_at, else the document's created_at.
// A created_at before the end of importedUntil's day (UTC) is reported as
// DateImport; a zero importedUntil marks nothing so.
func DocDate(d *Doc, importedUntil time.Time) (time.Time, string) {
	if d.RootValidAt != 0 {
		return time.Unix(0, d.RootValidAt).UTC(), DateValidAt
	}
	if d.SourceUpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, d.SourceUpdatedAt); err == nil {
			return t.UTC(), DateSourceUpd
		}
	}
	at := time.Unix(0, d.CreatedAt).UTC()
	if !importedUntil.IsZero() && at.Before(importedUntil.UTC().Truncate(24*time.Hour).Add(24*time.Hour)) {
		return at, DateImport
	}
	return at, DateCreatedAt
}

// DateReport is measurement 6.
type DateReport struct {
	All    map[string]int `json:"all_documents_by_source"`
	Placed map[string]int `json:"placed_documents_by_source"`
	Nodes  []NodeDates    `json:"nodes"`
}

// NodeDates: Periods is the number of distinct months the members fall in,
// Usable those with at least MinPeriodMembers members dated by something other
// than the bulk import (Dated counts those members).
type NodeDates struct {
	Name    string `json:"name"`
	Members int    `json:"members"`
	Dated   int    `json:"members_not_import_dated"`
	Periods int    `json:"months"`
	Usable  int    `json:"months_with_3_or_more"`
	First   string `json:"first,omitempty"`
	Last    string `json:"last,omitempty"`
}

// MinPeriodMembers is the fewest members a monthly period needs to be usable
// for a time series (RFC GM section 6A).
const MinPeriodMembers = 3

func Dates(tax *Taxonomy, docs map[string]*Doc, placed map[string][]string, members map[string][]string, importedUntil time.Time) DateReport {
	r := DateReport{All: map[string]int{}, Placed: map[string]int{}}
	for id, d := range docs {
		if d.Ontology {
			continue
		}
		_, src := DocDate(d, importedUntil)
		r.All[src]++
		if len(placed[id]) > 0 {
			r.Placed[src]++
		}
	}
	for _, id := range tax.Order() {
		months := map[string]int{}
		dated := 0
		for _, m := range members[id] {
			if d := docs[m]; d != nil {
				t, src := DocDate(d, importedUntil)
				if src == DateImport {
					continue
				}
				dated++
				months[t.Format("2006-01")]++
			}
		}
		nd := NodeDates{Name: pathName(tax, id), Members: len(members[id]), Dated: dated, Periods: len(months)}
		keys := make([]string, 0, len(months))
		for k, n := range months {
			keys = append(keys, k)
			if n >= MinPeriodMembers {
				nd.Usable++
			}
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			nd.First, nd.Last = keys[0], keys[len(keys)-1]
		}
		r.Nodes = append(r.Nodes, nd)
	}
	return r
}

// MisfitReport is one comparison set at one margin.
type MisfitReport struct {
	Set      string          `json:"set"`
	Margin   float64         `json:"margin"`
	Count    int             `json:"count"`
	Examples []MisfitExample `json:"examples"`
}

// MisfitExample is one misfit, by name.
type MisfitExample struct {
	Title   string  `json:"title"`
	Node    string  `json:"node"`
	Own     float64 `json:"own_similarity"`
	Nearest string  `json:"nearest"`
	NearSim float64 `json:"nearest_similarity"`
}

// MeasureMisfits runs nodestats.Misfits over the top level and over the
// deepest level (all concepts of the maximum depth together), at each margin.
func MeasureMisfits(tax *Taxonomy, docs map[string]*Doc, members map[string][]string, margins []float64) ([]MisfitReport, error) {
	maxDepth := 0
	for _, id := range tax.Order() {
		if d := tax.Get(id).Depth; d > maxDepth {
			maxDepth = d
		}
	}
	sets := []struct {
		name  string
		depth int
	}{{"top level", 1}}
	if maxDepth > 1 {
		sets = append(sets, struct {
			name  string
			depth int
		}{fmt.Sprintf("leaf level (depth %d)", maxDepth), maxDepth})
	}
	var out []MisfitReport
	for _, set := range sets {
		var nodes []nodestats.Node
		for _, id := range tax.Order() {
			if tax.Get(id).Depth == set.depth {
				nodes = append(nodes, nodestats.Node{ID: id, Members: vectorMembers(docs, members[id])})
			}
		}
		for _, m := range margins {
			mf, err := nodestats.Misfits(nodes, m, nodestats.MinMembers)
			if err != nil && !errors.Is(err, nodestats.ErrEmpty) {
				return nil, fmt.Errorf("misfits %s margin %v: %w", set.name, m, err)
			}
			sort.SliceStable(mf, func(i, j int) bool {
				return mf[i].NearestSimilarity-mf[i].OwnSimilarity > mf[j].NearestSimilarity-mf[j].OwnSimilarity
			})
			rep := MisfitReport{Set: set.name, Margin: m, Count: len(mf)}
			for i := 0; i < len(mf) && i < 10; i++ {
				f := mf[i]
				rep.Examples = append(rep.Examples, MisfitExample{Title: docs[f.MemberID].Title,
					Node: tax.Get(f.NodeID).Name, Own: f.OwnSimilarity, Nearest: tax.Get(f.NearestNodeID).Name,
					NearSim: f.NearestSimilarity})
			}
			out = append(out, rep)
		}
	}
	return out, nil
}
