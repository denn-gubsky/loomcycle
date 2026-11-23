package builtin

// document_node_assign.go — assignment by centroid (RFC GM P3.3): Document
// op=node_stats mode assign. For each document a scheme arranges, it compares the
// document's annotation vector (the unit is the whole document, D3) with the
// centroids of one level's concepts and reads off the nearest, the second nearest
// and the margin between them, walking down the scheme.
//
// REPORT BY DEFAULT (the held-out check, owner ruling 2026-11-22). Without propose
// the mode writes nothing: a page of documents, each with its live placements and
// its rankings, and the agreement of the nearest with the live placements by margin
// bucket per level, over every document compared (not only the page). created_after
// narrows the set to documents created later, the shape of a held-out set. It is the
// one read that shows what the margin rule would do before any placement is made.
//
// PROPOSALS ONLY (D17, owner ruling 2026-11-20: every placement stays a proposal).
// With propose, a ranking whose margin is at least its level's threshold, and whose
// nearest concept the document is not already placed under, is filed as a `subject`
// proposal with method rule:centroid and confidence = the margin (at most 1), exactly
// as misfits files: proposeOnly (the @auto_assert rule never asserts it), at most
// limit per call, never a placement any reviewer rejected nor one already proposed or
// live, only by an operator or a run granted rule. Nothing here makes a placement
// live: only a review rule the tenant writes on method rule:centroid could, through
// the existing review policy at filing.
//
// THRESHOLDS. margins sets one per level; the default is 0.03 at level 2 only (P0's
// margin check: 98% within a parent at 39% coverage) and nothing at level 1, where
// 90% at 0.03 was not clearly established. A level without a threshold is reported
// and never filed.
//
// THE WALK. Level 1 compares the scheme's top concepts. A lower level compares the
// children of a concept the document is placed under live, or of the nearest concept
// one level up when that ranking cleared its threshold: within a parent, the shape
// P0 measured. A comparison needs at least two concepts with a centroid (MinMembers
// members with a vector each).
//
// LEAVE-ONE-OUT (nodestats.Ranker). A document that is a member of a concept is
// compared with that concept's centroid without itself, so agreement is not
// inflated by the document's own pull.
//
// WHO IS COUNTED (C8). Only documents every member of the tenant may read, as every
// node_stats mode; the caller must read the whole scope (node_stats' own gate).
// Vectors are returned by no answer, only similarities.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

const (
	nodeStatsModeAssign = "assign"

	// assignPageDefault / assignPageMax bound the documents one report page lists.
	assignPageDefault = 50
	assignPageMax     = 200
	// assignBucketWidth and assignBucketCount shape the agreement table: buckets of
	// 0.01 from 0, the last one open (at least assignBucketCount × width).
	assignBucketWidth = 0.01
	assignBucketCount = 10
)

// assignDefaultMargins is the threshold per level when margins is not given (RFC GM
// P0 margin check; owner ruling 2026-11-20: level 2 at 0.03, level 1 off).
var assignDefaultMargins = map[int]float64{2: 0.03}

// assignNode is a concept a document was compared with.
type assignNode struct {
	NodeID     string  `json:"node_id"`
	Node       string  `json:"node"`
	Tag        string  `json:"tag"`
	Similarity float64 `json:"similarity"`
}

// assignRanking is one comparison of a document with one level's concepts.
type assignRanking struct {
	Scheme string `json:"scheme"`
	Level  int    `json:"level"`
	// Parent is the broader concept's tag, "" at the scheme's top level.
	Parent   string     `json:"parent,omitempty"`
	Compared int        `json:"compared"`
	Nearest  assignNode `json:"nearest"`
	Second   assignNode `json:"second"`
	Margin   float64    `json:"margin"`
	// Agrees: the nearest is a concept the document is placed under live (directly
	// or through a narrower one). Absent when it is placed under none of the
	// compared concepts: there is nothing to agree with.
	Agrees *bool `json:"agrees,omitempty"`
	// Threshold is the level's; Eligible marks a ranking the propose form would file.
	Threshold *float64 `json:"threshold,omitempty"`
	Eligible  bool     `json:"eligible,omitempty"`
	// With propose: the outcome, as misfits reports it.
	StatementID string `json:"statement_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Skipped     string `json:"skipped,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// assignPlacement is a concept a document is placed under live, the most specific
// of each branch.
type assignPlacement struct {
	NodeID string `json:"node_id"`
	Node   string `json:"node"`
	Tag    string `json:"tag"`
}

// assignDoc is one document of a report page.
type assignDoc struct {
	DocumentID string            `json:"document_id"`
	Title      string            `json:"title"`
	Placements []assignPlacement `json:"placements"`
	Rankings   []assignRanking   `json:"rankings"`
}

// assignProposal is an eligible ranking with its document, in the propose form.
type assignProposal struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	assignRanking
}

