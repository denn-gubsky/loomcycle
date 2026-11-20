package builtin

// document_node_split.go — split, unplaced grouping and merge candidates (RFC GM §5.2,
// §5.3, §2C step 5, plan P5.2): Document op=node_stats mode split.
//
//   - splits: each concept with at least nodestats.SplitMinMembers members that have
//     a vector is clustered (k-means, k 2 to 4). A group is a candidate only when it
//     RECURS: nodestats.RecurringGroups re-clusters seeded subsamples and keeps a
//     group every run finds again. A candidate is a proposed narrower concept under
//     the node (§5.2); the node keeps its id, its members and its rolled-up series.
//   - unplaced: per scheme, the things it projects that no concept of it holds,
//     those of periods whose unplaced share rose first (6A.3), grouped the same way.
//     A thing whose similarity to its group's centre is below unplacedFarSimilarity
//     stays unplaced, and so does every thing of a group that does not recur (§5.3).
//     A group is a proposed concept under the nearest concept of the scheme, or at
//     its top when no concept is that near.
//   - merges: two sibling concepts whose centroids are at least mergeSimilarity alike.
//     The smaller would be retired into the larger.
//
// THE MODEL CHECK IS NOT HERE. §2C step 5 proposes a group only when a model grouping
// of the things' fine subtopic labels finds it too. That is a model call, and node_stats
// makes none: each group carries what that step reads (its documents with their titles
// and briefs, its distinctive terms, its exemplars), and the step belongs to the
// generation agent (kb/taxonomist, plan P3.2), which files what both find. With propose
// this mode files the recurring groups as they stand, each body saying no model
// grouping checked it; the operator rules on every one (D7).
//
// WHAT IS FILED. Report by default: nothing is written. With propose (the grant rule of
// mode misfits: an operator, or a run granted rule in statement_methods), at most limit
// proposals per call, through propose_entity, so every check of that op applies and
// every filing is inert until an operator accepts it:
//   - a new concept (a split group, an unplaced group) is an add proposal
//     (section schemes, under its parent);
//   - a merge is a change to the ontology, filed as RFC GQ's retire proposal of the
//     smaller concept (change retire), its body naming the concept it merges into. The
//     plan of that change is RFC GQ's: the operator plans the proposal by its id
//     ({"op":"proposal","id"}) and the accept requires the plan's fingerprint.
//
// Moving members is never filed here. A split group's documents can be placed under the
// new concept only once it is in force; until then the group lists them, and after the
// accept mode misfits and classify see the new concept like any other.
//
// FIXED CONCEPTS. A concept of an @origin adopted scheme, or one @locked (itself or its
// scheme), is never proposed for retiring, renaming or moving: a merge that would retire
// one is reported with fixed and never filed. A local concept may still be added under it.
//
// Like misfits, the mode needs the vector stage (Postgres with pgvector and an
// embedder); elsewhere it says so and lists nothing. The same input gives the same
// answer: every random draw is seeded with splitSeed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

const (
	nodeStatsModeSplit = "split"

	// splitProposalCap is the proposals one call files when limit is not given;
	// splitProposalMax bounds limit. Lower than misfits': each is an ontology entry.
	splitProposalCap = 10
	splitProposalMax = 50
	// splitClusterCap bounds the things one node or scheme clusters: the first by id,
	// and ids are random, so a large node is sampled rather than refused.
	splitClusterCap = 2000
	// splitDocsCap bounds the documents a group lists; splitListCap the stay-unplaced
	// things a scheme lists.
	splitDocsCap = 50
	splitListCap = 200
	// splitSeed seeds every clustering: the same input gives the same answer.
	splitSeed = 1
	// splitNameTerms is how many distinctive terms make a suggested name.
	splitNameTerms = 3

	// unplacedFarSimilarity: an unplaced thing whose cosine similarity to its group's
	// centre is below it stays unplaced, and a group whose nearest concept is below it
	// is proposed at the top of its scheme. mergeSimilarity: sibling concepts whose
	// centroids are at least this alike are a merge candidate. Neither was set by the
	// P0 measurement; both depend on the embedding model, as MisfitMargin does.
	unplacedFarSimilarity = 0.5
	mergeSimilarity       = 0.95
)

// splitModelCheck is what the answer says of the model check §2C step 5 asks for.
const splitModelCheck = "not run: node_stats makes no model call. A group is meant to be proposed only when a model grouping of the things' subtopic labels finds it too; each group lists the documents that step reads"

