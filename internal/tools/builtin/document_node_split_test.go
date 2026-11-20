package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// splitOntologyMD is conceptOntologyMD with two more top-level topics, audit and
// auditing, for a merge.
var splitOntologyMD = strings.Replace(conceptOntologyMD, "#### reporting\n\n### file-plan",
	"#### reporting\n\n#### audit\n\n#### auditing\n\n### file-plan", 1)

// splitCorpus is a statsCorpus (4-dimensional vectors) with documents the split
// tests add.
type splitCorpus struct {
	*statsCorpus
	audit, auditing string
	n               int
}

func newSplitCorpus(t *testing.T, d *Document, base context.Context, md string) *splitCorpus {
	t.Helper()
	k := &splitCorpus{statsCorpus: newStatsCorpusMD(t, d, base, md)}
	k.audit, k.auditing = k.f.concepts["topics/audit"], k.f.concepts["topics/auditing"]
	return k
}

// doc adds a document of type typ ("" untyped: projected by no scheme, so never
// unplaced) with a document annotation brief and vector vec, placed under concept
// ("" nowhere) and dated at (zero: undated).
func (k *splitCorpus) doc(t *testing.T, typ, brief string, vec []float32, concept string, at time.Time) string {
	t.Helper()
	k.n++
	v := map[string]any{"op": "create_document", "scope": "tenant", "title": fmt.Sprintf("Split doc %d %s", k.n, brief),
		"path": fmt.Sprintf("/kb/s%d", k.n)}
	if typ != "" {
		v["type"] = typ
	}
	out := k.f.c.exec(v)
	doc := out["document_id"].(string)
	k.docs[doc] = out["root_chunk_id"].(string)
	if concept != "" {
		k.f.link("link_chunks", k.f.chunk("tenant", doc), concept, "subject")
	}
	if !at.IsZero() {
		g := k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": k.docs[doc]})
		k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": k.docs[doc],
			"valid_at": at.UnixNano(), "revision": g["revision"]})
	}
	k.annotate(t, doc, brief, vec)
	return doc
}

// twoTopics adds 12 documents near (0,0,1,0) about filing deadlines and 12 near
// (0,0,0,1) about templates, each typed typ, placed under concept and dated by
// date(i) (nil: undated). It returns the two sets.
func (k *splitCorpus) twoTopics(t *testing.T, typ, concept string, date func(int) time.Time) (filing, templates []string) {
	t.Helper()
	for i := 0; i < 12; i++ {
		e, f := float32(i%4)*0.05, float32(i%3)*0.04
		var at time.Time
		if date != nil {
			at = date(i)
		}
		filing = append(filing, k.doc(t, typ, "quarterly filing deadlines", []float32{0, e, 1, f}, concept, at))
		templates = append(templates, k.doc(t, typ, "xbrl template taxonomy", []float32{f, 0, e, 1}, concept, at))
	}
	return filing, templates
}

// ring adds 24 documents spread evenly round a circle, placed under concept: k-means
// partitions them, but differently for each seed and sample, so no group recurs.
func (k *splitCorpus) ring(t *testing.T, concept string) {
	t.Helper()
	for i := 0; i < 24; i++ {
		a := float64(i) * 2 * math.Pi / 24
		k.doc(t, "", "statute clause", []float32{0, float32(math.Cos(a)), 0, float32(math.Sin(a))}, concept, time.Time{})
	}
}

func splitRun(t *testing.T, d *Document, ctx context.Context, body string) NodeSplitReport {
	t.Helper()
	_, res := docExec(t, d, ctx, body)
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out NodeSplitReport
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Text)
	}
	return out
}

func (a NodeSplitReport) split(t *testing.T, node string) splitNode {
	t.Helper()
	for _, s := range a.Splits {
		if s.NodeID == node {
			return s
		}
	}
	t.Fatalf("no split of %s in %+v", node, a.Splits)
	return splitNode{}
}