// assignBucket is one row of the agreement table: the rankings whose margin is in
// [From, To) (To absent: no upper bound), how many could be judged (Placed: the
// document is placed under one of the compared concepts) and how many of those the
// nearest agrees with.
type assignBucket struct {
	From      float64  `json:"from"`
	To        *float64 `json:"to,omitempty"`
	Rankings  int      `json:"rankings"`
	Placed    int      `json:"placed"`
	Agree     int      `json:"agree"`
	Agreement *float64 `json:"agreement,omitempty"`
}

// assignLevel is the agreement of one level: by bucket, and at or above each
// bucket's lower bound (the form P0's margin check was read in).
type assignLevel struct {
	Level     int            `json:"level"`
	Threshold *float64       `json:"threshold,omitempty"`
	Rankings  int            `json:"rankings"`
	Placed    int            `json:"placed"`
	Agree     int            `json:"agree"`
	Eligible  int            `json:"eligible"`
	Buckets   []assignBucket `json:"buckets"`
	AtLeast   []assignBucket `json:"at_least"`
}

// NodeAssignReport is node_stats mode assign's answer.
type NodeAssignReport struct {
	Mode   string `json:"mode"`
	Scheme string `json:"scheme,omitempty"`
	// Vectors: the comparison ran; Note says why it did not.
	Vectors    bool   `json:"vectors"`
	Note       string `json:"note,omitempty"`
	EmbedModel string `json:"embed_model,omitempty"`
	MinMembers int    `json:"min_members"`
	// Margins is the threshold per level in force for this call.
	Margins map[string]float64 `json:"margins"`
	Propose bool               `json:"propose"`
	Cap     int                `json:"cap,omitempty"`
	// CreatedAfter echoes the held-out bound.
	CreatedAfter *int64 `json:"created_after,omitempty"`
	// Documents: compared (a vector of the embed model); WithoutVector: arranged by
	// a scheme but with no document annotation vector, left out.
	Documents     int           `json:"documents"`
	WithoutVector int           `json:"without_vector"`
	Levels        []assignLevel `json:"levels"`
	// Page and NextCursor: the report form's documents, by id.
	Page       []assignDoc `json:"page,omitempty"`
	NextCursor string      `json:"next_cursor,omitempty"`
	// Proposals: the propose form's eligible rankings, largest margin first, with
	// each outcome; Proposed and Skipped count them.
	Proposals []assignProposal `json:"proposals,omitempty"`
	Proposed  int              `json:"proposed,omitempty"`
	Skipped   map[string]int   `json:"skipped,omitempty"`
	Truncated bool             `json:"truncated,omitempty"`
	// MembersSkipped: vectors that could not be compared (zero, non-finite, another
	// dimension). Issues: comparisons that failed, left out.
	MembersSkipped int      `json:"members_skipped,omitempty"`
	Issues         []string `json:"issues,omitempty"`
}

// assignMargins resolves the call's margins over the defaults: a level named with a
// margin from 0 to 1 sets it, with a negative one (or null) turns it off. Keys are
// levels 1..SchemeMaxDepth.
func assignMargins(in map[string]*float64) (map[int]float64, string) {
	out := map[int]float64{}
	for l, m := range assignDefaultMargins {
		out[l] = m
	}
	for k, v := range in {
		level, err := strconv.Atoi(k)
		if err != nil || level < 1 || level > memrank.SchemeMaxDepth {
			return nil, fmt.Sprintf("margins: %q is not a level from 1 to %d", k, memrank.SchemeMaxDepth)
		}
		if v != nil && (math.IsNaN(*v) || *v > 1) {
			return nil, fmt.Sprintf("margins: level %d's margin must be at most 1", level)
		}
		if v == nil || *v < 0 {
			delete(out, level)
			continue
		}
		out[level] = *v
	}
	return out, ""
}

