// Command longhorizon is the RFC CS long-horizon benchmark harness (P1).
//
// It measures the RFC CR retention regimes — A0 append (today), A1 recap, A2
// structured state — on a synthetic, horizon-scalable, oracle-graded counter task,
// driving the model through loomcycle's OpenAI-compatible gateway so every arm
// shares provider routing and the RFC AV cost ledger. It reports cumulative tokens
// (the O(T^2) vs O(T) curve), peak single-prompt size, and task accuracy.
//
// The synthetic task is the CLEAN-CURVE instrument and is maximally schema-able, so
// it likely OVERSTATES the A2 win (RFC CS Q3). The go/no-go weights the controlled
// marketing-research team run (P2) higher; this harness produces the cost curves and
// the first A0-vs-A1 (local gate) / A0-vs-A2 (frontier gate) signal.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	var (
		base      = flag.String("base", "http://127.0.0.1:8787", "loomcycle base URL (OpenAI-compat gateway)")
		bearer    = flag.String("bearer", os.Getenv("LONGHORIZON_BEARER"), "bearer token (default $LONGHORIZON_BEARER)")
		model     = flag.String("model", "", "model / alias to route to (required)")
		arm       = flag.String("arm", "all", "A0 | A1 | A2 | all")
		horizon   = flag.Int("horizon", 50, "number of instructions T")
		keys      = flag.Int("keys", 5, "number of counters")
		seed      = flag.Int64("seed", 1, "base RNG seed")
		seeds     = flag.Int("seeds", 1, "number of consecutive seeds to run + average")
		noisePct  = flag.Int("noise", 0, "percent of steps that are distractor NOTE lines (0..100)")
		drift     = flag.Bool("drift", false, "inject one external CORRECTION at the midpoint")
		keepLastN = flag.Int("keep-last-n", 6, "A1 verbatim window (steps)")
		timeout   = flag.Duration("timeout", 120*time.Second, "per-call HTTP timeout")
		out       = flag.String("out", "", "append JSONL results to this file")
	)
	flag.Parse()

	if *model == "" {
		fmt.Fprintln(os.Stderr, "longhorizon: -model is required")
		os.Exit(2)
	}
	arms := []string{"A0", "A1", "A2"}
	if *arm != "all" {
		arms = []string{strings.ToUpper(*arm)}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := NewModelClient(*base, *bearer, *model, *timeout)

	var results []RunResult
	for _, a := range arms {
		for i := 0; i < *seeds; i++ {
			s := *seed + int64(i)
			task := GenerateTask(s, *horizon, *keys, float64(*noisePct)/100, *drift)
			strat := newStrategy(a, task, *keepLastN)
			if strat == nil {
				fmt.Fprintf(os.Stderr, "unknown arm %q\n", a)
				os.Exit(2)
			}
			r := RunOnce(ctx, strat, task, client)
			r.NoisePct = *noisePct
			r.Drift = *drift
			results = append(results, r)
			if r.Err != "" {
				fmt.Fprintf(os.Stderr, "%s seed=%d: ERROR %s\n", a, s, r.Err)
			}
			if ctx.Err() != nil {
				break
			}
		}
	}

	if *out != "" {
		if err := appendJSONL(*out, results); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		}
	}
	printSummary(results, *horizon)
}

func appendJSONL(path string, rs []RunResult) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// printSummary aggregates results by arm (averaging across seeds) into a table.
func printSummary(rs []RunResult, horizon int) {
	type agg struct {
		n                     int
		totalTok, peakTok     int
		acc                   float64
		stepCalls, recapCalls int
		elapsed               int64
	}
	order := []string{}
	m := map[string]*agg{}
	for _, r := range rs {
		a, ok := m[r.Arm]
		if !ok {
			a = &agg{}
			m[r.Arm] = a
			order = append(order, r.Arm)
		}
		a.n++
		a.totalTok += r.TotalTokens
		a.peakTok += r.PeakPromptTokens
		a.acc += r.Accuracy
		a.stepCalls += r.StepCalls
		a.recapCalls += r.RecapCalls
		a.elapsed += r.ElapsedMs
	}
	fmt.Printf("\nlonghorizon  T=%d  (avg over seeds)\n", horizon)
	fmt.Printf("%-12s %12s %12s %10s %8s %8s %8s\n",
		"arm", "total_tok", "peak_prompt", "accuracy", "steps", "recaps", "ms")
	for _, name := range order {
		a := m[name]
		n := float64(a.n)
		fmt.Printf("%-12s %12d %12d %9.3f %8d %8d %8d\n",
			name, a.totalTok/a.n, a.peakTok/a.n, a.acc/n,
			a.stepCalls/a.n, a.recapCalls/a.n, a.elapsed/int64(a.n))
	}
}
