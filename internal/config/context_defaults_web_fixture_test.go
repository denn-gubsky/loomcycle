package config

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// webContextDefaultsFixture is read by web/src/lib/compactionMethod.test.ts.
const webContextDefaultsFixture = "../../web/src/lib/__fixtures__/context-defaults.json"

// The Web UI labels a run's context method ("append · auto-compact at 80%")
// from the run's recorded compaction and context blocks, and fills what they
// leave unset with the runtime's defaults. Those defaults live HERE, applied at
// use time by the loop; the UI keeps a copy only because it has to render
// before asking the server anything. Generating its fixture from these
// constants means a changed default fails the web test instead of leaving the
// label quoting a threshold the runtime no longer uses. Regenerate with
// LOOMCYCLE_UPDATE_FIXTURES=1.
func TestContextDefaults_MatchTheWebFixture(t *testing.T) {
	defaults := map[string]map[string]any{
		"compaction": {
			"target_percentage":  CompactionDefaultTargetPct,
			"keep_last_n":        CompactionDefaultKeepLastN,
			"keep_first":         CompactionDefaultKeepFirst,
			"autocompact_at_pct": CompactionDefaultAutoAtPct,
		},
		"context": {
			"mode":             ContextModeAppend,
			"keep_last_n":      ContextDefaultKeepLastN,
			"reasoning":        ContextDefaultReasoning,
			"recap_max_chars":  ContextDefaultRecapMaxChars,
			"autorecap_at_pct": ContextDefaultAutoRecapAtPct,
		},
	}
	got, err := json.MarshalIndent(defaults, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("LOOMCYCLE_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(webContextDefaultsFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(webContextDefaultsFixture)
	if err != nil {
		t.Skipf("web fixture not readable from here: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("a context/compaction default changed; the Web UI's method label is tested "+
			"against the old one. Regenerate with LOOMCYCLE_UPDATE_FIXTURES=1 and re-run the "+
			"web tests.\n got: %s\nwant: %s", got, want)
	}
}