func (a NodeSplitReport) unplaced(t *testing.T, scheme string) unplacedGrouping {
	t.Helper()
	for _, u := range a.Unplaced {
		if u.Scheme == scheme {
			return u
		}
	}
	t.Fatalf("no unplaced grouping of %s in %+v", scheme, a.Unplaced)
	return unplacedGrouping{}
}

// groupOf is the group holding doc.
func groupOf(t *testing.T, groups []splitGroup, doc string) splitGroup {
	t.Helper()
	for _, g := range groups {
		for _, m := range g.Documents {
			if m.DocumentID == doc {
				return g
			}
		}
	}
	t.Fatalf("no group holds %s: %+v", doc, groups)
	return splitGroup{}
}

// ontologyProposals is every proposal of the tenant ontology.
func ontologyProposals(t *testing.T, d *Document, ctx context.Context) []memrank.OntologyProposal {
	t.Helper()
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("ontology unreadable")
	}
	return read.Proposals
}

// TestSplit_OnlyARecurringGroupIsProposed — reporting holds two clear topics, which
// every re-clustering finds again: both are proposed as narrower concepts of
// reporting, named from their distinctive terms. Law holds documents spread round a
// circle: k-means splits them, but no group recurs, so the split is reported with
// its groups not recurring and nothing is proposed under Law.
func TestSplit_OnlyARecurringGroupIsProposed(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	filing, templates := k.twoTopics(t, "", k.rep, nil)
	k.ring(t, k.law)
	ans := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","propose":true}`)
	if !ans.Vectors || ans.Runs != nodestats.RecurRuns || ans.RecurMin != nodestats.RecurMin || ans.ModelCheck == "" {
		t.Errorf("answer header = %+v", ans)
	}
	rep := ans.split(t, k.rep)
	if len(rep.Groups) != 2 || rep.NotRecurring != 0 {
		t.Fatalf("reporting = %+v; want two recurring groups", rep)
	}
	gf, gt := groupOf(t, rep.Groups, filing[0]), groupOf(t, rep.Groups, templates[0])
	// The corpus's own reports r1..r3 sit under reporting near the filing topic.
	if gf.Members != 15 || gt.Members != 12 {
		t.Errorf("groups hold %d and %d; want 15 (12 + r1..r3) and 12", gf.Members, gt.Members)
	}
	for _, g := range []splitGroup{gf, gt} {
		if g.Recurrence != nodestats.RecurRuns || g.ParentID != k.rep || g.ProposalID == "" || g.Skipped != "" {
			t.Errorf("group %+v; want recurring in every run, filed under reporting", g)
		}
	}
	if !strings.Contains(gf.Name, "filing") || !strings.Contains(gt.Name, "template") {
		t.Errorf("names %q / %q; want them from the distinctive terms", gf.Name, gt.Name)
	}
	law := ans.split(t, k.law)
	if len(law.Groups) != 0 || law.NotRecurring < 2 {
		t.Errorf("Law = %+v; want its groups reported as not recurring", law)
	}
	byID := map[string]memrank.OntologyProposal{}
	for _, p := range ontologyProposals(t, d, k.f.operator) {
		byID[p.ChunkID] = p
	}
	if len(byID) != 2 {
		t.Fatalf("proposals = %+v; want the two reporting groups", byID)
	}
	for _, g := range []splitGroup{gf, gt} {
		p := byID[g.ProposalID]
		if p.Status != memrank.OntologyStatusProposed || p.Section != memrank.SectionSchemes || p.Parent != "reporting" ||
			!strings.EqualFold(p.Name, g.Name) || !strings.Contains(p.Body, "No model grouping") {
			t.Errorf("proposal %+v for group %s", p, g.Name)
		}
	}
	if ans.Proposed != 2 {
		t.Errorf("proposed %d, want 2", ans.Proposed)
	}
	// A second pass files nothing again.
	again := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","propose":true}`)
	if again.Proposed != 0 || again.Skipped["exists"] != 2 {
		t.Errorf("second pass = proposed %d skipped %v; want both exists", again.Proposed, again.Skipped)
	}
}

