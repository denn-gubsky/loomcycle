// Package workflowcheck holds tests over the repository's CI workflow files.
package workflowcheck

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCIGate_NeedsEveryJob: the `main` ruleset requires the "CI ok" job
// instead of each job by name (a skipped matrix job reports under its
// unexpanded name), so a job missing from its needs could fail without
// blocking a merge.
func TestCIGate_NeedsEveryJob(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Name  string `yaml:"name"`
			If    string `yaml:"if"`
			Needs any    `yaml:"needs"` // a job id, or a list of them
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	gate, ok := wf.Jobs["ci-ok"]
	if !ok {
		t.Fatal(`ci.yml has no "ci-ok" job`)
	}
	if gate.Name != "CI ok" || gate.If != "always()" {
		t.Errorf("ci-ok: name %q if %q; want \"CI ok\" and always()", gate.Name, gate.If)
	}
	needs, _ := gate.Needs.([]any)
	for id := range wf.Jobs {
		if id != "ci-ok" && !slices.Contains(needs, any(id)) {
			t.Errorf("job %q is not in ci-ok's needs, so its failure would not block a merge", id)
		}
	}
}
