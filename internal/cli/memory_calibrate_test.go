package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
)

// minimalConfig writes the smallest config that loads, so a calibration test
// exercises the real flag/config path without needing a deployment.
func minimalConfig(t *testing.T) string {
	t.Helper()
	// loadLayeredConfig honours the same env layering the server does, so a
	// developer with presets exported would otherwise get a different config
	// than CI — and these tests assert on what the config contains.
	for _, k := range []string{"LOOMCYCLE_PRESETS", "LOOMCYCLE_CONFIG_DIR", "LOOMCYCLE_CONFIG_FILES"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "loomcycle.yaml")
	if err := os.WriteFile(path, []byte("agents:\n  demo:\n    system_prompt: \"demo\"\n    tier: middle\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// runCalibrate invokes the subcommand exactly as main.go does.
func runCalibrate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rc := RunMemoryCalibrate(args, &stdout, &stderr)
	return rc, stdout.String(), stderr.String()
}

// TestMemoryCalibrate_ExitsNonZeroWhenTheClassesOverlap — the command's
// contract. The deterministic stub embedder is a bag of hashed tokens with no
// notion of meaning, so its duplicate and related classes overlap; that is a
// model on which no merge threshold works, and a zero exit would call it
// calibrated.
func TestMemoryCalibrate_ExitsNonZeroWhenTheClassesOverlap(t *testing.T) {
	rc, out, errOut := runCalibrate(t, "--embedder", "stub", "--config", minimalConfig(t))
	if rc == 0 {
		t.Errorf("exit = 0 with overlapping classes, want non-zero\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(out, "NOT SEPARABLE") {
		t.Errorf("report does not say the classes are not separable:\n%s", out)
	}
	// It must still print the measurement — an operator needs to see WHERE
	// the classes landed to tell a bad model from a mislabelled corpus.
	for _, want := range []string{"duplicate", "related", "unrelated", "threshold sweep", "recommended for THIS embedder"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

// TestMemoryCalibrate_ReportsTheEmbedderItMeasured — a calibration number
// without its model is not a result, so provider/model/dimension are always on
// the report.
func TestMemoryCalibrate_ReportsTheEmbedderItMeasured(t *testing.T) {
	_, out, _ := runCalibrate(t, "--embedder", "stub", "--embed-dim", "48", "--config", minimalConfig(t))
	if !strings.Contains(out, "deterministic-eval-stub") || !strings.Contains(out, "48d") {
		t.Errorf("report does not attribute the result to the embedder it used:\n%s", out)
	}
	if !strings.Contains(out, "Re-run this after ANY change of embedding") {
		t.Errorf("report omits the model-specificity warning:\n%s", out)
	}
}

// TestMemoryCalibrate_RefusesWithNoConfiguredEmbedder — calibration measures
// the operator's OWN model. With none configured there is nothing to measure,
// and silently substituting the stub would produce an authoritative-looking
// number for a model nobody runs.
func TestMemoryCalibrate_RefusesWithNoConfiguredEmbedder(t *testing.T) {
	rc, _, errOut := runCalibrate(t, "--config", minimalConfig(t))
	if rc != 2 {
		t.Errorf("exit = %d with no memory.embedder configured, want 2 (fix the yaml, not the runtime)", rc)
	}
	if !strings.Contains(errOut, "no memory.embedder configured") {
		t.Errorf("stderr = %q, want it to name the missing embedder config", errOut)
	}
}

// TestMemoryCalibrate_RejectsABadInvocation — invocation errors exit 2, the
// documented user-error code, distinct from an operational or verdict failure.
func TestMemoryCalibrate_RejectsABadInvocation(t *testing.T) {
	cfg := minimalConfig(t)
	for _, tc := range []struct{ name, arg, val string }{
		{"missing corpus", "--dataset", filepath.Join(t.TempDir(), "nope.json")},
		{"unreadable config", "--config", filepath.Join(t.TempDir(), "nope.yaml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--embedder", "stub", "--config", cfg, tc.arg, tc.val}
			if rc, _, _ := runCalibrate(t, args...); rc != 2 {
				t.Errorf("exit = %d, want 2 (invocation error)", rc)
			}
		})
	}
	if rc, _, _ := runCalibrate(t, "--embedder", "banana", "--config", cfg); rc != 2 {
		t.Errorf("exit = %d for an unknown --embedder, want 2 (invocation error)", rc)
	}
}

// TestMemoryCalibrate_WritesTheJSONReport — --output is what makes a
// calibration re-analysable without re-embedding, so the raw pairs must
// survive the round trip.
func TestMemoryCalibrate_WritesTheJSONReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	if _, _, _ = runCalibrate(t, "--embedder", "stub", "--config", minimalConfig(t), "--output", out); true {
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read report: %v", err)
		}
		var rep eval.CalibrationReport
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatalf("parse report: %v", err)
		}
		if len(rep.Pairs) == 0 || len(rep.Sweep) == 0 || len(rep.Classes) != 3 {
			t.Errorf("report = %d pairs / %d sweep rows / %d classes, want all populated",
				len(rep.Pairs), len(rep.Sweep), len(rep.Classes))
		}
	}
}

// TestCalibrationExitCode_CheckFailsOnAnInertConfiguredBand — the regression
// guard --check exists for. A separable model exits 0 on its own; with --check
// it must ALSO fail when the band actually configured merges nothing, which is
// the silent failure the whole command was built to surface.
func TestCalibrationExitCode_CheckFailsOnAnInertConfiguredBand(t *testing.T) {
	rep := eval.Analyze("measured", eval.MeasuredEmbeddingGemmaInfo(), eval.MeasuredEmbeddingGemmaPairs(),
		eval.ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})
	if !rep.Separable {
		t.Fatal("fixture premise broken: the measured classes DO separate")
	}
	if got := calibrationExitCode(rep, false); got != 0 {
		t.Errorf("exit without --check = %d, want 0 (the classes separate)", got)
	}
	if got := calibrationExitCode(rep, true); got != 1 {
		t.Errorf("exit with --check = %d, want 1 (0.95 merges 0 of 12 duplicates)", got)
	}

	// With the recommended bands instead, --check passes: the guard tracks
	// the CONFIGURED value, not the corpus.
	tuned := eval.Analyze("measured", eval.MeasuredEmbeddingGemmaInfo(), eval.MeasuredEmbeddingGemmaPairs(),
		eval.ConfiguredBands{MergeThreshold: rep.RecommendedMerge, RelatedThreshold: rep.RecommendedRelated})
	if got := calibrationExitCode(tuned, true); got != 0 {
		t.Errorf("exit with --check on the recommended bands = %d, want 0 (configured = %+v)", got, tuned.Configured)
	}
}

