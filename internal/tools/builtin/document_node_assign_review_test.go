package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// assignGammaMD adds a third top-level concept, Gamma, to assignOntologyMD.
const assignGammaMD = assignOntologyMD + "\n#### Gamma\n"

// retireConcept is the operator's planned retire of concept id.
func retireConcept(t *testing.T, d *Document, ctx context.Context, id string) {
	t.Helper()
	plan, err := d.PlanOntology(ctx, []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: id}})
	if err != nil || plan.Fingerprint == "" {
		t.Fatalf("plan: %v %+v", err, plan)
	}
	_, g := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, id))
	var chunk struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal([]byte(g.Text), &chunk); err != nil {
		t.Fatalf("get_chunk: %v", err)
	}
	_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,"status":%q,"plan_fingerprint":%q}`,
		id, chunk.Revision, memrank.OntologyStatusRetired, plan.Fingerprint))
	if r.IsError {
		t.Fatalf("retire: %s", r.Text)
	}
}

// TestAssign_PlacementUnderAnUncomparedConceptIsNotJudged — a document placed
// only under a concept with too few members to have a centroid is not judged
// against the concepts that were compared: it can never agree, so counting it
// would bias the agreement figure a threshold is chosen from.
func TestAssign_PlacementUnderAnUncomparedConceptIsNotJudged(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignGammaMD)
	gamma := a.f.concepts["topics/gamma"]
	if gamma == "" {
		t.Fatalf("concepts = %v", a.f.concepts)
	}
	g := a.doc(t, "/kb/g1", []float32{0, 0, 0.2, 1}, gamma)
	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200}`)
	l1 := rep.docOf(t, g).at(t, 1)
	if l1.Compared != 2 || l1.Agrees != nil {
		t.Errorf("g1 level 1 = %+v; want Alpha and Beta compared and nothing to agree with", l1)
	}
	for _, lv := range rep.Levels {
		if lv.Level == 1 && lv.Placed != 9 {
			t.Errorf("level 1 placed = %d; want the 9 members of compared concepts", lv.Placed)
		}
	}
}

// TestAssign_RetiredConceptIsNeitherComparedNorProposed — a retired concept takes
// no new placement, so it is left out of the comparison: the live concepts are
// ranked and the proposal goes to the live nearest.
func TestAssign_RetiredConceptIsNeitherComparedNorProposed(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignGammaMD)
	gamma := a.f.concepts["topics/gamma"]
	for i, v := range [][]float32{{0, 0, 0, 1}, {0, 0.05, 0, 0.95}, {0.05, 0, 0, 0.9}} {
		a.doc(t, fmt.Sprintf("/kb/gamma/%d", i), v, gamma)
	}
	u := a.doc(t, "/kb/u", []float32{0, 0, 1, 0.3})
	retireConcept(t, d, a.f.operator, a.beta)

	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200,"margins":{"1":0}}`)
	for _, pd := range rep.Page {
		for _, rk := range pd.Rankings {
			if rk.Nearest.NodeID == a.beta || rk.Second.NodeID == a.beta {
				t.Fatalf("%s ranks the retired Beta: %+v", pd.DocumentID, rk)
			}
		}
	}
	if l1 := rep.docOf(t, u).at(t, 1); l1.Nearest.NodeID != gamma {
		t.Errorf("u level 1 = %+v; want Gamma, the live nearest", l1)
	}
	assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true,"limit":100,"margins":{"1":0}}`)
	filed := assignedStatements(t, d, a.f.operator)
	if _, ok := filed[a.docs[u]+"->"+gamma]; !ok {
		t.Errorf("filed %v; want u under Gamma", filedKeys(filed))
	}
	for k, s := range filed {
		if s.ToID == a.beta {
			t.Errorf("filed %s to the retired Beta", k)
		}
	}
}