// assignConcept is what the walk knows of one concept.
type assignConcept struct {
	id, name, tag, scheme string
	level                 int
}

// assignPass is one call's state, every read made before any proposal.
type assignPass struct {
	p       *nodeStatsPlan
	vecs    map[string]nodestats.Vector // usable document vectors, by document
	titles  map[string]string
	roots   map[string]string
	member  map[string]map[string]bool // document → the concepts it is placed under live (rolled up)
	members map[string][]string        // concept → its members, sorted (the plan's)
	concept map[string]assignConcept
	groups  map[string][]string // group key → its concept ids
	rankers map[string]*nodestats.Ranker
	margins map[int]float64
	issues  []string
}

func assignGroupKey(parent string) string { return "concept\x00" + parent }

// ranker is the group's ranker, built once; nil when it compares fewer than two
// concepts.
func (a *assignPass) ranker(group string) *nodestats.Ranker {
	if r, ok := a.rankers[group]; ok {
		return r
	}
	var nodes []nodestats.Node
	for _, id := range a.groups[group] {
		n := nodestats.Node{ID: id}
		for _, m := range a.members[id] {
			if v, ok := a.vecs[m]; ok && a.p.averaged(m) {
				n.Members = append(n.Members, nodestats.Member{ID: m, Vec: v})
			}
		}
		nodes = append(nodes, n)
	}
	r, err := nodestats.NewRanker(nodes, nodestats.MinMembers)
	if err != nil {
		_, parent, _ := strings.Cut(group, "\x00")
		a.issues = append(a.issues, fmt.Sprintf("the concepts under %s were not compared: %v", parent, err))
		r = nil
	}
	if r != nil && r.Len() < 2 {
		r = nil
	}
	a.rankers[group] = r
	return r
}

// rank walks one document down scheme s, from its top level.
func (a *assignPass) rank(doc string, s memrank.OntologyScheme) []assignRanking {
	type step struct {
		parent string
		level  int
	}
	var out []assignRanking
	queue := []step{{parent: "", level: 1}}
	seen := map[string]bool{}
	for len(queue) > 0 {
		st := queue[0]
		queue = queue[1:]
		parent := st.parent
		if parent == "" {
			parent = s.Name
		}
		group := assignGroupKey(parent)
		if seen[group] || st.level > memrank.SchemeMaxDepth {
			continue
		}
		seen[group] = true
		r := a.ranker(group)
		if r == nil {
			continue
		}
		scores, err := r.Rank(doc, a.vecs[doc])
		if err != nil {
			a.issues = append(a.issues, fmt.Sprintf("document %s was not compared under %s: %v", doc, parent, err))
			continue
		}
		if len(scores) < 2 {
			continue
		}
		nearest, second := a.concept[scores[0].ID], a.concept[scores[1].ID]
		rk := assignRanking{Scheme: s.Name, Level: st.level, Parent: st.parent, Compared: len(scores),
			Nearest: assignNode{NodeID: nearest.id, Node: nearest.name, Tag: nearest.tag, Similarity: scores[0].Score},
			Second:  assignNode{NodeID: second.id, Node: second.name, Tag: second.tag, Similarity: scores[1].Score},
			Margin:  scores[0].Score - scores[1].Score}
		placed := false
		for _, id := range a.groups[group] {
			placed = placed || a.member[doc][id]
		}
		if placed {
			agrees := a.member[doc][nearest.id]
			rk.Agrees = &agrees
		}
		if t, ok := a.margins[st.level]; ok {
			thr := t
			rk.Threshold = &thr
			rk.Eligible = rk.Margin >= t && !a.member[doc][nearest.id]
		}
		out = append(out, rk)
		for _, id := range a.groups[group] {
			if a.member[doc][id] || (rk.Eligible && id == nearest.id) {
				queue = append(queue, step{parent: a.concept[id].tag, level: st.level + 1})
			}
		}
	}
	return out
}

