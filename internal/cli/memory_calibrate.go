package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory/embedders"
	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// RunMemoryCalibrate implements `loomcycle memory-calibrate`: it measures the
// consolidation similarity bands against the operator's OWN embedder.
//
// WHY IT EXISTS. `memory.consolidation.merge_threshold` decides whether the
// consolidator treats two facts as the same fact. Cosine scale is a property of
// the embedding model, so a constant is right for at most one model — and being
// wrong is SILENT. On `embeddinggemma` (the model loomcycle's own self-hosting
// docs recommend) the shipped 0.95 default merges NOTHING: the highest genuine
// paraphrase measured 0.9487. Nothing logs that; duplicates simply accumulate
// forever. The defect is not the value — 0.95 fails safe, and this command does
// not change it — it is that an operator had no way to learn their band was
// inert. That is what this fixes.
//
// Flags:
//
//	--config <yaml>       config to read the embedder + configured bands from
//	--dataset <path>      labelled corpus JSON (default: the bundled corpus)
//	--embedder auto|stub  auto = the configured embedder; stub = plumbing only
//	--check               also fail when the CONFIGURED bands classify badly
//	--output <file>       write the JSON report (the text report still prints)
//
// Exit codes:
//
//	0 — the duplicate and related classes separate: a merge threshold exists.
//	1 — they overlap (no threshold can separate them: the model is unsuitable
//	    for this corpus, or the corpus is mislabelled), OR --check found the
//	    configured bands inert/destructive. Also operational failure.
//	2 — invocation error (bad flag, missing corpus, unloadable config).
func RunMemoryCalibrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory-calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "loomcycle.yaml", "path to config YAML")
	dataset := fs.String("dataset", "builtin", "labelled corpus: 'builtin' or a path to a .json file")
	embedderKind := fs.String("embedder", "auto", "'auto' (the configured embedder) or 'stub' (deterministic, plumbing check only)")
	check := fs.Bool("check", false, "also fail when the CONFIGURED thresholds classify badly on this corpus")
	output := fs.String("output", "", "optional path to write the JSON report (the text report still prints)")
	embedDim := fs.Int("embed-dim", 64, "stub embedder output dimension (--embedder stub only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	corpus, err := loadCalibrationCorpus(*dataset)
	if err != nil {
		return fail(stderr, "memory-calibrate: %v", err)
	}

	// The SAME layered stack the server assembles (presets → CONFIG_DIR →
	// CONFIG_FILES → --config), so the embedder and the bands reported here
	// are the ones the running server would use. Reading a single file would
	// reintroduce the bug loadLayeredConfig exists to fix.
	cfg, err := loadLayeredConfig(*cfgPath)
	if err != nil {
		return fail(stderr, "memory-calibrate: config: %v", err)
	}

	// An unknown --embedder, an unconfigured one, or one the config cannot
	// construct are all "fix the invocation or the yaml" → exit 2. Only the
	// embed CALL below is operational (a host that will not answer) → exit 1.
	emb, err := calibrationEmbedder(*embedderKind, *embedDim, cfg)
	if err != nil {
		return fail(stderr, "memory-calibrate: %v", err)
	}

	bands := eval.ConfiguredBands{
		MergeThreshold:   cfg.Memory.Consolidation.EffectiveMergeThreshold(),
		RelatedThreshold: cfg.Memory.Consolidation.EffectiveRelatedThreshold(),
	}

	rep, err := eval.Calibrate(context.Background(), corpus, emb, bands)
	if err != nil {
		return failOp(stderr, "memory-calibrate: %v", err)
	}

	if *output != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*output, append(b, '\n'), 0o644); err != nil {
			return failOp(stderr, "memory-calibrate: write report: %v", err)
		}
		fmt.Fprintf(stdout, "wrote report to %s\n\n", *output)
	}

	printCalibrationReport(stdout, rep, *embedderKind == "stub")
	return calibrationExitCode(rep, *check)
}

// calibrationExitCode turns the report into the verdict.
//
// Overlap is a failure and not a warning because there is no such thing as a
// partially-correct merge threshold: if a related pair outscores a duplicate
// pair, every threshold either leaves duplicates unmerged or destroys a
// distinct fact, and the operator has to change the model or the corpus. A
// zero exit would say "calibrated" about a deployment that cannot be.
func calibrationExitCode(rep eval.CalibrationReport, check bool) int {
	if !rep.Separable {
		return 1
	}
	if check && (rep.Configured.MergeInert || rep.Configured.MergeDestructive || rep.Configured.RelatedInert) {
		return 1
	}
	return 0
}

