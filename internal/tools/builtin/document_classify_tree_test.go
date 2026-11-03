package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// treeStub says yes to the concepts it is told the document is about, and names its
// class only when it is asked one. It records every request.
type treeStub struct {
	yes   map[string]bool
	class string
	asked []memrank.ClassifyRequest
}

func (s *treeStub) Classify(_ context.Context, req memrank.ClassifyRequest) memrank.ClassifyVerdict {
	s.asked = append(s.asked, req)
	v := memrank.ClassifyVerdict{Method: memrank.ClassifyMethodDecision}
	if !req.ConceptsOnly && len(req.Classes) > 0 {
		v.Class, v.Probability = s.class, 0.9
	}
	for _, c := range req.Concepts {
		if s.yes[c.Name] {
			v.Concepts = append(v.Concepts, c.Name)
		}
	}
	return v
}

func (s *treeStub) askedNames() [][]string {
	var out [][]string
	for _, r := range s.asked {
		var names []string
		for _, c := range r.Concepts {
			names = append(names, c.Name)
		}
		out = append(out, names)
	}
	return out
}

// bigTreeOntologyMD is the classify fixture's classes with a topic tree of 29 concepts,
// more than one request may name: Alpha (12 children), Beta (12 children, Beta5 with two
// of its own) and Gamma (a leaf).
func bigTreeOntologyMD() string {
	var b strings.Builder
	b.WriteString(strings.SplitN(classifyOntologyMD, "## Schemes", 2)[0])
	b.WriteString("## Schemes\n@section schemes\n\n### topics\n\n")
	for _, top := range []string{"Alpha", "Beta"} {
		b.WriteString("#### " + top + "\n\n")
		for i := 1; i <= 12; i++ {
			fmt.Fprintf(&b, "##### %s%d\n\n", top, i)
			if top == "Beta" && i == 5 {
				b.WriteString("###### Beta5x\n\n###### Beta5y\n\n")
			}
		}
	}
	b.WriteString("#### Gamma\n")
	return b.String()
}

func newBigTreeFixture(t *testing.T) *classifyFixture {
	t.Helper()
	d, base, _ := documentFixture(t)
	ctx := operatorCtx(installTenantOntology(t, d, base, bigTreeOntologyMD(), memrank.OntologyConfirmedStatus))
	return &classifyFixture{t: t, d: d, ctx: ctx}
}

func conceptNames(cs []classifyConcept) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Concept)
	}
	sort.Strings(out)
	return out
}

// TestClassify_ALargeConceptTreeIsAskedLevelByLevel: 29 concepts cannot go in one
// request, so the top level is asked first, then only the children of the concepts
// said yes. A child said yes stands for its parent; a yes leaf is kept.
func TestClassify_ALargeConceptTreeIsAskedLevelByLevel(t *testing.T) {
	f := newBigTreeFixture(t)
	stub := &treeStub{yes: map[string]bool{"Alpha": true, "Alpha3": true, "Gamma": true}}
	f.d.Classifier = stub
	// A hand-set type decides the class, so only the concepts are asked.
	docID, _ := f.doc("tenant", "A note", "/kb/n/1", "Order", [2]string{"Part", "text"})
	a := f.classify(f.ctx, "tenant", docID, nil)

	for i, r := range stub.asked {
		if len(r.Concepts) == 0 || len(r.Concepts) > memrank.ClassifyMaxConcepts || !r.ConceptsOnly {
			t.Fatalf("request %d named %d concepts (concepts only: %v)", i, len(r.Concepts), r.ConceptsOnly)
		}
	}
	names := stub.askedNames()
	if len(names) != 2 || strings.Join(names[0], ",") != "Alpha,Beta,Gamma" || len(names[1]) != 12 || names[1][0] != "Alpha1" {
		t.Fatalf("asked %v; want the top level, then Alpha's children only", names)
	}
	if got := conceptNames(a.Concepts); strings.Join(got, ",") != "Alpha3,Gamma" {
		t.Errorf("concepts = %v, want the most specific yes on each branch: Alpha3, Gamma", got)
	}
	if len(a.Statements) != 2 {
		t.Errorf("statements = %+v, want one proposed subject statement per concept", a.Statements)
	}
	if step := modelStep(a); step == nil || !strings.Contains(step.Reason, "2 calls") {
		t.Errorf("model step = %+v", step)
	}
}