// splitDoc is one document of a group, or a thing that stays unplaced: Similarity is
// its cosine to the group's centre (a stay-unplaced thing: to the nearest kept group's,
// 0 when none was kept).
type splitDoc struct {
	DocumentID string  `json:"document_id"`
	Title      string  `json:"title"`
	Brief      string  `json:"brief,omitempty"`
	Similarity float64 `json:"similarity"`
}

// splitGroup is one recurring group: a candidate concept.
type splitGroup struct {
	// Key names the group across calls: a hash of its sorted member ids.
	Key  string `json:"key"`
	Name string `json:"name"`
	// Terms are its distinctive terms against the other groups (the name is the first).
	Terms   []string `json:"terms"`
	Members int      `json:"members"`
	// Recurrence: the runs that found it, of Runs.
	Recurrence int `json:"recurrence"`
	Runs       int `json:"runs"`
	// ParentID / Parent: where it would be filed — a concept (by tag) or a scheme.
	ParentID string `json:"parent_id"`
	Parent   string `json:"parent"`
	// ParentSimilarity (unplaced groups): the cosine of its centre to the parent
	// concept's centroid; absent when it goes to the top of the scheme.
	ParentSimilarity   *float64   `json:"parent_similarity,omitempty"`
	Exemplars          []splitDoc `json:"exemplars"`
	Documents          []splitDoc `json:"documents"`
	DocumentsTruncated bool       `json:"documents_truncated,omitempty"`
	// With propose: ProposalID of the add proposal filed, or Skipped (exists, rejected,
	// duplicate, cap, refused:<code>) and Detail.
	ProposalID string `json:"proposal_id,omitempty"`
	Skipped    string `json:"skipped,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// splitNode is one concept clustered for a split.
type splitNode struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Tag    string `json:"tag"`
	Scheme string `json:"scheme"`
	// Members is its member count; Clustered the members with a vector that were
	// clustered (at most splitClusterCap).
	Members   int     `json:"members"`
	Clustered int     `json:"clustered"`
	Spread    float64 `json:"spread"`
	// Fixed: adopted or locked. A narrower local concept may still be added under it.
	Fixed string `json:"fixed,omitempty"`
	// K is the clustering's k; Groups the recurring groups, NotRecurring how many of
	// its groups did not recur (reported, never proposed).
	K            int          `json:"k"`
	Groups       []splitGroup `json:"groups"`
	NotRecurring int          `json:"not_recurring"`
}

// unplacedGrouping is one scheme's unplaced things, grouped.
type unplacedGrouping struct {
	Scheme   string `json:"scheme"`
	SchemeID string `json:"scheme_id"`
	// Things: the unplaced things it projects; Clustered those with a vector that were
	// grouped (rising periods first, at most splitClusterCap); Grouped: there were
	// enough to group (nodestats.SplitMinMembers).
	Things       int          `json:"things"`
	Clustered    int          `json:"clustered"`
	Truncated    bool         `json:"truncated,omitempty"`
	Grouped      bool         `json:"grouped"`
	Groups       []splitGroup `json:"groups"`
	NotRecurring int          `json:"not_recurring"`
	// StaysUnplaced: the grouped things in no group kept — far from every group, or
	// in a group that did not recur.
	StaysUnplaced  []splitDoc `json:"stays_unplaced"`
	StaysTruncated bool       `json:"stays_truncated,omitempty"`
}

// mergeCandidate is two sibling concepts alike enough to be one.
type mergeCandidate struct {
	// NodeID / Name / Tag: the concept that would be retired; Into*: the one it merges
	// into (the one with more members).
	NodeID      string  `json:"node_id"`
	Name        string  `json:"name"`
	Tag         string  `json:"tag"`
	IntoID      string  `json:"into_id"`
	Into        string  `json:"into"`
	IntoTag     string  `json:"into_tag"`
	Similarity  float64 `json:"similarity"`
	Members     int     `json:"members"`
	IntoMembers int     `json:"into_members"`
	// Fixed: the concept that would be retired is adopted or locked, so no retire
	// proposal is ever filed for it.
	Fixed string `json:"fixed,omitempty"`
	// With propose: ProposalID of the retire proposal filed, or Skipped and Detail.
	ProposalID string `json:"proposal_id,omitempty"`
	Skipped    string `json:"skipped,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// NodeSplitReport is node_stats mode split's answer.
type NodeSplitReport struct {
	Mode   string `json:"mode"`
	Scheme string `json:"scheme,omitempty"`
	// Vectors: the clustering ran; Note says why it did not.
	Vectors    bool   `json:"vectors"`
	Note       string `json:"note,omitempty"`
	EmbedModel string `json:"embed_model,omitempty"`
	// The rules the answer was made under.
	MinMembers      int     `json:"min_members"`
	Runs            int     `json:"runs"`
	RecurMin        int     `json:"recur_min"`
	Jaccard         float64 `json:"jaccard"`
	FarSimilarity   float64 `json:"far_similarity"`
	MergeSimilarity float64 `json:"merge_similarity"`
	ModelCheck      string  `json:"model_check"`
	Propose         bool    `json:"propose"`
	// Cap is the most proposals this call files (propose only).
	Cap      int                `json:"cap,omitempty"`
	Splits   []splitNode        `json:"splits"`
	Unplaced []unplacedGrouping `json:"unplaced"`
	Merges   []mergeCandidate   `json:"merges"`
	// Proposed and Skipped count the outcomes (propose only).
	Proposed int            `json:"proposed,omitempty"`
	Skipped  map[string]int `json:"skipped,omitempty"`
	// MembersSkipped: members and things whose stored vector could not be used.
	// Issues: nodes and schemes the clustering failed on, left out.
	MembersSkipped int      `json:"members_skipped,omitempty"`
	Issues         []string `json:"issues,omitempty"`
	Truncated      bool     `json:"truncated,omitempty"`
}

// splitCap is the proposal cap a call asks for: limit, bounded, else the default.
func splitCap(limit int) int {
	switch {
	case limit <= 0:
		return splitProposalCap
	case limit > splitProposalMax:
		return splitProposalMax
	}
	return limit
}

// splitConcept is what the mode knows of an in-force concept.
type splitConcept struct {
	id, name, tag, scheme, fixed string
	retired                      bool
}

// splitState is one call's working set.
type splitState struct {
	p        *nodeStatsPlan
	dim      int
	concepts map[string]splitConcept
	// centroids of the in-force concepts with at least MinMembers usable vectors.
	centroids map[string]nodestats.Centroid
	bad       map[string]bool
}

// usable is the member's vector when it can be clustered (of dim, finite, not zero).
func (s *splitState) usable(vecs map[string]nodestats.Vector, id string) (nodestats.Vector, bool) {
	v, ok := vecs[id]
	if !ok {
		return nil, false
	}
	if !usableMisfitVector(v, s.dim) {
		s.bad[id] = true
		return nil, false
	}
	return v, true
}

// NodeSplit reports the split, unplaced-grouping and merge candidates of the tenant
// scope key names (only scheme's when scheme is set) and, with propose, files at most
// limit proposals. The caller has checked who may run it.
func (d *Document) NodeSplit(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, scheme string, propose bool, limit int) (NodeSplitReport, error) {
	ans := NodeSplitReport{Mode: nodeStatsModeSplit, Scheme: scheme, MinMembers: nodestats.SplitMinMembers,
		Runs: nodestats.RecurRuns, RecurMin: nodestats.RecurMin, Jaccard: nodestats.RecurJaccard,
		FarSimilarity: unplacedFarSimilarity, MergeSimilarity: mergeSimilarity, ModelCheck: splitModelCheck,
		Propose: propose, Splits: []splitNode{}, Unplaced: []unplacedGrouping{}, Merges: []mergeCandidate{}}
	if propose {
		ans.Cap = splitCap(limit)
	}
	p, err := d.buildNodeStatsPlan(ctx, key, read, scheme)
	if err != nil {
		return ans, err
	}
	ans.Vectors, ans.Note, ans.EmbedModel = p.model != "", p.note, p.model
	if p.model == "" {
		ans.Note = "split needs the vector stage, which does not run here: " + p.note
		return ans, nil
	}
	queues, uvecs, err := d.loadUnplaced(ctx, key, read, scheme, p)
	if err != nil {
		return ans, err
	}
	// The dimension is the most common over members and unplaced things alike: a new
	// scheme may have no member with a vector, and its unplaced things are all there is.
	s := &splitState{p: p, dim: commonDim(p.vecs, uvecs), concepts: map[string]splitConcept{},
		centroids: map[string]nodestats.Centroid{}, bad: map[string]bool{}}
	info := growthConceptInfo(read)
	if read.Confirmed {
		for _, sc := range read.Schemes {
			for _, c := range sc.Concepts {
				s.concepts[c.ChunkID] = splitConcept{id: c.ChunkID, name: c.Name, tag: c.Tag, scheme: sc.Name,
					fixed: info[c.ChunkID].fixed, retired: c.EntryStatus != "" || sc.EntryStatus != ""}
			}
		}
	}
	for _, n := range p.nodes {
		c, ok := s.concepts[n.ID]
		if n.Kind != nodeKindConcept || !ok || c.retired {
			continue
		}
		var vecs []nodestats.Vector
		for _, id := range n.members {
			if v, ok := s.usable(p.vecs, id); ok {
				vecs = append(vecs, v)
			}
		}
		if len(vecs) >= nodestats.MinMembers {
			if cen, err := nodestats.CentroidOf(vecs); err == nil {
				s.centroids[n.ID] = cen
			}
		}
	}
	for _, n := range p.nodes {
		if c, ok := s.concepts[n.ID]; ok && n.Kind == nodeKindConcept && !c.retired {
			if sn, ok := s.splitNode(n, c, &ans); ok {
				ans.Splits = append(ans.Splits, sn)
			}
		}
	}
	s.groupUnplaced(read, queues, uvecs, &ans)
	ans.Merges = s.merges()
	ans.MembersSkipped = len(s.bad)
	if propose {
		ans.Skipped = map[string]int{}
		d.proposeSplits(ctx, read, &ans)
	}
	if len(ans.Splits) > splitListCap {
		ans.Splits, ans.Truncated = ans.Splits[:splitListCap], true
	}
	if len(ans.Merges) > splitListCap {
		ans.Merges, ans.Truncated = ans.Merges[:splitListCap], true
	}
	return ans, nil
}

// splitNode clusters one concept's members. ok is false when the concept is too small
// or the clustering kept fewer than two groups (no split).
func (s *splitState) splitNode(n statNode, c splitConcept, ans *NodeSplitReport) (splitNode, bool) {
	var members []nodestats.Member
	for _, id := range n.members {
		if v, ok := s.usable(s.p.vecs, id); ok {
			members = append(members, nodestats.Member{ID: id, Vec: v})
		}
	}
	if len(members) < nodestats.SplitMinMembers {
		return splitNode{}, false
	}
	if len(members) > splitClusterCap {
		members = members[:splitClusterCap]
	}
	res, err := nodestats.RecurringGroups(members, nodestats.DefaultRecurOptions(splitSeed))
	if err != nil {
		ans.Issues = append(ans.Issues, fmt.Sprintf("%s was not clustered: %v", c.tag, err))
		return splitNode{}, false
	}
	if len(res.Reference.Groups) < 2 {
		return splitNode{}, false
	}
	sn := splitNode{NodeID: n.ID, Name: c.name, Tag: c.tag, Scheme: c.scheme, Members: len(n.members),
		Clustered: len(members), Fixed: c.fixed, K: res.Reference.K, Groups: []splitGroup{},
		NotRecurring: len(res.Reference.Groups) - len(res.Groups)}
	if cen, ok := s.centroids[n.ID]; ok {
		sn.Spread = cen.Spread()
	}
	vecOf := memberVectors(members)
	all := make([][]string, len(res.Groups))
	for i, g := range res.Groups {
		all[i] = g.Members
	}
	for i, g := range res.Groups {
		sg := s.p.describeGroup(g.Group, vecOf, others(all, i, nil))
		sg.Recurrence, sg.Runs, sg.ParentID, sg.Parent = g.Recurrence, res.Runs, n.ID, c.tag
		sn.Groups = append(sn.Groups, sg)
	}
	return sn, true
}

// others is every member list of all but the i-th, plus extra: the texts a group's
// distinctive terms are ranked against.
func others(all [][]string, i int, extra []string) [][]string {
	out := make([][]string, 0, len(all))
	for j, ids := range all {
		if j != i {
			out = append(out, ids)
		}
	}
	if len(extra) > 0 {
		out = append(out, extra)
	}
	return out
}

func memberVectors(members []nodestats.Member) map[string]nodestats.Vector {
	out := make(map[string]nodestats.Vector, len(members))
	for _, m := range members {
		out[m.ID] = m.Vec
	}
	return out
}

// describeGroup is a group as the answer lists it: its key, distinctive terms (against
// the other groups' texts) and the name they suggest, exemplars, and documents nearest
// the centre first.
func (p *nodeStatsPlan) describeGroup(g nodestats.Group, vecOf map[string]nodestats.Vector, against [][]string) splitGroup {
	sg := splitGroup{Key: groupKey(g.Members), Members: len(g.Members), Terms: []string{}, Exemplars: []splitDoc{}, Documents: []splitDoc{}}
	text := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if d := p.docs[id]; d != nil {
				out = append(out, d.brief+"\n"+d.abstract)
			}
		}
		return out
	}
	sib := make([][]string, len(against))
	for i, ids := range against {
		sib[i] = text(ids)
	}
	if terms, err := nodestats.DistinctiveTerms(text(g.Members), sib, nodestats.TopTerms); err == nil {
		for _, t := range terms {
			sg.Terms = append(sg.Terms, t.Term)
		}
	}
	var docs []splitDoc
	for _, id := range g.Members {
		sim := 0.0
		if i, s, err := nodestats.Nearest(vecOf[id], []nodestats.Centroid{g.Centroid}); err == nil && i == 0 {
			sim = s
		}
		docs = append(docs, p.splitDoc(id, sim))
	}
	sortSplitDocs(docs)
	for i := 0; i < len(docs) && i < nodestats.KExemplars; i++ {
		ex := docs[i]
		ex.Brief = ""
		sg.Exemplars = append(sg.Exemplars, ex)
	}
	if len(docs) > splitDocsCap {
		docs, sg.DocumentsTruncated = docs[:splitDocsCap], true
	}
	sg.Documents = docs
	sg.Name = suggestedName(sg.Terms, sg.Exemplars)
	return sg
}