func loadCalibrationCorpus(name string) (eval.CalibrationCorpus, error) {
	if name == "" || name == "builtin" || name == "bundled" {
		return eval.BuiltinCalibrationCorpus()
	}
	f, err := os.Open(name)
	if err != nil {
		return eval.CalibrationCorpus{}, fmt.Errorf("open corpus: %w", err)
	}
	defer func() { _ = f.Close() }()
	return eval.LoadCalibrationCorpus(f)
}

// calibrationEmbedder resolves --embedder.
//
// `stub` is a PLUMBING check, not a calibration: the deterministic embedder is
// a bag of hashed tokens with no notion of meaning, so its classes overlap by
// construction and it will report a model that cannot be calibrated. It exists
// so the command is runnable — and its exit paths testable — with no provider
// key and no reachable model host. A real number needs a real embedder.
func calibrationEmbedder(kind string, dim int, cfg *config.Config) (providers.Embedder, error) {
	switch strings.TrimSpace(kind) {
	case "stub":
		return eval.NewDeterministicEmbedder(dim), nil
	case "", "auto":
		emb, err := embedders.Build(cfg)
		if err != nil {
			return nil, fmt.Errorf("embedder: %w", err)
		}
		if emb == nil {
			return nil, fmt.Errorf("no memory.embedder configured — calibration measures YOUR model, so there is nothing to measure (or pass --embedder stub for a plumbing check)")
		}
		return emb, nil
	default:
		return nil, fmt.Errorf("unknown --embedder %q (want 'auto' or 'stub')", kind)
	}
}

