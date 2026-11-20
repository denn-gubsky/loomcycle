package builtin

import (
	"strings"
	"sync"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// countConceptProposals is the open concept proposals named name.
func countConceptProposals(t *testing.T, f *conceptFixture, name string) int {
	t.Helper()
	read, ok := f.c.d.tenantOntologyRead(f.operator)
	if !ok {
		t.Fatal("ontology unreadable")
	}
	n := 0
	for _, p := range read.Proposals {
		if p.Section == memrank.SectionSchemes && strings.EqualFold(p.Name, name) && p.Status == memrank.OntologyStatusProposed {
			n++
		}
	}
	return n
}

// TestProposeEntity_ConceptUnderATagIsNotProposedTwice — the duplicate check compares
// the resolved parent, so a concept proposed under a parent named by its tag is
// refused the second time, as one named by its name is.
func TestProposeEntity_ConceptUnderATagIsNotProposedTwice(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newConceptFixture(t, d, base)
	body := `{"op":"propose_entity","section":"schemes","name":"vendor exit","parent":"topics/risk-management","body":"seen in three plans"}`
	if _, res := docExec(t, d, f.operator, body); res.IsError {
		t.Fatalf("first: %s", res.Text)
	}
	_, res := docExec(t, d, f.operator, body)
	if !res.IsError || !strings.Contains(res.Text, "already proposed") {
		t.Errorf("second under the tag: %s; want already proposed", res.Text)
	}
	_, res = docExec(t, d, f.operator, strings.Replace(body, "topics/risk-management", "Risk Management", 1))
	if !res.IsError || !strings.Contains(res.Text, "already proposed") {
		t.Errorf("again under the name: %s; want already proposed", res.Text)
	}
	if n := countConceptProposals(t, f, "vendor exit"); n != 1 {
		t.Errorf("%d proposals filed, want 1", n)
	}
}

// TestProposeEntity_ConcurrentAddsFileOne — identical add proposals filed at once
// file one: the read, the duplicate check and the filing run under one lock.
func TestProposeEntity_ConcurrentAddsFileOne(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newConceptFixture(t, d, base)
	body := `{"op":"propose_entity","section":"schemes","name":"vendor exit","parent":"Risk Management","body":"x"}`
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Execute(f.operator, []byte(body))
		}()
	}
	wg.Wait()
	if n := countConceptProposals(t, f, "vendor exit"); n != 1 {
		t.Errorf("%d proposals filed by 8 concurrent calls, want 1", n)
	}
}

// TestProposeEntity_NameWithAControlCharacterIsRefused — an add proposal's name is
// one line: a line break or another control character is refused and nothing filed.
func TestProposeEntity_NameWithAControlCharacterIsRefused(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newConceptFixture(t, d, base)
	for _, name := range []string{`vendor\nexit`, `vendor\rexit`, `vendor\u0007exit`} {
		_, res := docExec(t, d, f.operator, `{"op":"propose_entity","section":"schemes","name":"`+name+`","parent":"Risk Management"}`)
		if !res.IsError || !strings.Contains(res.Text, "control characters") {
			t.Errorf("%s: %s; want refused", name, res.Text)
		}
	}
	if _, res := docExec(t, d, f.operator, `{"op":"propose_entity","name":"Outage\nType"}`); !res.IsError {
		t.Errorf("a class name with a line break was filed: %s", res.Text)
	}
	read, _ := d.tenantOntologyRead(f.operator)
	if len(read.Proposals) != 0 {
		t.Errorf("proposals filed: %+v", read.Proposals)
	}
}