// TestSplit_WithoutChangeProposalsIsReportedNotFiled — with RFC GQ in, a merge is an
// ontology change and is only ever filed as GQ's retire proposal, never written:
// without propose, the merge and the split groups are reported and nothing is filed
// or changed; with propose, the merge is a retire proposal (a change_proposal mark
// on an inert chunk) and the concept stays in force with its placements.
func TestSplit_WithoutChangeProposalsIsReportedNotFiled(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, splitOntologyMD)
	k.twoTopics(t, "", k.rep, nil)
	for i := 0; i < 4; i++ {
		k.doc(t, "", "internal audit plan", []float32{0.1 * float32(i%2), 1, 0, 0.05}, k.audit, time.Time{})
	}
	for i := 0; i < 3; i++ {
		k.doc(t, "", "internal audit review", []float32{0.05, 1, 0.1 * float32(i%2), 0}, k.auditing, time.Time{})
	}
	before := ontologySnapshot(t, d, k.f.operator)
	rep := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split"}`)
	if len(rep.Merges) != 1 || rep.Merges[0].NodeID != k.auditing || rep.Merges[0].IntoID != k.audit {
		t.Fatalf("merges = %+v; want auditing (3 members) into audit (4)", rep.Merges)
	}
	if rep.Merges[0].Similarity < mergeSimilarity || rep.Merges[0].ProposalID != "" || rep.Merges[0].Skipped != "" {
		t.Errorf("merge = %+v", rep.Merges[0])
	}
	if len(rep.split(t, k.rep).Groups) != 2 || rep.Proposed != 0 || rep.Skipped != nil {
		t.Errorf("report = %+v; want the split reported, nothing filed", rep)
	}
	if after := ontologySnapshot(t, d, k.f.operator); !reflect.DeepEqual(before, after) {
		t.Fatalf("a report changed the ontology:\n%v\n%v", before, after)
	}

	filed := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","propose":true}`)
	m := filed.Merges[0]
	if m.ProposalID == "" || m.Skipped != "" {
		t.Fatalf("merge = %+v; want a retire proposal filed", m)
	}
	var change *memrank.ProposedChange
	for _, p := range ontologyProposals(t, d, k.f.operator) {
		if p.ChunkID == m.ProposalID {
			change = p.Change
		}
	}
	if change == nil || change.Op != memrank.ChangeOpRetire || change.Target != k.auditing || filed.Proposed != 3 {
		t.Fatalf("filed change %+v, proposed %d; want a retire of auditing and two adds", change, filed.Proposed)
	}
	read, _ := d.tenantOntologyRead(k.f.operator)
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			if c.ChunkID == k.auditing && c.EntryStatus != "" {
				t.Errorf("auditing is %s; a proposal must leave it in force", c.EntryStatus)
			}
		}
	}
	if n := growthCount(t, d, k.f.operator, `SELECT count(*) FROM chunk_edges WHERE to_id = ? AND status = 'asserted' AND expired_at IS NULL`, k.auditing); n != 3 {
		t.Errorf("auditing's placements = %d, want 3", n)
	}
}