func (p *nodeStatsPlan) splitDoc(id string, sim float64) splitDoc {
	out := splitDoc{DocumentID: id, Similarity: sim}
	if d := p.docs[id]; d != nil {
		out.Title, out.Brief = d.title, d.brief
	}
	return out
}

// sortSplitDocs orders documents nearest the centre first, then by id.
func sortSplitDocs(docs []splitDoc) {
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].Similarity != docs[j].Similarity {
			return docs[i].Similarity > docs[j].Similarity
		}
		return docs[i].DocumentID < docs[j].DocumentID
	})
}

// suggestedName is the group's first distinctive terms, else its first exemplar's
// title: a placeholder the operator (or the generation agent) renames on accepting.
func suggestedName(terms []string, exemplars []splitDoc) string {
	if len(terms) > 0 {
		return strings.Join(terms[:min(splitNameTerms, len(terms))], " ")
	}
	if len(exemplars) > 0 && strings.TrimSpace(exemplars[0].Title) != "" {
		t := strings.TrimSpace(exemplars[0].Title)
		if utf8.RuneCountInString(t) > ontologyPlanMaxName {
			t, _ = cutRunes(t, ontologyPlanMaxName)
		}
		return t
	}
	return "unnamed group"
}

// groupKey is a group's name across calls: the first 16 hex of the hash of its sorted
// member ids.
func groupKey(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:8])
}