// NodeAssign runs mode assign over the tenant scope key names. The caller has
// checked who may run it and, for propose, who may file.
func (d *Document) NodeAssign(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, in docInput) (NodeAssignReport, error) {
	margins, why := assignMargins(in.Margins)
	if why != "" {
		return NodeAssignReport{}, fmt.Errorf("%s", why)
	}
	ans := NodeAssignReport{Mode: nodeStatsModeAssign, Scheme: in.Scheme, MinMembers: nodestats.MinMembers,
		Margins: map[string]float64{}, Propose: in.Propose, CreatedAfter: in.CreatedAfter, Levels: []assignLevel{}}
	for l, m := range margins {
		ans.Margins[strconv.Itoa(l)] = m
	}
	if in.Propose {
		ans.Cap = misfitCap(in.Limit)
	}
	p, err := d.buildNodeStatsPlan(ctx, key, read, in.Scheme)
	if err != nil {
		return ans, err
	}
	ans.Vectors, ans.Note, ans.EmbedModel = p.model != "", p.note, p.model
	if p.model == "" {
		ans.Note = "assignment needs the vector stage, which does not run here: " + p.note
		return ans, nil
	}
	a, docsBy, err := d.assignPlan(ctx, key, read, p, in, margins)
	if err != nil {
		return ans, err
	}
	ans.MembersSkipped = a.skippedVectors
	var ids []string
	for id := range docsBy {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	levels := map[int]*assignLevel{}
	var eligible []assignProposal
	var page []assignDoc
	pageCap := assignPageDefault
	if !in.Propose && in.Limit > 0 {
		pageCap = min(in.Limit, assignPageMax)
	}
	for _, doc := range ids {
		schemes := docsBy[doc]
		if _, ok := a.vecs[doc]; !ok {
			ans.WithoutVector++
			continue
		}
		ans.Documents++
		var rankings []assignRanking
		for _, s := range schemes {
			rankings = append(rankings, a.rank(doc, s)...)
		}
		for _, rk := range rankings {
			lv := levels[rk.Level]
			if lv == nil {
				lv = newAssignLevel(rk.Level, margins)
				levels[rk.Level] = lv
			}
			lv.count(rk)
			if rk.Eligible {
				eligible = append(eligible, assignProposal{DocumentID: doc, Title: a.titles[doc], assignRanking: rk})
			}
		}
		if !in.Propose && doc > in.After && len(page) < pageCap {
			page = append(page, assignDoc{DocumentID: doc, Title: a.titles[doc], Placements: a.placements(doc), Rankings: nonNilRankings(rankings)})
			if len(page) == pageCap {
				ans.NextCursor = doc
			}
		}
	}
	// A full page whose last document is the last compared has nothing after it.
	if ans.NextCursor != "" && !hasComparedAfter(ids, ans.NextCursor, a.vecs) {
		ans.NextCursor = ""
	}
	for _, l := range sortedLevels(levels) {
		ans.Levels = append(ans.Levels, *levels[l])
	}
	ans.Issues = a.issues
	if !in.Propose {
		ans.Page = page
		if ans.Page == nil {
			ans.Page = []assignDoc{}
		}
		return ans, nil
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Margin != eligible[j].Margin {
			return eligible[i].Margin > eligible[j].Margin
		}
		if eligible[i].DocumentID != eligible[j].DocumentID {
			return eligible[i].DocumentID < eligible[j].DocumentID
		}
		return eligible[i].Nearest.NodeID < eligible[j].Nearest.NodeID
	})
	ans.Proposals, ans.Skipped = eligible, map[string]int{}
	if err := d.proposeAssignments(ctx, key, a, &ans); err != nil {
		return ans, err
	}
	if ans.Proposals == nil {
		ans.Proposals = []assignProposal{}
	}
	if len(ans.Proposals) > misfitReportCap {
		ans.Proposals, ans.Truncated = ans.Proposals[:misfitReportCap], true
	}
	return ans, nil
}

func nonNilRankings(r []assignRanking) []assignRanking {
	if r == nil {
		return []assignRanking{}
	}
	return r
}

func hasComparedAfter(ids []string, after string, vecs map[string]nodestats.Vector) bool {
	for _, id := range ids {
		if id > after {
			if _, ok := vecs[id]; ok {
				return true
			}
		}
	}
	return false
}

func sortedLevels(m map[int]*assignLevel) []int {
	out := make([]int, 0, len(m))
	for l := range m {
		out = append(out, l)
	}
	sort.Ints(out)
	return out
}

func newAssignLevel(level int, margins map[int]float64) *assignLevel {
	lv := &assignLevel{Level: level}
	if t, ok := margins[level]; ok {
		lv.Threshold = &t
	}
	for i := 0; i < assignBucketCount; i++ {
		from := float64(i) * assignBucketWidth
		var to *float64
		if i < assignBucketCount-1 {
			t := float64(i+1) * assignBucketWidth
			to = &t
		}
		lv.Buckets = append(lv.Buckets, assignBucket{From: from, To: to})
		lv.AtLeast = append(lv.AtLeast, assignBucket{From: from})
	}
	return lv
}

