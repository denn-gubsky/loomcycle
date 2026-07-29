package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
)

// runEvalLive invokes the subcommand exactly as main.go does.
func runEvalLive(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rc := RunMemoryEvalLive(args, &stdout, &stderr)
	return rc, stdout.String(), stderr.String()
}

// noPresetsConfig writes a config with no memory bundle, so the "nothing to score"
// path is reachable without a deployment.
func noPresetsConfig(t *testing.T) string {
	t.Helper()
	for _, k := range []string{"LOOMCYCLE_PRESETS", "LOOMCYCLE_CONFIG_DIR", "LOOMCYCLE_CONFIG_FILES"} {
		t.Setenv(k, "")
	}
	path := filepath.Join(t.TempDir(), "loomcycle.yaml")
	if err := os.WriteFile(path, []byte("agents:\n  demo:\n    system_prompt: \"demo\"\n    tier: middle\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestMemoryEvalLive_RequiresProviderAndModel(t *testing.T) {
	rc, _, stderr := runEvalLive(t)
	if rc != 2 {
		t.Errorf("want exit 2 for a missing required flag, got %d", rc)
	}
	if !strings.Contains(stderr, "--provider and --model are required") {
		t.Errorf("stderr should name the missing flags: %s", stderr)
	}
	// The message must explain WHY, because "a score" alone invites someone to
	// average two models' numbers together.
	if !strings.Contains(stderr, "triple") {
		t.Errorf("the message should say a score is only meaningful per (provider, model, effort): %s", stderr)
	}
}

// TestMemoryEvalLive_RefusesWithoutTheMemoryBundle: the eval scores the SHIPPED
// extractor prompt, so a config that has no extractor has nothing to score. Saying
// so beats silently scoring some default prompt.
func TestMemoryEvalLive_RefusesWithoutTheMemoryBundle(t *testing.T) {
	cfg := noPresetsConfig(t)
	rc, _, stderr := runEvalLive(t, "--provider", "mock", "--model", "m", "--config", cfg)
	if rc != 2 {
		t.Errorf("want exit 2, got %d (stderr: %s)", rc, stderr)
	}
	if !strings.Contains(stderr, "memory/extractor") {
		t.Errorf("stderr should name the missing agent: %s", stderr)
	}
	if !strings.Contains(stderr, "LOOMCYCLE_PRESETS") {
		t.Errorf("stderr should tell the operator how to fix it: %s", stderr)
	}
}

// TestMemoryEvalLive_UnknownProviderNamesTheDeclaredOnes: a typo'd provider id is
// the likeliest invocation error, and the fix is knowing what IS declared.
func TestMemoryEvalLive_UnknownProviderNamesTheDeclaredOnes(t *testing.T) {
	t.Setenv("LOOMCYCLE_PRESETS", "base,memory")
	for _, k := range []string{"LOOMCYCLE_CONFIG_DIR", "LOOMCYCLE_CONFIG_FILES"} {
		t.Setenv(k, "")
	}
	rc, _, stderr := runEvalLive(t, "--provider", "no-such-provider", "--model", "m")
	if rc == 0 {
		t.Fatalf("an unknown provider must not exit 0 (stderr: %s)", stderr)
	}
	if !strings.Contains(stderr, "no-such-provider") {
		t.Errorf("stderr should quote the bad id: %s", stderr)
	}
	if !strings.Contains(stderr, "declared") {
		t.Errorf("stderr should list what is declared: %s", stderr)
	}
}

// TestPrintExtractionReport_WithholdsScoresOnAFault: the printed output must not
// show a score table when the canary tripped, because a table of zeros next to a
// warning is read as a model result.
func TestPrintExtractionReport_WithholdsScoresOnAFault(t *testing.T) {
	var out bytes.Buffer
	printExtractionReport(&out, eval.ExtractionReport{
		Provider: "p", Model: "m",
		HarnessFault: "canary case returned NO facts",
		// Deliberately populated: even if scores are present on the struct, a
		// faulted report must not print them.
		Abilities: []eval.AbilityScore{{Ability: eval.AbilityExtraction, Recall: 0}},
	}, nil)
	s := out.String()
	if !strings.Contains(s, "HARNESS FAULT") {
		t.Errorf("the fault must be prominent: %s", s)
	}
	if strings.Contains(s, "per-ability") {
		t.Errorf("scores must be withheld on a fault: %s", s)
	}
}

// TestPrintExtractionReport_SaysWhenThereIsNoBaseline: a number with nothing to
// compare against is the state that produced every wrong conclusion so far, so the
// report says so out loud rather than letting the reader assume.
func TestPrintExtractionReport_SaysWhenThereIsNoBaseline(t *testing.T) {
	var out bytes.Buffer
	printExtractionReport(&out, eval.ExtractionReport{
		Provider: "p", Model: "m",
		Abilities: []eval.AbilityScore{{Ability: eval.AbilityExtraction, Cases: 1, Recall: 1}},
	}, nil)
	if !strings.Contains(out.String(), "compared against nothing") {
		t.Errorf("the report should say there is no baseline: %s", out.String())
	}
}

// TestPrintExtractionReport_ShowsMissesAndViolationsWithTheirReason: a score with
// no explanation is not actionable — the fixture's Why is what tells you whether to
// change the prompt or the corpus.
func TestPrintExtractionReport_ShowsMissesAndViolationsWithTheirReason(t *testing.T) {
	var out bytes.Buffer
	printExtractionReport(&out, eval.ExtractionReport{
		Provider: "p", Model: "m",
		Abilities: []eval.AbilityScore{{Ability: eval.AbilityProperty, Cases: 1, Recall: 0}},
		Cases: []eval.CaseResult{{
			Name: "request-implies-condition", Ability: eval.AbilityProperty,
			Wanted:     1,
			Misses:     []string{"the request reveals what the user TAKES (markers: all of [statin])"},
			Violations: []string{"distractor fixture: \"The user asked about alternatives.\" — recording that a question was ASKED"},
			Facts:      []eval.ExtractedFact{{Text: "The user asked about alternatives.", Class: "fact"}},
		}},
	}, nil)
	s := out.String()
	for _, want := range []string{"MISS", "VIOLATION", "statin", "asked", "emitted"} {
		if !strings.Contains(s, want) {
			t.Errorf("report should contain %q:\n%s", want, s)
		}
	}
}

func TestWrapText_DoesNotLoseWords(t *testing.T) {
	in := "a fairly long fixture reason that will certainly need wrapping at a narrow width to stay readable"
	got := wrapText(in, 30, "    ")
	flat := strings.Join(strings.Fields(strings.ReplaceAll(got, "\n", " ")), " ")
	if flat != in {
		t.Errorf("wrapping lost or reordered words:\n got %q\nwant %q", flat, in)
	}
}