// loadUnplaced reads each scheme's unplaced queue (unplacedQueue, at most
// splitClusterCap things each) and those things' annotations and vectors. The
// unplaced things are no node's members, so the plan read none of them: their
// documents are added to p.docs, as a plan holds its members'.
func (d *Document) loadUnplaced(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, scheme string, p *nodeStatsPlan) (map[string][]string, map[string]nodestats.Vector, error) {
	queues := map[string][]string{}
	if !read.Confirmed {
		return queues, nil, nil
	}
	sp, err := d.buildSeriesPlan(ctx, key, read, scheme, p)
	if err != nil {
		return nil, nil, err
	}
	var want []string
	for _, sc := range read.Schemes {
		if (scheme != "" && sc.Name != scheme) || sc.EntryStatus != "" {
			continue
		}
		if q := unplacedQueue(sp, sc.ChunkID); len(q) > 0 {
			queues[sc.ChunkID] = q
			want = append(want, q[:min(len(q), splitClusterCap)]...)
		}
	}
	if len(want) == 0 {
		return queues, nil, nil
	}
	extra := &nodeStatsPlan{docs: map[string]*statDoc{}, model: p.model, tierCol: p.tierCol}
	if err := d.loadStatDocs(ctx, key, sortedSet(toSet(want)), extra.docs, p.tierCol); err != nil {
		return nil, nil, err
	}
	vecs, err := d.loadNodeStatsVectors(ctx, key, direntTenant(ctx), extra)
	if err != nil {
		return nil, nil, err
	}
	for id, sd := range extra.docs {
		if _, ok := p.docs[id]; !ok {
			p.docs[id] = sd
		}
	}
	return queues, vecs, nil
}