// TestClassify_AParentWhoseChildrenAreAllNoIsKept, and a grandchild said yes stands
// for both levels above it.
func TestClassify_AParentWhoseChildrenAreAllNoIsKept(t *testing.T) {
	for _, tc := range []struct {
		yes  []string
		want string
	}{
		{[]string{"Beta"}, "Beta"},
		{[]string{"Beta", "Beta5"}, "Beta5"},
		{[]string{"Beta", "Beta5", "Beta5y"}, "Beta5y"},
		{[]string{"Alpha2", "Beta5"}, ""}, // children of a parent said no are never asked
	} {
		f := newBigTreeFixture(t)
		yes := map[string]bool{}
		for _, n := range tc.yes {
			yes[n] = true
		}
		f.d.Classifier = &treeStub{yes: yes}
		docID, _ := f.doc("tenant", "A note", "/kb/n/2", "Order", [2]string{"Part", "text"})
		a := f.classify(f.ctx, "tenant", docID, nil)
		if got := strings.Join(conceptNames(a.Concepts), ","); got != tc.want {
			t.Errorf("yes %v: concepts = %q, want %q", tc.yes, got, tc.want)
		}
	}
}

// TestClassify_AWideLevelIsAskedInBatches: 25 top-level concepts go in two requests,
// 20 and 5.
func TestClassify_AWideLevelIsAskedInBatches(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.SplitN(classifyOntologyMD, "## Schemes", 2)[0])
	b.WriteString("## Schemes\n@section schemes\n\n### topics\n\n")
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, "#### T%02d\n\n", i)
	}
	d, base, _ := documentFixture(t)
	f := &classifyFixture{t: t, d: d, ctx: operatorCtx(installTenantOntology(t, d, base, b.String(), memrank.OntologyConfirmedStatus))}
	stub := &treeStub{yes: map[string]bool{"T03": true, "T24": true}}
	f.d.Classifier = stub
	docID, _ := f.doc("tenant", "A note", "/kb/n/3", "Order", [2]string{"Part", "text"})
	a := f.classify(f.ctx, "tenant", docID, nil)
	if n := stub.askedNames(); len(n) != 2 || len(n[0]) != 20 || len(n[1]) != 5 {
		t.Fatalf("asked %v; want 20 then 5", n)
	}
	if got := strings.Join(conceptNames(a.Concepts), ","); got != "T03,T24" {
		t.Errorf("concepts = %q", got)
	}
}

// TestClassify_TheClassIsAskedAloneThenTheTree: with the class still open, the model is
// asked the class without concepts (they would not fit), then the tree is walked; both
// answers are proposals.
func TestClassify_TheClassIsAskedAloneThenTheTree(t *testing.T) {
	f := newBigTreeFixture(t)
	stub := &treeStub{yes: map[string]bool{"Beta": true, "Beta7": true}, class: "Order"}
	f.d.Classifier = stub
	docID, _ := f.doc("tenant", "Розпорядження 5", "/kb/n/4", "", [2]string{"Part", "text"})
	a := f.classify(f.ctx, "tenant", docID, nil)
	if len(stub.asked) < 3 || stub.asked[0].ConceptsOnly || len(stub.asked[0].Classes) == 0 || len(stub.asked[0].Concepts) != 0 {
		t.Fatalf("first request = %+v; want the class question alone", stub.asked)
	}
	for _, r := range stub.asked[1:] {
		if !r.ConceptsOnly || len(r.Concepts) > memrank.ClassifyMaxConcepts {
			t.Fatalf("a tree request = %+v", r)
		}
	}
	if a.Class != "Order" || a.Live || a.Result != "proposed" {
		t.Errorf("class answer = %+v", a)
	}
	if got := strings.Join(conceptNames(a.Concepts), ","); got != "Beta7" {
		t.Errorf("concepts = %q, want Beta7", got)
	}
	n := 0
	for _, s := range a.Steps {
		if s.Step == "model" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("steps = %+v; want the class step and the tree step", a.Steps)
	}
}