// TestUnplaced_AThingFarFromEveryGroupStaysUnplaced — a scheme's unplaced things are
// grouped; a thing far from every group's centre stays unplaced and is listed so,
// and is in no proposal. The group near Risk Management is filed under it, the
// other at the top of the scheme.
func TestUnplaced_AThingFarFromEveryGroupStaysUnplaced(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	var vendor, filing []string
	for i := 0; i < 12; i++ {
		e, f := float32(i%4)*0.05, float32(i%3)*0.04
		vendor = append(vendor, k.doc(t, "Document", "vendor concentration", []float32{1, 0.15 + e, f, 0}, "", time.Time{}))
		filing = append(filing, k.doc(t, "Document", "statute digest", []float32{f, 0, e, 1}, "", time.Time{}))
	}
	far := k.doc(t, "Document", "cafeteria menu", []float32{0, 1, 0, 0}, "", time.Time{})
	ans := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","scheme":"topics","propose":true}`)
	u := ans.unplaced(t, "topics")
	if !u.Grouped || u.Things != 25 || u.Clustered != 25 || len(u.Groups) != 2 {
		t.Fatalf("topics unplaced = %+v", u)
	}
	gv, gs := groupOf(t, u.Groups, vendor[0]), groupOf(t, u.Groups, filing[0])
	if gv.ParentID != k.rm || gv.ParentSimilarity == nil || *gv.ParentSimilarity < unplacedFarSimilarity {
		t.Errorf("vendor group = %+v; want filed under Risk Management", gv)
	}
	if gs.ParentID != u.SchemeID || gs.Parent != "topics" || gs.ParentSimilarity != nil {
		t.Errorf("statute group = %+v; want the top of topics", gs)
	}
	if gv.Members != 12 || gs.Members != 12 {
		t.Errorf("groups hold %d and %d; want 12 each, the far thing in neither", gv.Members, gs.Members)
	}
	if len(u.StaysUnplaced) != 1 || u.StaysUnplaced[0].DocumentID != far || u.StaysUnplaced[0].Similarity >= unplacedFarSimilarity {
		t.Errorf("stays unplaced = %+v; want only the far thing", u.StaysUnplaced)
	}
	for _, g := range u.Groups {
		if g.ProposalID == "" {
			t.Errorf("group %+v not filed", g)
		}
		for _, m := range g.Documents {
			if m.DocumentID == far {
				t.Errorf("the far thing is in group %s", g.Name)
			}
		}
	}
	for _, p := range ontologyProposals(t, d, k.f.operator) {
		if strings.Contains(p.Body, far) {
			t.Errorf("proposal %s cites the far thing", p.Name)
		}
	}
}

// TestUnplaced_GroupsWhenNoMemberHasAVector — a new scheme's things are all
// unplaced and no concept member has a vector of the model: the unplaced things are
// still grouped (the vector dimension is taken from them too, not from members alone).
func TestUnplaced_GroupsWhenNoMemberHasAVector(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	for doc := range k.vecs {
		k.setVector(t, doc, k.vecs[doc], "m0") // another model: no member vector of m1
	}
	for i := 0; i < 12; i++ {
		e, f := float32(i%4)*0.05, float32(i%3)*0.04
		k.doc(t, "Document", "vendor concentration", []float32{1, 0.15 + e, f, 0}, "", time.Time{})
		k.doc(t, "Document", "statute digest", []float32{f, 0, e, 1}, "", time.Time{})
	}
	u := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","scheme":"topics"}`).unplaced(t, "topics")
	if u.Clustered != 24 || !u.Grouped || len(u.Groups) != 2 {
		t.Errorf("topics unplaced = %+v; want 24 clustered into two groups", u)
	}
	for _, g := range u.Groups {
		if g.ParentID != u.SchemeID {
			t.Errorf("group %s under %s; with no concept centroid it goes to the top of the scheme", g.Name, g.Parent)
		}
	}
}