// TestAssign_OnlyTheMostSpecificEligibleConceptIsFiled — eligible at level 1
// (Alpha) and at level 2 (Alpha One below it), a document is filed under Alpha One
// alone: a placement rolls up, so Alpha would be redundant.
func TestAssign_OnlyTheMostSpecificEligibleConceptIsFiled(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	u := a.doc(t, "/kb/u1", []float32{1, 0.05, 0, 0})
	body := `{"op":"node_stats","mode":"assign","limit":200,"margins":{"1":0,"2":0}}`
	ud := assignRun(t, d, a.f.operator, body).docOf(t, u)
	if l1, l2 := ud.at(t, 1), ud.at(t, 2); !l1.Eligible || l1.Nearest.NodeID != a.alpha || !l2.Eligible || l2.Nearest.NodeID != a.one {
		t.Fatalf("fixture: u1 = %+v", ud.Rankings)
	}
	assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true,"limit":100,"margins":{"1":0,"2":0}}`)
	filed := assignedStatements(t, d, a.f.operator)
	from := a.docs[u]
	if _, ok := filed[from+"->"+a.one]; !ok {
		t.Errorf("filed %v; want u1 under Alpha One", filedKeys(filed))
	}
	if _, ok := filed[from+"->"+a.alpha]; ok {
		t.Errorf("u1 filed under Alpha too, which Alpha One already implies")
	}
}

func filedKeys(m map[string]statementRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAssign_DocumentPlacedUnderASiblingIsNotFiled — a document already placed
// under a compared concept is not filed at that level even when a sibling is
// nearer by more than the threshold: a second, contradicting placement is misfits'
// question. The ranking is still reported, disagreeing; a document placed under
// none of the compared concepts is filed as before.
func TestAssign_DocumentPlacedUnderASiblingIsNotFiled(t *testing.T) {
	d, base, _ := documentFixture(t)
	a := newAssignCorpus(t, d, base, assignOntologyMD)
	sib := a.doc(t, "/kb/sib", []float32{1, 0.02, 0, 0}, a.two) // nearest Alpha One, placed under Alpha Two
	free := a.doc(t, "/kb/free", []float32{1, 0.02, 0, 0}, a.alpha)

	rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","limit":200,"margins":{"2":0}}`)
	r := rep.docOf(t, sib).at(t, 2)
	if r.Nearest.NodeID != a.one || r.Agrees == nil || *r.Agrees || r.Eligible {
		t.Errorf("sib level 2 = %+v; want nearest Alpha One, disagreeing, not eligible", r)
	}
	if r := rep.docOf(t, free).at(t, 2); r.Nearest.NodeID != a.one || !r.Eligible {
		t.Errorf("free level 2 = %+v; want nearest Alpha One, eligible", r)
	}

	assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true,"margins":{"2":0}}`)
	got := filedKeys(assignedStatements(t, d, a.f.operator))
	if want := []string{a.docs[free] + "->" + a.one}; !reflect.DeepEqual(got, want) {
		t.Errorf("filed = %v; want only %v", got, want)
	}
}

// TestAssign_AMisfitsReviewRuleDoesNotRuleOnAssignProposals — assign files under
// its own method, so a Review rule written for misfits' rule:centroid leaves its
// proposals alone, and one written for rule:assign rules on them.
func TestAssign_AMisfitsReviewRuleDoesNotRuleOnAssignProposals(t *testing.T) {
	for _, tc := range []struct {
		method, want string
	}{{misfitMethod, edgeStatusProposed}, {assignMethod, edgeStatusAsserted}} {
		t.Run(tc.method, func(t *testing.T) {
			d, base, _ := documentFixture(t)
			md := assignOntologyMD + "\n## Review\n@section review\n- accept kind:subject method:" + tc.method + "\n- human *\n"
			a := newAssignCorpus(t, d, base, md)
			p1 := a.doc(t, "/kb/p1", []float32{1, 0.02, 0, 0}, a.alpha)
			rep := assignRun(t, d, a.f.operator, `{"op":"node_stats","mode":"assign","propose":true}`)
			if len(rep.Proposals) != 1 || rep.Proposals[0].DocumentID != p1 || rep.Proposals[0].Status != tc.want {
				t.Errorf("proposals = %+v; want p1 %s", rep.Proposals, tc.want)
			}
		})
	}
}
