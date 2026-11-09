package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/decision"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/classifier"
)

// conceptProbDecision is a decision model that answers each "Is this document about
// X?" with the yes-probability it is given for X (0.05 for any other), so the real
// memory.classifier — its threshold and its ranking fallback — runs over the answers.
type conceptProbDecision struct {
	yes   map[string]float64
	asked [][]string
}

func (s *conceptProbDecision) Decide(_ context.Context, req decision.Request) (*decision.Response, error) {
	ans := map[string]decision.Answer{}
	var names []string
	for k, q := range req.Questions {
		name, ok := strings.CutPrefix(q.Instructions, "Is this document about ")
		if !ok {
			continue
		}
		name, _, _ = strings.Cut(name, "?")
		names = append(names, name)
		p, ok := s.yes[name]
		if !ok {
			p = 0.05
		}
		ans[k] = decision.Answer{Type: decision.TypeNoul, Noul: &p}
	}
	s.asked = append(s.asked, names)
	return &decision.Response{Answers: ans}, nil
}

func (s *conceptProbDecision) Limits(string) decision.Limits { return decision.Limits{} }

func fallbackClassifier(yes map[string]float64) (*classifier.Classifier, *conceptProbDecision) {
	dec := &conceptProbDecision{yes: yes}
	c := classifier.New(dec, "nimble", 0, nil, 0.6, 0.2)
	c.SetConceptFallback(0.3, 0.1)
	return c, dec
}

// TestClassify_AFallbackTopLevelPickIsDescendedInto: no top-level concept clears
// min_probability, the ranking fallback takes Alpha, so Alpha's children are asked and
// Alpha3, above the threshold, is the most specific pick — a normal one, not a fallback.
func TestClassify_AFallbackTopLevelPickIsDescendedInto(t *testing.T) {
	f := newBigTreeFixture(t)
	c, dec := fallbackClassifier(map[string]float64{"Alpha": 0.45, "Beta": 0.2, "Gamma": 0.1, "Alpha3": 0.7})
	f.d.Classifier = c
	docID, root := f.doc("tenant", "A note", "/kb/n/6", "Order", [2]string{"Part", "text"})
	a := f.classify(f.ctx, "tenant", docID, nil)
	if len(dec.asked) != 2 || len(dec.asked[1]) != 12 || !strings.HasPrefix(dec.asked[1][0], "Alpha") {
		t.Fatalf("asked %v; want the top level, then Alpha's children", dec.asked)
	}
	if len(a.Concepts) != 1 || a.Concepts[0].Concept != "Alpha3" || a.Concepts[0].Fallback {
		t.Fatalf("concepts = %+v, want Alpha3 taken normally", a.Concepts)
	}
	if attrs := classifyStatementAttrs(t, f, docID, root)["subject"]; attrs == nil || attrs[attrConceptFallback] != nil {
		t.Errorf("statement attributes = %v, want a statement with no concept_fallback mark", attrs)
	}
}

// TestClassify_AFallbackPickIsMarkedOnItsStatement: a flat vocabulary whose one
// concept is taken by the ranking fallback is reported as a fallback and its proposed
// statement carries concept_fallback, so the judge and the operator can tell it apart.
func TestClassify_AFallbackPickIsMarkedOnItsStatement(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	c, _ := fallbackClassifier(map[string]float64{"Risk Management": 0.45})
	f.d.Classifier = c
	docID, root := f.doc("tenant", "A note", "/kb/n/7", "Order", [2]string{"Part", "text"})
	a := f.classify(f.ctx, "tenant", docID, nil)
	if len(a.Concepts) != 1 || a.Concepts[0].Concept != "Risk Management" || !a.Concepts[0].Fallback {
		t.Fatalf("concepts = %+v, want Risk Management by the fallback", a.Concepts)
	}
	attrs := classifyStatementAttrs(t, f, docID, root)["subject"]
	if attrs[attrConceptFallback] != true || attrs[attrClassifiedBy] != classifiedByModel {
		t.Errorf("statement attributes = %v, want classified_by model and concept_fallback true", attrs)
	}
}