// commonDim is the dimension most of the vectors have (ties: the smaller): the one
// the clustering uses. A vector of another is a stray, left out (usableMisfitVector).
func commonDim(sets ...map[string]nodestats.Vector) int {
	byDim := map[int]int{}
	for _, set := range sets {
		for _, v := range set {
			byDim[len(v)]++
		}
	}
	best, bestN := 0, 0
	for dim, n := range byDim {
		if n > bestN || (n == bestN && dim < best) {
			best, bestN = dim, n
		}
	}
	return best
}

// groupUnplaced groups each scheme's unplaced queue (loadUnplaced).
func (s *splitState) groupUnplaced(read OntologyRead, queues map[string][]string, vecs map[string]nodestats.Vector, ans *NodeSplitReport) {
	for _, sc := range read.Schemes {
		q, ok := queues[sc.ChunkID]
		if !ok {
			continue
		}
		ug := unplacedGrouping{Scheme: sc.Name, SchemeID: sc.ChunkID, Things: len(q), Groups: []splitGroup{}, StaysUnplaced: []splitDoc{}}
		if len(q) > splitClusterCap {
			q, ug.Truncated = q[:splitClusterCap], true
		}
		var members []nodestats.Member
		for _, id := range q {
			if v, ok := s.usable(vecs, id); ok {
				members = append(members, nodestats.Member{ID: id, Vec: v})
			}
		}
		ug.Clustered = len(members)
		if len(members) >= nodestats.SplitMinMembers {
			ug.Grouped = true
			s.groupScheme(sc, members, &ug, ans)
		}
		ans.Unplaced = append(ans.Unplaced, ug)
	}
}

func toSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// unplacedQueue is a scheme's unplaced things as the current state dates them: those
// of periods whose unplaced share rose over the period before first (RFC GM 6A.3),
// most recent first, then the rest, most recent first; each thing once.
func unplacedQueue(sp *seriesPlan, schemeID string) []string {
	byStart := sp.unplaced[schemeID]
	if len(byStart) == 0 {
		return nil
	}
	share := map[int64]float64{}
	for _, r := range sp.rows {
		if r.measure == measureUnplaced && r.node == schemeID && r.basis > 0 {
			share[r.start] = r.value / float64(r.basis)
		}
	}
	starts := make([]int64, 0, len(byStart))
	for st := range byStart {
		starts = append(starts, st)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	rising := map[int64]bool{}
	for i := 1; i < len(starts); i++ {
		rising[starts[i]] = share[starts[i]] > share[starts[i-1]]
	}
	sort.SliceStable(starts, func(i, j int) bool {
		if rising[starts[i]] != rising[starts[j]] {
			return rising[starts[i]]
		}
		return starts[i] > starts[j]
	})
	seen := map[string]bool{}
	var out []string
	for _, st := range starts {
		for _, id := range byStart[st] {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// groupScheme groups one scheme's unplaced members (§5.3): a recurring group keeps the
// members at least unplacedFarSimilarity alike to its centre and is filed under the
// nearest concept of the scheme that near, else at its top; every other member stays
// unplaced.
func (s *splitState) groupScheme(sc memrank.OntologyScheme, members []nodestats.Member, ug *unplacedGrouping, ans *NodeSplitReport) {
	res, err := nodestats.RecurringGroups(members, nodestats.DefaultRecurOptions(splitSeed))
	if err != nil {
		ans.Issues = append(ans.Issues, fmt.Sprintf("the unplaced things of %s were not grouped: %v", sc.Name, err))
		res = nodestats.RecurResult{}
	}
	ug.NotRecurring = len(res.Reference.Groups) - len(res.Groups)
	vecOf := memberVectors(members)
	type kept struct {
		g   nodestats.Group
		rec int
	}
	var keep []kept
	for _, g := range res.Groups {
		var near []nodestats.Vector
		var ids []string
		for _, id := range g.Members {
			if _, sim, err := nodestats.Nearest(vecOf[id], []nodestats.Centroid{g.Centroid}); err == nil && sim >= unplacedFarSimilarity {
				near = append(near, vecOf[id])
				ids = append(ids, id)
			}
		}
		if len(ids) < nodestats.MinMembers {
			continue
		}
		cen, err := nodestats.CentroidOf(near)
		if err != nil {
			continue
		}
		keep = append(keep, kept{g: nodestats.Group{Members: ids, Centroid: cen}, rec: g.Recurrence})
	}
	in := map[string]bool{}
	all := make([][]string, len(keep))
	cents := make([]nodestats.Centroid, len(keep))
	for i, k := range keep {
		all[i], cents[i] = k.g.Members, k.g.Centroid
		for _, id := range k.g.Members {
			in[id] = true
		}
	}
	var stays []string
	for _, m := range members {
		if !in[m.ID] {
			stays = append(stays, m.ID)
		}
	}
	// The scheme's concepts with a centroid, in a fixed order, for the nearest node.
	var conceptIDs []string
	for _, c := range sc.Concepts {
		if _, ok := s.centroids[c.ChunkID]; ok {
			conceptIDs = append(conceptIDs, c.ChunkID)
		}
	}
	sort.Strings(conceptIDs)
	conceptCents := make([]nodestats.Centroid, len(conceptIDs))
	for i, id := range conceptIDs {
		conceptCents[i] = s.centroids[id]
	}
	for i, k := range keep {
		sg := s.p.describeGroup(k.g, vecOf, others(all, i, stays))
		sg.Recurrence, sg.Runs = k.rec, res.Runs
		sg.ParentID, sg.Parent = sc.ChunkID, sc.Name
		if j, sim := nearestCentroid(k.g.Centroid, conceptCents); j >= 0 && sim >= unplacedFarSimilarity {
			sg.ParentID, sg.Parent = conceptIDs[j], s.concepts[conceptIDs[j]].tag
			v := sim
			sg.ParentSimilarity = &v
		}
		ug.Groups = append(ug.Groups, sg)
	}
	for _, id := range stays {
		best := 0.0
		if _, sim, err := nodestats.Nearest(vecOf[id], cents); err == nil && len(cents) > 0 {
			best = sim
		}
		ug.StaysUnplaced = append(ug.StaysUnplaced, s.p.splitDoc(id, best))
	}
	if len(ug.StaysUnplaced) > splitListCap {
		ug.StaysUnplaced, ug.StaysTruncated = ug.StaysUnplaced[:splitListCap], true
	}
}

// nearestCentroid is the index of the centroid of cs most like c and the similarity;
// -1 when cs is empty.
func nearestCentroid(c nodestats.Centroid, cs []nodestats.Centroid) (int, float64) {
	best, bestSim := -1, 0.0
	for i, x := range cs {
		sim, err := nodestats.Similarity(c, x)
		if err != nil {
			continue
		}
		if best < 0 || sim > bestSim {
			best, bestSim = i, sim
		}
	}
	return best, bestSim
}

// merges is every pair of sibling concepts whose centroids are at least
// mergeSimilarity alike, most alike first; each concept is retired by at most one.
func (s *splitState) merges() []mergeCandidate {
	groups := map[string][]string{}
	for _, n := range s.p.nodes {
		if _, ok := s.centroids[n.ID]; ok && n.Kind == nodeKindConcept {
			groups[n.group] = append(groups[n.group], n.ID)
		}
	}
	size := map[string]int{}
	for _, n := range s.p.nodes {
		size[n.ID] = len(n.members)
	}
	var out []mergeCandidate
	for _, ids := range groups {
		sort.Strings(ids)
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				sim, err := nodestats.Similarity(s.centroids[ids[i]], s.centroids[ids[j]])
				if err != nil || sim < mergeSimilarity {
					continue
				}
				gone, into := s.concepts[ids[i]], s.concepts[ids[j]]
				if size[gone.id] > size[into.id] || (size[gone.id] == size[into.id] && gone.tag < into.tag) {
					gone, into = into, gone
				}
				out = append(out, mergeCandidate{NodeID: gone.id, Name: gone.name, Tag: gone.tag, IntoID: into.id,
					Into: into.name, IntoTag: into.tag, Similarity: sim, Members: size[gone.id], IntoMembers: size[into.id],
					Fixed: gone.fixed})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].Tag+out[i].IntoTag < out[j].Tag+out[j].IntoTag
	})
	retired := map[string]bool{}
	kept := out[:0]
	for _, m := range out {
		if retired[m.NodeID] {
			continue
		}
		retired[m.NodeID] = true
		kept = append(kept, m)
	}
	return kept
}

// proposeSplits files the proposals in the answer's order — split groups, unplaced
// groups, merges — until the cap. Each is its own propose_entity call, so the
// ontology lock and every check of that op apply to it.
func (d *Document) proposeSplits(ctx context.Context, read OntologyRead, ans *NodeSplitReport) {
	open := map[string]string{}
	for _, p := range read.Proposals {
		if p.Section == memrank.SectionSchemes && p.Change == nil && p.Scheme == nil {
			open[strings.ToLower(p.Name)+"\x00"+p.Parent] = p.Status
		}
	}
	parentName := func(sg *splitGroup) string {
		for _, s := range read.Schemes {
			if s.ChunkID == sg.ParentID {
				return s.Name
			}
			for _, c := range s.Concepts {
				if c.ChunkID == sg.ParentID {
					return c.Name
				}
			}
		}
		return sg.Parent
	}
	filed := map[string]bool{}
	add := func(sg *splitGroup, from string) {
		pair := strings.ToLower(sg.Name) + "\x00" + parentName(sg)
		switch status := open[pair]; {
		case status == memrank.OntologyStatusRejected:
			sg.Skipped = "rejected"
		case status != "":
			sg.Skipped = "exists"
		case filed[pair]:
			sg.Skipped = "duplicate"
		case ans.Proposed >= ans.Cap:
			sg.Skipped = "cap"
		}
		if sg.Skipped == "" {
			filed[pair] = true
			sg.ProposalID, sg.Skipped, sg.Detail = d.fileSplitProposal(ctx, docInput{Op: "propose_entity", Scope: "tenant",
				Section: memrank.SectionSchemes, Name: sg.Name, Parent: sg.Parent, Body: splitGroupBody(sg, from)})
		}
		splitCount(ans, sg.Skipped)
	}
	for i := range ans.Splits {
		for j := range ans.Splits[i].Groups {
			add(&ans.Splits[i].Groups[j], "a split of "+ans.Splits[i].Tag)
		}
	}
	for i := range ans.Unplaced {
		for j := range ans.Unplaced[i].Groups {
			add(&ans.Unplaced[i].Groups[j], "the unplaced things of the scheme "+ans.Unplaced[i].Scheme)
		}
	}
	for i := range ans.Merges {
		m := &ans.Merges[i]
		switch {
		case m.Fixed != "":
			m.Skipped = "fixed"
		case ans.Proposed >= ans.Cap:
			m.Skipped = "cap"
		default:
			m.ProposalID, m.Skipped, m.Detail = d.fileSplitProposal(ctx, docInput{Op: "propose_entity", Scope: "tenant",
				Change: memrank.ChangeOpRetire, Entry: m.NodeID, Body: mergeBody(m)})
		}
		splitCount(ans, m.Skipped)
	}
}

// splitCount counts one outcome: filed when skipped is empty.
func splitCount(ans *NodeSplitReport, skipped string) {
	if skipped == "" {
		ans.Proposed++
		return
	}
	ans.Skipped[strings.SplitN(skipped, ":", 2)[0]]++
}

// fileSplitProposal runs one propose_entity call: the chunk id it filed, or why not.
// A refusal propose_entity answers "already ..." (an open or ruled proposal of that
// name, an open retire of that entry) reads as exists.
func (d *Document) fileSplitProposal(ctx context.Context, in docInput) (id, skipped, detail string) {
	res, err := d.executeInput(ctx, in, nil)
	if err != nil {
		return "", "refused:error", err.Error()
	}
	if res.IsError {
		if strings.Contains(res.Text, "already_proposed") || strings.Contains(res.Text, " is already ") {
			return "", "exists", res.Text
		}
		code := "error"
		if res.Error != nil {
			code = string(res.Error.Category)
		}
		return "", "refused:" + code, res.Text
	}
	var out struct {
		ChunkID string `json:"chunk_id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out.ChunkID, "", ""
}

// splitGroupBody is an add proposal's evidence: where the group came from, that no
// model grouping checked it, its terms and its exemplars.
func splitGroupBody(sg *splitGroup, from string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proposed by node_stats mode split from %s: a k-means group of %d documents that recurred in %d of %d clusterings. No model grouping of subtopic labels checked it.\n",
		from, sg.Members, sg.Recurrence, sg.Runs)
	if len(sg.Terms) > 0 {
		fmt.Fprintf(&b, "Distinctive terms: %s.\n", strings.Join(sg.Terms, ", "))
	}
	b.WriteString("Exemplars:\n")
	for _, e := range sg.Exemplars {
		fmt.Fprintf(&b, "- %s (%s)\n", e.Title, e.DocumentID)
	}
	return capBody(b.String())
}

// mergeBody is a retire proposal's reason: what the concept merges into, and why.
func mergeBody(m *mergeCandidate) string {
	return capBody(fmt.Sprintf("Merge into %s (%s): proposed by node_stats mode split, whose centroids of the two sibling concepts are %.3f alike (at least %.2f). %d members of this concept would be placed under %s, which has %d; the placements are not moved by this proposal.",
		m.IntoTag, m.IntoID, m.Similarity, mergeSimilarity, m.Members, m.IntoTag, m.IntoMembers))
}

func capBody(s string) string {
	if len(s) <= proposalBodyMax {
		return s
	}
	cut := s[:proposalBodyMax]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