// count adds one ranking to the level's table. A margin is never negative (the
// nearest leads), so bucket i holds [i×width, (i+1)×width), the last no upper bound.
func (lv *assignLevel) count(rk assignRanking) {
	add := func(b *assignBucket) {
		b.Rankings++
		if rk.Agrees != nil {
			b.Placed++
			if *rk.Agrees {
				b.Agree++
			}
			share := float64(b.Agree) / float64(b.Placed)
			b.Agreement = &share
		}
	}
	lv.Rankings++
	if rk.Agrees != nil {
		lv.Placed++
		if *rk.Agrees {
			lv.Agree++
		}
	}
	if rk.Eligible {
		lv.Eligible++
	}
	// The bucket index is taken on a rounded margin, so 0.03 computed as
	// 0.0299999… falls in the bucket a reader expects.
	i := int(math.Floor(math.Round(rk.Margin*1e9)/1e9/assignBucketWidth + 1e-9))
	i = max(0, min(i, assignBucketCount-1))
	add(&lv.Buckets[i])
	for j := 0; j <= i; j++ {
		add(&lv.AtLeast[j])
	}
}

// placements is the most specific concepts doc is placed under live.
func (a *assignPass) placements(doc string) []assignPlacement {
	out := []assignPlacement{}
	for id := range a.member[doc] {
		c, ok := a.concept[id]
		if !ok {
			continue
		}
		narrower := false
		for o := range a.member[doc] {
			if oc, ok := a.concept[o]; ok && oc.scheme == c.scheme && strings.HasPrefix(oc.tag, c.tag+"/") {
				narrower = true
				break
			}
		}
		if !narrower {
			out = append(out, assignPlacement{NodeID: id, Node: c.name, Tag: c.tag})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// assignPlan reads what the walk needs: the documents each scheme arranges
// (unrestricted, not the ontology, created after the bound), their titles, roots
// and annotation vectors, and the concepts with their live members. Reads only.
func (d *Document) assignPlan(ctx context.Context, key sqlmem.ScopeKey, read OntologyRead, p *nodeStatsPlan, in docInput, margins map[int]float64) (*assignPassResult, map[string][]memrank.OntologyScheme, error) {
	a := &assignPassResult{assignPass: assignPass{p: p, titles: map[string]string{}, roots: map[string]string{},
		member: map[string]map[string]bool{}, members: map[string][]string{}, concept: map[string]assignConcept{},
		groups: map[string][]string{}, rankers: map[string]*nodestats.Ranker{}, margins: margins}}
	for _, n := range p.nodes {
		if n.Kind != nodeKindConcept {
			continue
		}
		a.groups[n.group] = append(a.groups[n.group], n.ID)
		a.members[n.ID] = n.members
		for _, m := range n.members {
			if a.member[m] == nil {
				a.member[m] = map[string]bool{}
			}
			a.member[m][n.ID] = true
		}
	}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			if c.ChunkID != "" {
				a.concept[c.ChunkID] = assignConcept{id: c.ChunkID, name: c.Name, tag: c.Tag, scheme: s.Name, level: len(c.Path)}
			}
		}
	}
	all, err := d.loadSeriesDocs(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	classes := describeOntologyRead(read).Classes
	docsBy := map[string][]memrank.OntologyScheme{}
	for _, s := range read.Schemes {
		if in.Scheme != "" && s.Name != in.Scheme {
			continue
		}
		proj := projectedClasses(s.Projects, classes)
		for id, sd := range all {
			if id == read.DocumentID || !proj[sd.typ] || !p.unrestricted.readable(id) {
				continue
			}
			if in.CreatedAfter != nil && sd.created <= *in.CreatedAfter {
				continue
			}
			docsBy[id] = append(docsBy[id], s)
		}
	}
	// Vectors: members (for the centroids) and the documents compared.
	need := map[string]bool{}
	for id := range docsBy {
		need[id] = true
	}
	for m := range a.member {
		need[m] = true
	}
	docs := map[string]*statDoc{}
	for id, sd := range p.docs {
		docs[id] = sd
	}
	var missing []string
	for id := range need {
		if docs[id] == nil {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if err := d.loadStatDocs(ctx, key, missing, docs, p.tierCol); err != nil {
		return nil, nil, err
	}
	q := &nodeStatsPlan{docs: docs, model: p.model}
	raw, err := d.loadNodeStatsVectors(ctx, key, direntTenant(ctx), q)
	if err != nil {
		return nil, nil, err
	}
	dim := q.misfitVectorDim()
	a.vecs = map[string]nodestats.Vector{}
	for id, v := range raw {
		if !need[id] {
			continue
		}
		if !usableMisfitVector(v, dim) {
			a.skippedVectors++
			continue
		}
		a.vecs[id] = v
	}
	for id, sd := range docs {
		a.titles[id], a.roots[id] = sd.title, sd.root
	}
	return a, docsBy, nil
}

// assignPassResult is the pass with the count of vectors it could not compare.
type assignPassResult struct {
	assignPass
	skippedVectors int
}

// proposeAssignments files the eligible rankings, in the answer's order, until the
// cap, as proposeMisfits does: every read first, then one propose_statement each.
func (d *Document) proposeAssignments(ctx context.Context, key sqlmem.ScopeKey, a *assignPassResult, ans *NodeAssignReport) error {
	var docs, targets []string
	seenDoc, seenTarget := map[string]bool{}, map[string]bool{}
	for _, pr := range ans.Proposals {
		if !seenDoc[pr.DocumentID] {
			seenDoc[pr.DocumentID] = true
			docs = append(docs, pr.DocumentID)
		}
		if !seenTarget[pr.Nearest.NodeID] {
			seenTarget[pr.Nearest.NodeID] = true
			targets = append(targets, pr.Nearest.NodeID)
		}
	}
	blocked, err := d.misfitBlockedPlacements(ctx, key, docs, targets)
	if err != nil {
		return err
	}
	rootTitles, err := d.rootTitles(ctx, key, &nodeStatsPlan{docs: rootDocs(a.roots)}, docs)
	if err != nil {
		return err
	}
	filed := map[string]bool{}
	for i := range ans.Proposals {
		pr := &ans.Proposals[i]
		pair := pr.DocumentID + "\x00" + pr.Nearest.NodeID
		switch {
		case blocked[pair] != "":
			pr.Skipped = blocked[pair]
		case filed[pair]:
			pr.Skipped = "duplicate"
		case ans.Proposed >= ans.Cap:
			pr.Skipped = "cap"
		}
		if pr.Skipped != "" {
			ans.Skipped[pr.Skipped]++
			continue
		}
		filed[pair] = true
		d.proposeAssignment(ctx, a.roots[pr.DocumentID], rootTitles[pr.DocumentID], pr)
		if pr.Skipped != "" {
			ans.Skipped[strings.SplitN(pr.Skipped, ":", 2)[0]]++
			continue
		}
		ans.Proposed++
	}
	return nil
}

func rootDocs(roots map[string]string) map[string]*statDoc {
	out := make(map[string]*statDoc, len(roots))
	for id, r := range roots {
		out[id] = &statDoc{root: r}
	}
	return out
}

// proposeAssignment files one placement. Confidence is the margin (at most 1): the
// signal P0 measured, which a tenant review rule on method rule:centroid reads.
func (d *Document) proposeAssignment(ctx context.Context, root, rootTitle string, pr *assignProposal) {
	conf := math.Min(1, math.Max(0, pr.Margin))
	in := docInput{
		Op: "propose_statement", Scope: "tenant", FromID: root, ToID: pr.Nearest.NodeID, Kind: "subject",
		Confidence: &conf, edgeMethod: misfitMethod, proposeOnly: true,
		Attributes: map[string]any{"assigned_by": nodeStatsModeAssign, "level": pr.Level, "margin": pr.Margin,
			"nearest_similarity": pr.Nearest.Similarity, "second": pr.Second.NodeID, "second_similarity": pr.Second.Similarity},
		Evidence: misfitEvidence(root, rootTitle),
	}
	res, err := d.executeInput(ctx, in, nil)
	if err != nil || res.IsError {
		code := "error"
		if err == nil && res.Error != nil {
			code = string(res.Error.Category)
		}
		pr.Skipped = "refused:" + code
		if err == nil {
			pr.Detail = res.Text
		}
		return
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	pr.StatementID, pr.Status = out.ID, out.Status
}
