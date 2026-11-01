package builtin

import (
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// reviewOntologyMD is relationsOntologyMD with a Review section, the shape MTB's
// first policy takes (accept the rule-made citations; classify's rule-matched
// subject tags; everything else waits for a person), and a judge rule.
const reviewOntologyMD = relationsOntologyMD +
	"## Review\n@section review\n" +
	"What the policy accepts needs no person.\n" +
	"judge_floor: 0.8\n\n" +
	"- accept kind:cites,repeals method:rule:citation when quotes:resolved self:false target:held|placeholder\n" +
	"- accept kind:subject method:classify when rule:matched\n" +
	"- reject kind:elaborates method:extractor:relate when confidence<0.6\n" +
	"- judge  kind:elaborates floor:0.9 mode:recommend\n" +
	"- human  *                                  # default\n"

// The Review section is read into the ontology, in force once confirmed, and
// describe_ontology shows the parsed policy with its section listed.
func TestDescribeOntology_ShowsTheReviewPolicy(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, reviewOntologyMD, memrank.OntologyConfirmedStatus)
	desc := describeOntologyVia(t, d, ctx)
	if desc.Review == nil {
		t.Fatalf("no review policy described; sections %v issues %v", desc.Sections, desc.Issues)
	}
	r := desc.Review
	if !r.Applied || r.Source != memrank.ReviewSourceSection || r.JudgeFloor != 0.8 || len(r.Rules) != 5 {
		t.Fatalf("review = %+v", r)
	}
	if r.Rules[0].Index != 1 || r.Rules[0].Action != "accept" || r.Rules[3].Mode != "recommend" || *r.Rules[3].Floor != 0.9 || !r.Rules[4].Any {
		t.Errorf("rules = %+v", r.Rules)
	}
	if !hasSection(desc.Sections, memrank.SectionReview) {
		t.Errorf("sections = %v, want review listed", desc.Sections)
	}
	if !strings.Contains(strings.Join(desc.Issues, "\n"), "@auto_assert on supervises is not applied") {
		t.Errorf("an @auto_assert kind beside a Review section is not reported: %v", desc.Issues)
	}

	// A draft governs nothing: no policy is described.
	dd, dbase, _ := documentFixture(t)
	dctx := installTenantOntology(t, dd, dbase, reviewOntologyMD, memrank.OntologyDraftStatus)
	if got := describeOntologyVia(t, dd, dctx).Review; got != nil {
		t.Errorf("a draft describes a policy: %+v", got)
	}
}

// A malformed rule is an issue of the document, and the policy is described as
// not applied.
func TestDescribeOntology_AMalformedReviewRuleIsAnIssue(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(reviewOntologyMD, "when confidence<0.6", "when confidence~0.6", 1)
	ctx := installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	desc := describeOntologyVia(t, d, ctx)
	if desc.Review == nil || desc.Review.Applied || len(desc.Review.Issues) == 0 {
		t.Fatalf("review = %+v", desc.Review)
	}
	issues := strings.Join(desc.Issues, "\n")
	if !strings.Contains(issues, "Review: rule 3") || !strings.Contains(issues, "not applied") {
		t.Errorf("issues = %v", desc.Issues)
	}
}

// Without a Review section the policy described is the one @auto_assert
// implies, so an existing ontology reads as the rule it always was.
func TestDescribeOntology_AutoAssertImpliesThePolicy(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, relationsOntologyMD, memrank.OntologyConfirmedStatus)
	desc := describeOntologyVia(t, d, ctx)
	if desc.Review == nil || desc.Review.Source != memrank.ReviewSourceAutoAssert || !desc.Review.Applied ||
		len(desc.Review.Rules) != 1 || desc.Review.Rules[0].Text != "accept kind:supervises method:rule:* when quotes:resolved" {
		t.Fatalf("review = %+v", desc.Review)
	}
	for _, iss := range desc.Issues {
		if strings.HasPrefix(iss, "Review:") {
			t.Errorf("unexpected review issue: %s", iss)
		}
	}
}