// TestSplit_NewNodesHaveAHistoryAndTheParentSeriesIsUnchanged — a split group
// accepted as a narrower concept, with its documents placed under it, gets count
// points back over every period its members are dated in, and the split node's
// rolled-up series is unchanged (RFC GM 6A.7).
func TestSplit_NewNodesHaveAHistoryAndTheParentSeriesIsUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	filing, _ := k.twoTopics(t, "", k.rep, func(i int) time.Time { return monthsAgo(i % 4) })
	nodeStatsRun(t, d, k.f.operator, computeAll)
	countsOf := func(node string) map[int64]float64 {
		out := map[int64]float64{}
		for _, r := range measureRows(t, d, k.f.operator) {
			if r.Measure == measureCount && r.Node == node {
				out[r.Start] = r.Value // the latest row of a point wins: rows are appended
			}
		}
		return out
	}
	parentBefore := countsOf(k.rep)
	ans := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","propose":true}`)
	g := groupOf(t, ans.split(t, k.rep).Groups, filing[0])
	if g.ProposalID == "" {
		t.Fatalf("group not filed: %+v", g)
	}
	got := k.f.c.exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": g.ProposalID})
	k.f.c.exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": g.ProposalID, "status": "", "revision": got["revision"]})
	read, _ := d.tenantOntologyRead(k.f.operator)
	child := ""
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			if c.ChunkID == g.ProposalID && strings.HasPrefix(c.Tag, "topics/reporting/") {
				child = c.ChunkID
			}
		}
	}
	if child == "" {
		t.Fatalf("the accepted group is not a concept under reporting: %+v", read.Schemes)
	}
	months := map[int64]bool{}
	for _, m := range g.Documents {
		k.f.link("link_chunks", k.f.chunk("tenant", m.DocumentID), child, "subject")
	}
	for i := 0; i < 4; i++ {
		months[periodStart(monthsAgo(i).UnixNano(), memrank.SchemePeriodMonth)] = true
	}
	nodeStatsRun(t, d, k.f.operator, computeAll)
	history := countsOf(child)
	if len(history) != len(months) {
		t.Errorf("the new concept's count points = %v; want one per month its members are dated in (%d)", history, len(months))
	}
	total := 0.0
	for start, v := range history {
		if !months[start] {
			t.Errorf("a point at %d, outside its members' months", start)
		}
		total += v
	}
	if int(total) != g.Members {
		t.Errorf("the new concept's points sum to %v, want its %d members", total, g.Members)
	}
	if after := countsOf(k.rep); !reflect.DeepEqual(parentBefore, after) {
		t.Errorf("reporting's series changed:\n%v\n%v", parentBefore, after)
	}
}

// TestSplit_ProposingNeedsTheRuleGrant — filing is an operator's or a run granted
// rule, as for misfits; an extractor-only run reads the candidates and is refused
// propose with nothing filed.
func TestSplit_ProposingNeedsTheRuleGrant(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	k.twoTopics(t, "", k.rep, nil)
	run := func(sm ...string) context.Context {
		return tools.WithRunID(tools.WithRunPolicy(base, tools.RunPolicy{
			Tools:            []string{"*"},
			Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
			SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
			StatementMethods: sm,
		}), "run-split")
	}
	extractor := run("extractor")
	if rep := splitRun(t, d, extractor, `{"op":"node_stats","mode":"split"}`); len(rep.Splits) == 0 {
		t.Fatalf("an extractor-granted run reads no candidate: %+v", rep)
	}
	_, res := docExec(t, d, extractor, `{"op":"node_stats","mode":"split","propose":true}`)
	if !res.IsError || !strings.Contains(res.Text, "method_not_granted") {
		t.Errorf("an extractor-granted run filed: %s", res.Text)
	}
	if n := len(ontologyProposals(t, d, k.f.operator)); n != 0 {
		t.Fatalf("the refused call filed %d proposals", n)
	}
	ruled := splitRun(t, d, run("rule"), `{"op":"node_stats","mode":"split","propose":true,"limit":1}`)
	if ruled.Proposed != 1 || ruled.Cap != 1 || ruled.Skipped["cap"] != 1 {
		t.Errorf("a rule-granted run = proposed %d cap %d skipped %v; want 1, 1, {cap:1}", ruled.Proposed, ruled.Cap, ruled.Skipped)
	}
	if n := len(ontologyProposals(t, d, k.f.operator)); n != 1 {
		t.Errorf("filed %d proposals, want 1", n)
	}
	if a := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","propose":true,"limit":100000}`); a.Cap != splitProposalMax {
		t.Errorf("cap for a huge limit = %d, want %d", a.Cap, splitProposalMax)
	}
}

