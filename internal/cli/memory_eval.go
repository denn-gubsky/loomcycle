package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
)

// RunMemoryEval implements the `loomcycle memory-eval` subcommand (RFC I
// MR-5 / Decision 5): the memory retrieval-quality harness. It seeds a
// corpus into the REAL in-process memory backend (ranker + search-time
// dedup), runs the dataset's queries, and prints precision@k / recall@k /
// duplication_rate / recall latency percentiles.
//
// THE EVAL HARNESS IS THE GATING TOOL FOR RANKER + DEDUP CHANGES: run it
// before and after a change and compare the metrics. The bundled dataset
// uses a DETERMINISTIC stub embedder (no provider key, reproducible in CI)
// — it validates the plumbing and metric math but is NOT a semantic
// benchmark. For a real quality number, pass --dataset <file.jsonl> and
// run against your real memory stack with a real embedder.
//
// Flags:
//
//	--dataset bundled|<path.jsonl>   default "bundled"
//	--rank-config <file.json>        optional RankConfig override (applies to every query)
//	--output <report.json>           optional; default stdout
//	--embed-dim <n>                  deterministic-embedder dimension (default 64)
func RunMemoryEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory-eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataset := fs.String("dataset", "bundled", "dataset: 'bundled' or a path to a .jsonl file")
	rankConfig := fs.String("rank-config", "", "optional path to a RankConfig JSON file applied to every query")
	output := fs.String("output", "", "optional path to write the JSON report (default: stdout)")
	embedDim := fs.Int("embed-dim", 64, "deterministic embedder output dimension")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// A bad --dataset (missing file, malformed JSONL) is a user/invocation
	// error → exit 2 via fail(), matching cli.go's documented convention
	// (and consistent with the flag-parse failure above, which also exits
	// 2). The eval RUN below is operational → exit 1 via failOp().
	ds, err := loadEvalDataset(*dataset)
	if err != nil {
		return fail(stderr, "memory-eval: %v", err)
	}

	// --rank-config overrides whatever the dataset declared. This is how an
	// operator A/B-tests a ranker weight change without editing the dataset.
	// A bad config path/contents is also a user error → exit 2.
	if *rankConfig != "" {
		rc, err := loadRankConfig(*rankConfig)
		if err != nil {
			return fail(stderr, "memory-eval: rank-config: %v", err)
		}
		ds.Rank = &rc
	}

	emb := eval.NewDeterministicEmbedder(*embedDim)
	rep, err := eval.Run(context.Background(), ds, emb)
	if err != nil {
		return failOp(stderr, "memory-eval: %v", err)
	}

	if *output != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*output, append(b, '\n'), 0o644); err != nil {
			return failOp(stderr, "memory-eval: write report: %v", err)
		}
		fmt.Fprintf(stdout, "wrote report to %s\n", *output)
		return 0
	}

	printEvalReport(stdout, rep)
	return 0
}

func loadEvalDataset(name string) (eval.Dataset, error) {
	if name == "" || name == "bundled" {
		return eval.BundledDataset()
	}
	f, err := os.Open(name)
	if err != nil {
		return eval.Dataset{}, fmt.Errorf("open dataset: %w", err)
	}
	defer func() { _ = f.Close() }()
	return eval.LoadJSONL(f)
}

func loadRankConfig(path string) (memory.RankConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return memory.RankConfig{}, fmt.Errorf("read: %w", err)
	}
	var rc memory.RankConfig
	if err := json.Unmarshal(b, &rc); err != nil {
		return memory.RankConfig{}, fmt.Errorf("parse: %w", err)
	}
	return rc, nil
}

// printEvalReport renders the metrics table (the RFC example shape).
func printEvalReport(w io.Writer, r eval.Report) {
	fmt.Fprintf(w, "memory-eval report: %s\n", r.Dataset)
	fmt.Fprintf(w, "  embedder              %s\n", r.Embedder)
	fmt.Fprintf(w, "  corpus_size          %d\n", r.CorpusSize)
	fmt.Fprintf(w, "  queries              %d\n", r.Queries)
	fmt.Fprintf(w, "  top_k                %d\n", r.TopK)
	fmt.Fprintf(w, "  precision@k          %.4f\n", r.PrecisionAtK)
	fmt.Fprintf(w, "  recall@k             %.4f\n", r.RecallAtK)
	fmt.Fprintf(w, "  duplication_rate     %.4f\n", r.DuplicationRate)
	fmt.Fprintf(w, "  recall_latency_p50   %.3f ms\n", r.RecallLatencyP50Ms)
	fmt.Fprintf(w, "  recall_latency_p99   %.3f ms\n", r.RecallLatencyP99Ms)
	fmt.Fprintf(w, "\nNote: the bundled dataset uses a deterministic stub embedder — "+
		"reproducible, not semantic. For real numbers run --dataset <file> against a real embedder.\n")
}