// TestPrintCalibrationReport_RendersTheMeasuredEmbeddingGemmaRun — renders the
// reference measurement through the command's own printer.
//
// Two jobs: it pins the published numbers to the output an operator actually
// sees (a doc table and a report that disagree is worse than neither), and it
// makes that output reproducible with no live embedder — the measured host is
// not always reachable, and a result that only renders on one machine is not
// reproducible.
func TestPrintCalibrationReport_RendersTheMeasuredEmbeddingGemmaRun(t *testing.T) {
	rep := eval.Analyze("builtin", eval.MeasuredEmbeddingGemmaInfo(), eval.MeasuredEmbeddingGemmaPairs(),
		eval.ConfiguredBands{MergeThreshold: 0.95, RelatedThreshold: 0.85})
	var buf bytes.Buffer
	printCalibrationReport(&buf, rep, false)
	out := buf.String()

	for _, want := range []string{
		"ollama-local / embeddinggemma:latest (768d)",
		// The published class table, as the operator sees it.
		"duplicate     12   0.7181   0.7909   0.9035   0.9376   0.9487",
		"related       12   0.3722   0.3884   0.5205   0.6129   0.6775",
		"unrelated     72   0.1337   0.1460   0.2953   0.5018   0.5858",
		// The two gaps, including the honest one.
		"+0.0406",
		"-0.2136",
		"OVERLAP — a property of the model, not a tuning failure",
		// The headline finding.
		"merges 0/12 duplicates",
		"INERT — no duplicate reaches it",
		// The recommendation and its window.
		"0.6978   midpoint of the safe window (0.6775, 0.7181]",
		"verdict: SEPARABLE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report is missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, " \n") {
		t.Error("rendered report has trailing whitespace on a line")
	}
	t.Logf("rendered report:\n%s", out)
}