// TestSplit_AdoptedOrLockedConceptIsNeverRetired — a merge that would retire a
// concept of an adopted scheme, or a @locked one, is reported fixed and never filed;
// a local concept may still be added under an adopted one.
func TestSplit_AdoptedOrLockedConceptIsNeverRetired(t *testing.T) {
	for name, md := range map[string]string{
		"adopted": strings.Replace(splitOntologyMD, "### topics\n\n", "### topics\n@origin adopted\n@source https://example.org/topics\n@version 2026.1\n\n", 1),
		"locked":  strings.Replace(splitOntologyMD, "#### auditing\n\n", "#### auditing\n@locked\n\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			d, base, _ := documentFixture(t)
			k := newSplitCorpus(t, d, base, md)
			k.twoTopics(t, "", k.rep, nil)
			for i := 0; i < 4; i++ {
				k.doc(t, "", "internal audit plan", []float32{0.1 * float32(i%2), 1, 0, 0.05}, k.audit, time.Time{})
			}
			for i := 0; i < 3; i++ {
				k.doc(t, "", "internal audit review", []float32{0.05, 1, 0.1 * float32(i%2), 0}, k.auditing, time.Time{})
			}
			ans := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","propose":true}`)
			if len(ans.Merges) != 1 || ans.Merges[0].Fixed != name || ans.Merges[0].Skipped != "fixed" || ans.Merges[0].ProposalID != "" {
				t.Fatalf("merges = %+v; want auditing fixed %s and not filed", ans.Merges, name)
			}
			for _, p := range ontologyProposals(t, d, k.f.operator) {
				if p.Change != nil {
					t.Errorf("a change proposal was filed: %+v", p.Change)
				}
			}
			if ans.Proposed != 2 {
				t.Errorf("proposed %d; want the two local concepts under reporting", ans.Proposed)
			}
		})
	}
}

// TestSplit_StoreWithoutVectorsReportsItCannotRun — on a store that keeps no vectors
// the mode says why, lists nothing and files nothing.
func TestSplit_StoreWithoutVectorsReportsItCannotRun(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	k.twoTopics(t, "", k.rep, nil)
	d.Store = k.f.c.vs.Store // the plain sqlite store: SupportsVectors is false
	ans := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","propose":true}`)
	if ans.Vectors || !strings.Contains(ans.Note, "keeps no vectors") || len(ans.Splits)+len(ans.Unplaced)+len(ans.Merges) != 0 || ans.Proposed != 0 {
		t.Errorf("answer = %+v; want no vectors, why, and nothing listed", ans)
	}
}

// TestSplit_InputAndAccessAreChecked — an unknown mode names split, propose is
// refused outside misfits and split, and a plain reader may not run it.
func TestSplit_InputAndAccessAreChecked(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	for body, want := range map[string]string{
		`{"op":"node_stats","mode":"nope"}`:                    "is not report, compute, misfits, growth or split",
		`{"op":"node_stats","mode":"growth","propose":true}`:   "propose is taken only with mode misfits or split",
		`{"op":"node_stats","mode":"split","quiet_periods":2}`: "quiet_periods is taken only with mode growth",
		`{"op":"node_stats","mode":"split","scheme":"nope"}`:   `no scheme "nope" is in force`,
	} {
		_, res := docExec(t, d, k.f.operator, body)
		if !res.IsError || !strings.Contains(res.Text, want) {
			t.Errorf("%s: %s; want %q", body, res.Text, want)
		}
	}
	if _, res := docExec(t, d, k.f.reader, `{"op":"node_stats","mode":"split"}`); !res.IsError || !strings.Contains(res.Text, "permission") {
		t.Errorf("a plain reader ran split: %s", res.Text)
	}
}