// printCalibrationReport renders the measurement. Layout mirrors
// printEvalReport's plain-text shape: an operator reads this in a terminal
// beside the yaml they are about to edit.
func printCalibrationReport(w io.Writer, r eval.CalibrationReport, stub bool) {
	dup, rel, unrel := classOf(r, eval.ClassDuplicate), classOf(r, eval.ClassRelated), classOf(r, eval.ClassUnrelated)
	nonDup := rel.N + unrel.N

	fmt.Fprintf(w, "memory-calibrate: corpus %q\n", r.Corpus)
	fmt.Fprintf(w, "  embedder             %s / %s (%dd)\n", orDash(r.Embedder.Provider), orDash(r.Embedder.Model), r.Embedder.Dimension)
	if stub {
		fmt.Fprintf(w, "  NOTE                 --embedder stub is a plumbing check, NOT a calibration:\n")
		fmt.Fprintf(w, "                       the stub embedder has no notion of meaning.\n")
	}

	fmt.Fprintf(w, "\nclass distributions (raw cosine)\n")
	fmt.Fprintf(w, "  %-10s %5s %8s %8s %8s %8s %8s\n", "class", "n", "min", "p05", "median", "p95", "max")
	for _, c := range r.Classes {
		fmt.Fprintf(w, "  %-10s %5d %8.4f %8.4f %8.4f %8.4f %8.4f\n", c.Class, c.N, c.Min, c.P05, c.Median, c.P95, c.Max)
	}

	fmt.Fprintf(w, "\nclass separation\n")
	fmt.Fprintf(w, "  duplicate vs related   %+8.4f  %s\n", r.DuplicateVsRelatedGap, separationNote(r.Separable))
	fmt.Fprintf(w, "  related  vs unrelated  %+8.4f  %s\n", r.RelatedVsUnrelatedGap, overlapNote(r.RelatedUnrelatedOverlap))

	fmt.Fprintf(w, "\nthreshold sweep\n")
	fmt.Fprintf(w, "  %9s %12s %14s %12s %12s\n", "threshold", "merged", "false merges", "related", "unrelated")
	for _, row := range r.Sweep {
		fmt.Fprintf(w, "  %9.4f %12s %14s %12s %12s\n",
			row.Threshold,
			ratio(row.DuplicatesMerged, dup.N),
			ratio(row.FalseMerges, nonDup),
			ratio(row.RelatedCaught, rel.N),
			ratio(row.UnrelatedFlagged, unrel.N))
	}
	fmt.Fprintf(w, "  merged/false merges are the MERGE band; related/unrelated the RELATED band.\n")

	fmt.Fprintf(w, "\nconfigured bands (from the loaded config)\n")
	fmt.Fprintf(w, "  merge_threshold      %.4f   merges %d/%d duplicates, %d/%d false merges\n",
		r.Configured.MergeThreshold, r.Configured.DuplicatesMerged, dup.N, r.Configured.FalseMerges, nonDup)
	if r.Configured.MergeInert {
		fmt.Fprintf(w, "                       INERT — no duplicate reaches it, so merging can never fire and\n")
		fmt.Fprintf(w, "                       duplicates accumulate silently. This fails SAFE (an unmerged\n")
		fmt.Fprintf(w, "                       duplicate is recoverable; a wrongly merged pair is not), which is\n")
		fmt.Fprintf(w, "                       why the shipped default is high — but it is not calibrated to you.\n")
	}
	if r.Configured.MergeDestructive {
		fmt.Fprintf(w, "                       DESTRUCTIVE — %d non-duplicate pair(s) reach it. Merging them\n", r.Configured.FalseMerges)
		fmt.Fprintf(w, "                       destroys a distinct fact, and that is not recoverable. Raise it.\n")
	}
	fmt.Fprintf(w, "  related_threshold    %.4f   catches %d/%d related, %d/%d unrelated\n",
		r.Configured.RelatedThreshold, r.Configured.RelatedCaught, rel.N, r.Configured.UnrelatedFlagged, unrel.N)
	if r.Configured.RelatedInert {
		fmt.Fprintf(w, "                       INERT — no related pair reaches it.\n")
	}

	fmt.Fprintf(w, "\nrecommended for THIS embedder\n")
	if r.Separable {
		fmt.Fprintf(w, "  merge_threshold      %.4f   midpoint of the safe window (%.4f, %.4f]\n",
			r.RecommendedMerge, r.SafeWindowLow, r.SafeWindowHigh)
	} else {
		fmt.Fprintf(w, "  merge_threshold      %.4f   NO safe window exists — this is the lowest threshold\n", r.RecommendedMerge)
		fmt.Fprintf(w, "                       with zero false merges, chosen on the asymmetry (an unmerged\n")
		fmt.Fprintf(w, "                       duplicate is recoverable, a wrongly merged pair is not).\n")
	}
	fmt.Fprintf(w, "  related_threshold    %.4f   catches %.0f%% of related, %d/%d unrelated false positives\n",
		r.RecommendedRelated, r.RecommendedRelatedRecall*100, r.RecommendedRelatedFalsePositives, unrel.N)
	if r.RelatedUnrelatedOverlap {
		fmt.Fprintf(w, "                       the related and unrelated classes OVERLAP, so no threshold\n")
		fmt.Fprintf(w, "                       separates them: this is a recall/false-positive trade-off,\n")
		fmt.Fprintf(w, "                       not a clean split.\n")
	}

	fmt.Fprintf(w, "\n%s\n", verdictBlock(r))
	fmt.Fprintf(w, "\nThese numbers describe %s ONLY. Re-run this after ANY change of embedding\nmodel or dimension — a threshold carried across models is not calibrated.\n",
		orDash(r.Embedder.Model))
}

func verdictBlock(r eval.CalibrationReport) string {
	if !r.Separable {
		return strings.Join([]string{
			"verdict: NOT SEPARABLE",
			"  A non-duplicate pair scores at or above the closest duplicate pair, so no",
			"  merge threshold both merges every duplicate and spares every distinct fact.",
			"  Either the embedding model cannot tell these facts apart, or a probe in the",
			"  corpus is mislabelled.",
		}, "\n")
	}
	return fmt.Sprintf("verdict: SEPARABLE\n  A merge threshold exists: %.4f merges every duplicate with no false merge.",
		r.RecommendedMerge)
}

// ratio renders "7/12" so a column stays aligned as one field.
func ratio(n, total int) string { return fmt.Sprintf("%d/%d", n, total) }

func separationNote(separable bool) string {
	if separable {
		return "separated"
	}
	return "OVERLAP — no merge threshold is clean"
}

func overlapNote(overlap bool) string {
	if overlap {
		return "OVERLAP — a property of the model, not a tuning failure"
	}
	return "separated"
}

func classOf(r eval.CalibrationReport, class string) eval.ClassStats {
	for _, c := range r.Classes {
		if c.Class == class {
			return c
		}
	}
	return eval.ClassStats{Class: class}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
