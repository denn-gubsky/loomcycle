package builtin

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// retiredOntologyMD has an entry of every section with something beneath it: a document
// class with a subclass and a concept field, a tenant class overriding the seed class
// `event`, a relation kind with a sub-kind, a scheme whose concept carries policy and a
// narrower concept, and two instances.
const retiredOntologyMD = "# Tenant Ontology\n\n" +
	"## Classes\n@section classes\n\n" +
	"### Document\n- `title` text\n\n" +
	"#### Policy\n- `topic` concept(topics)\n\n##### Internal Policy\n\n" +
	"#### Memo\n\n" +
	"### Provision\n\n" +
	"### event\n- `name` text\n\n" +
	"## Relations\n@section relations\n\n" +
	"### supervises\n\n#### co_supervises\n\n" +
	"### filed_under\n@range concept\n\n" +
	"## Schemes\n@section schemes\n\n" +
	"### topics\n@render\n\n" +
	"#### Risk Management\n@retention 10y\n\n##### outsourcing\n\n" +
	"#### Reporting\n\n" +
	"## Instances\n@section instances\n\n" +
	"### Acme\n@a organization\n\n### Globex\n@a organization\n"

var updateRetiredGolden = flag.Bool("update-retired-golden", false, "rewrite testdata/ontology_without_retired.golden")

// TestOntology_WithoutRetiredReadsIdentically: an ontology with nothing retired reads,
// describes and renders exactly as before the status existed. The golden was written
// by the code before RFC GQ P2.1 (origin/main 96abb9208) with -update-retired-golden;
// ids are masked, everything else is compared byte for byte.
func TestOntology_WithoutRetiredReadsIdentically(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		t.Fatal("read")
	}
	_, res := docExec(t, d, ctx, `{"op":"describe_ontology"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	parts, _ := json.MarshalIndent(map[string]any{
		"terms": read.Terms, "relations": read.Relations, "schemes": read.Schemes,
		"instances": read.Instances, "issues": read.Issues, "proposals": read.Proposals,
	}, "", " ")
	got := strings.Join([]string{
		"== read", string(parts),
		"== describe_ontology", res.Text,
		"== prompt",
		memrank.RenderOntology(memrank.EffectiveOntology(read.Terms, true), true) +
			memrank.RenderRelations(memrank.EffectiveRelations(read.Relations, true, true)) +
			memrank.RenderSchemes(read.Schemes),
	}, "\n")
	got = hexIDRe.ReplaceAllString(got, "<id>")
	path := filepath.Join("testdata", "ontology_without_retired.golden")
	if *updateRetiredGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("an ontology with nothing retired no longer reads as before;\n--- got\n%s\n--- want\n%s", got, want)
	}
}
