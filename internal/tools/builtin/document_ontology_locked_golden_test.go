package builtin

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

var updateLockedGolden = flag.Bool("update-locked-golden", false, "rewrite testdata/ontology_without_locked.golden")

// TestOntology_WithoutLockedReadsIdentically: an ontology with no @locked anywhere
// reads, describes, renders, reports its instances and plans exactly as before the
// directive existed. The golden was written by the code before RFC GQ P2.3 (the P2.2
// branch, 5ba1fba38) with -update-locked-golden; ids are masked, everything else is
// compared byte for byte. Re-pinned once since, for an additive field: the plan's
// concept re-tag job block on the concept and scheme renames (RFC GQ P5.5).
func TestOntology_WithoutLockedReadsIdentically(t *testing.T) {
	f := newRetiredFixture(t)
	read := f.read()
	_, res := docExec(t, f.d, f.ctx, `{"op":"describe_ontology"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	instances, err := f.d.AdoptInstances(f.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var changes []connector.OntologyChange
	for _, title := range []string{"Policy", "supervises", "Risk Management", "topics", "Globex"} {
		id := f.chunk(title)
		changes = append(changes, connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: id},
			connector.OntologyChange{Op: connector.OntologyPlanDelete, Entry: id},
			connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: id, Name: title + " 2"})
	}
	plan, err := f.d.PlanOntology(f.ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	plan.Fingerprint = ""
	parts, _ := json.MarshalIndent(map[string]any{
		"terms": read.Terms, "relations": read.Relations, "schemes": read.Schemes,
		"instances": read.Instances, "issues": read.Issues, "proposals": read.Proposals,
	}, "", " ")
	report, _ := json.MarshalIndent(instances, "", " ")
	planned, _ := json.MarshalIndent(plan, "", " ")
	got := strings.Join([]string{
		"== read", string(parts),
		"== describe_ontology", res.Text,
		"== prompt",
		memrank.RenderOntology(memrank.EffectiveOntology(read.Terms, true), true) +
			memrank.RenderRelations(memrank.EffectiveRelations(read.Relations, true, true)) +
			memrank.RenderSchemes(read.Schemes),
		"== instances", string(report),
		"== plan", string(planned),
	}, "\n")
	got = hexIDRe.ReplaceAllString(withoutContentVersion(got), "<id>")
	path := filepath.Join("testdata", "ontology_without_locked.golden")
	if *updateLockedGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("an ontology with no @locked no longer reads as before;\n--- got\n%s\n--- want\n%s", got, want)
	}
}