// TestNodeStats_AnswerIsUnchangedWithoutSplitOptions — plan §2, byte-identical when
// unused: report, compute, misfits and growth answer with the fields they had before
// mode split, and a split report writes nothing (ontology, node_stats rows, series).
func TestNodeStats_AnswerIsUnchangedWithoutSplitOptions(t *testing.T) {
	d, base, _ := documentFixture(t)
	k := newSplitCorpus(t, d, base, conceptOntologyMD)
	k.twoTopics(t, "Document", k.rep, nil)
	fields := func(body string) string {
		_, res := docExec(t, d, k.f.operator, body)
		if res.IsError {
			t.Fatalf("%s: %s", body, res.Text)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(res.Text), &top); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(top))
		for key := range top {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}
	for body, want := range map[string]string{
		`{"op":"node_stats"}`:                  "computed,embed_model,fresh,min_members,missing,mode,nodes,removed,series,stale,vectors",
		`{"op":"node_stats","mode":"misfits"}`: "count,embed_model,margin,min_members,misfits,mode,propose,vectors",
		`{"op":"node_stats","mode":"growth"}`:  "candidates,mode,note,quiet,quiet_periods,series_missing,series_stale,unplaced",
	} {
		if got := fields(body); got != want {
			t.Errorf("%s: fields %s, want %s", body, got, want)
		}
	}
	snap := ontologySnapshot(t, d, k.f.operator)
	rows, points := len(storedRows(t, d, k.f.operator)), len(measureRows(t, d, k.f.operator))
	if rep := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split"}`); len(rep.Splits) == 0 {
		t.Fatalf("precondition: a split candidate: %+v", rep)
	}
	if !reflect.DeepEqual(snap, ontologySnapshot(t, d, k.f.operator)) || rows != len(storedRows(t, d, k.f.operator)) ||
		points != len(measureRows(t, d, k.f.operator)) {
		t.Errorf("a split report wrote something")
	}
}

// TestOntologySchemes_SplitProposalsOnBothTiers — mode split on both SQL Memory tiers
// (the postgres half runs where LOOMCYCLE_TEST_SQLMEM_PG_DSN is set; the CI filter
// takes this prefix): recurring split groups filed as add proposals, unplaced things
// grouped with the far one left out, and a merge filed as a retire proposal.
func TestOntologySchemes_SplitProposalsOnBothTiers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		k := newSplitCorpus(t, d, ctx, splitOntologyMD)
		k.twoTopics(t, "", k.rep, nil)
		for i := 0; i < 4; i++ {
			k.doc(t, "", "internal audit plan", []float32{0.1 * float32(i%2), 1, 0, 0.05}, k.audit, time.Time{})
		}
		for i := 0; i < 3; i++ {
			k.doc(t, "", "internal audit review", []float32{0.05, 1, 0.1 * float32(i%2), 0}, k.auditing, time.Time{})
		}
		var vendor []string
		for i := 0; i < 12; i++ {
			e, f := float32(i%4)*0.05, float32(i%3)*0.04
			vendor = append(vendor, k.doc(t, "Document", "vendor concentration", []float32{1, 0.15 + e, f, 0}, "", time.Time{}))
			k.doc(t, "Document", "statute digest", []float32{f, 0, e, 1}, "", time.Time{})
		}
		far := k.doc(t, "Document", "cafeteria menu", []float32{0, 0.3, 0, -1}, "", time.Time{})
		ans := splitRun(t, d, k.f.operator, `{"op":"node_stats","mode":"split","scheme":"topics","propose":true}`)
		if len(ans.split(t, k.rep).Groups) != 2 {
			t.Errorf("reporting = %+v", ans.split(t, k.rep))
		}
		u := ans.unplaced(t, "topics")
		if len(u.Groups) != 2 || len(u.StaysUnplaced) != 1 || u.StaysUnplaced[0].DocumentID != far {
			t.Errorf("unplaced = %+v", u)
		}
		if groupOf(t, u.Groups, vendor[0]).ParentID != k.rm {
			t.Errorf("vendor group not under Risk Management")
		}
		if len(ans.Merges) != 1 || ans.Merges[0].ProposalID == "" || ans.Proposed != 5 {
			t.Errorf("merges %+v proposed %d; want the retire and four adds filed", ans.Merges, ans.Proposed)
		}
		changes, adds := 0, 0
		for _, p := range ontologyProposals(t, d, k.f.operator) {
			if p.Change != nil {
				changes++
			} else {
				adds++
			}
		}
		if changes != 1 || adds != 4 {
			t.Errorf("proposals: %d changes, %d adds; want 1 and 4", changes, adds)
		}
	})
}
