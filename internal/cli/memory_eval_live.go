package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
)

// extractorAgentName is the bundled agent whose judgement this command scores.
// Its system prompt is READ from the loaded config, never inlined here — see
// RunMemoryEvalLive.
const extractorAgentName = "memory/extractor"

// RunMemoryEvalLive implements `loomcycle memory-eval-live`: the memory
// extraction eval against a REAL model.
//
// HOW IT DIFFERS FROM ITS TWO SIBLINGS, since three "memory eval" things is one
// too many to keep straight:
//
//   - `make memory-eval-mock` proves the consolidation PIPELINE — queue,
//     watermark, provenance, dedup, erasure — with a scripted provider. Offline,
//     hermetic, per-PR. It says nothing about judgement.
//   - `loomcycle memory-eval` measures RETRIEVAL quality (precision/recall over a
//     corpus) with a stub embedder.
//   - this one measures the extractor's JUDGEMENT: given a transcript, does the
//     model write down the right things, and refuse the wrong ones. That needs a
//     real model, so it is not hermetic and not per-PR.
//
// It exists because judgement was the standing problem and there was no
// instrument for it. Tuning the extractor by hand produced three confidently
// wrong conclusions in an afternoon, because the ad-hoc harness was sending an
// empty transcript and a well-formed empty reply looks identical to a model
// deciding there was nothing to record. The corpus carries a canary for exactly
// that, and this command refuses to print scores when it trips.
//
// Flags:
//
//	--provider <id>          required; a provider id declared in the config
//	--model <name>           required; the wire model name to score
//	--effort <low|medium|high>  default: the extractor agent's configured effort
//	--config <path>          optional extra config layer
//	--baseline <path>        compare against a committed baseline, and gate on regressions
//	--update-baseline <path> write this run's scores into a baseline file
//	--output <path>          write the JSON report (default: human-readable to stdout)
//	--timeout <dur>          per-run wall clock (default 10m; local models are slow)
//	--no-gate                report only; exit 0 even on gate failure
func RunMemoryEvalLive(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory-eval-live", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerID := fs.String("provider", "", "provider id to score (must be declared in the config)")
	model := fs.String("model", "", "wire model name to score")
	effort := fs.String("effort", "", "reasoning effort hint (default: the extractor agent's configured effort)")
	cfgPath := fs.String("config", "", "optional extra config layer")
	baseline := fs.String("baseline", "", "compare against this baseline file and gate on regressions")
	updateBaseline := fs.String("update-baseline", "", "write this run's scores into this baseline file")
	output := fs.String("output", "", "write the JSON report here (default: human-readable to stdout)")
	timeout := fs.Duration("timeout", 10*time.Minute, "wall clock for the whole run")
	maxTokens := fs.Int("max-tokens", 0, "per-call max_tokens (0 = provider default)")
	noGate := fs.Bool("no-gate", false, "report only; exit 0 even when the gate fails")
	showFacts := fs.Bool("show-facts", false, "print every case's emitted facts, including the ones that passed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *providerID == "" || *model == "" {
		return fail(stderr, "memory-eval-live: --provider and --model are required — a score is only "+
			"meaningful against a named (provider, model, effort) triple")
	}

	cfg, err := loadLayeredConfig(*cfgPath)
	if err != nil {
		return fail(stderr, "memory-eval-live: %v", err)
	}

	// The system prompt comes from the SHIPPED bundle, not from this file. That is
	// the whole point: an eval that scored its own copy of the prompt would report
	// on something nobody runs. A deployment without the memory bundle selected has
	// no extractor to score, and saying so beats scoring a default.
	agent, ok := cfg.Agents[extractorAgentName]
	if !ok {
		return fail(stderr, "memory-eval-live: agent %q is not in this config — select the memory bundle "+
			"(LOOMCYCLE_PRESETS=base,memory) so the eval can read the SHIPPED extractor prompt",
			extractorAgentName)
	}
	if agent.SystemPrompt == "" {
		return fail(stderr, "memory-eval-live: agent %q has no system_prompt", extractorAgentName)
	}
	if *effort == "" {
		*effort = agent.Effort
	}

	prov, err := providerbuild.Provider(cfg, *providerID)
	if err != nil {
		return fail(stderr, "memory-eval-live: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	rep, err := eval.RunExtraction(ctx, prov, eval.ExtractionInput{
		Corpus:       eval.ExtractionFixture(),
		SystemPrompt: agent.SystemPrompt,
		Provider:     *providerID,
		Model:        *model,
		Effort:       *effort,
		MaxTokens:    *maxTokens,
	})
	if err != nil {
		return failOp(stderr, "memory-eval-live: %v", err)
	}

	var base *eval.Baseline
	if *baseline != "" {
		b, berr := eval.LoadBaseline(*baseline)
		if berr != nil {
			return fail(stderr, "memory-eval-live: baseline: %v", berr)
		}
		base = &b
	}

	if *output != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if werr := os.WriteFile(*output, append(b, '\n'), 0o644); werr != nil {
			return failOp(stderr, "memory-eval-live: write report: %v", werr)
		}
		fmt.Fprintf(stdout, "wrote report to %s\n", *output)
	} else {
		printExtractionReport(stdout, rep, base, *showFacts)
	}

	if *updateBaseline != "" {
		if werr := eval.SaveBaselineEntry(*updateBaseline, rep); werr != nil {
			return failOp(stderr, "memory-eval-live: update baseline: %v", werr)
		}
		fmt.Fprintf(stdout, "\nbaseline updated: %s\n", *updateBaseline)
	}

	// Gate. A harness fault ALWAYS fails, even under --no-gate: the flag means
	// "don't block on scores", and a faulted run has no scores to not-block on.
	fails := eval.DefaultGate().Check(rep)
	if base != nil {
		fails = append(fails, base.Regressions(rep)...)
	}
	if rep.HarnessFault != "" {
		fmt.Fprintf(stderr, "\nHARNESS FAULT: %s\n", rep.HarnessFault)
		return failOp(stderr, "memory-eval-live: no scores produced")
	}
	if len(fails) > 0 {
		fmt.Fprintf(stderr, "\nGATE FAILED:\n")
		for _, f := range fails {
			fmt.Fprintf(stderr, "  - %s\n", f)
		}
		if *noGate {
			fmt.Fprintf(stderr, "  (--no-gate: reporting only)\n")
			return 0
		}
		return 1
	}
	fmt.Fprintf(stdout, "\ngate PASSED\n")
	return 0
}

// printExtractionReport renders the per-ability table plus every miss and
// violation. Layout mirrors printEvalReport / printCalibrationReport: an operator
// reads this in a terminal next to the prompt they are about to change.
// showFacts prints the emitted facts for EVERY case, not just the ones with a
// problem. The default report is problem-only, which is right for a regression
// check and wrong for the first measurement of a new model: the first live run
// scored 2/2 on the ability that fails in production, and there was no way to read
// what the model had actually written without re-running.
func printExtractionReport(w io.Writer, r eval.ExtractionReport, base *eval.Baseline, showFacts bool) {
	fmt.Fprintf(w, "memory-eval-live: %s / %s", r.Provider, r.Model)
	if r.Effort != "" {
		fmt.Fprintf(w, " (effort=%s)", r.Effort)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  extractor prompt     %s\n", shortSHA(r.SystemPromptSHA256))
	fmt.Fprintf(w, "  corpus               %s\n", shortSHA(r.CorpusSHA256))

	if r.HarnessFault != "" {
		fmt.Fprintf(w, "\n  !! HARNESS FAULT — scores withheld\n")
		fmt.Fprintf(w, "  %s\n", wrapText(r.HarnessFault, 76, "  "))
		return
	}

	fmt.Fprintf(w, "\nper-ability\n")
	fmt.Fprintf(w, "  %-12s %6s %9s %11s %8s\n", "ability", "cases", "recall", "violations", "clean")
	for _, s := range r.Abilities {
		recall := "     n/a"
		if s.Recall >= 0 {
			recall = fmt.Sprintf("%8.2f", s.Recall)
		}
		delta := ""
		if base != nil {
			delta = base.DeltaFor(r, s)
		}
		fmt.Fprintf(w, "  %-12s %6d %9s %11d %5d/%d%s\n",
			s.Ability, s.Cases, recall, s.Violations, s.CleanCases, s.Cases, delta)
	}
	fmt.Fprintf(w, "\n  total violations     %d\n", r.TotalViolations)

	// Every miss and violation, with the fixture's own reason. A score with no
	// explanation is not actionable, and the reason is what tells you whether to
	// change the prompt or the corpus.
	for _, c := range r.Cases {
		quiet := c.Err == "" && len(c.Misses) == 0 && len(c.Violations) == 0 &&
			len(c.ClassMismatches) == 0 && !c.EmptyReply
		if quiet && !showFacts {
			continue
		}
		fmt.Fprintf(w, "\n%s [%s]\n", c.Name, c.Ability)
		if c.Err != "" {
			fmt.Fprintf(w, "  ERROR      %s\n", c.Err)
		}
		if c.RawReply != "" {
			fmt.Fprintf(w, "  reply      %s\n", c.RawReply)
		}
		for _, m := range c.Misses {
			fmt.Fprintf(w, "  MISS       %s\n", wrapText(m, 72, "             "))
		}
		for _, v := range c.Violations {
			fmt.Fprintf(w, "  VIOLATION  %s\n", wrapText(v, 72, "             "))
		}
		for _, m := range c.ClassMismatches {
			fmt.Fprintf(w, "  class      %s\n", m)
		}
		if c.EmptyReply {
			// Production treats this as zero facts and consolidates the chat, so it
			// is NOT a failure — but a rising rate is the earliest sign of a
			// degrading extractor, which is why it is surfaced at all.
			note := "empty reply (no `[]`) — production counts this and treats it as zero facts"
			if c.ThinkingOnly {
				note = "REASONING TRACE ONLY, no answer — on Ollama effort=medium sets think:true; " +
					"try --effort low or a larger --max-tokens"
			}
			fmt.Fprintf(w, "  note       %s\n", wrapText(note, 72, "             "))
		}
		if c.Dropped > 0 {
			fmt.Fprintf(w, "  dropped    %d fact(s) production would discard\n", c.Dropped)
		}
		for _, f := range c.Facts {
			fmt.Fprintf(w, "  emitted    [%s] %s\n", f.Class, f.Text)
		}
	}

	if base == nil {
		fmt.Fprintf(w, "\nNote: no --baseline given, so these numbers are compared against nothing. "+
			"Pass --baseline to gate on regressions.\n")
	}
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(unknown)"
	}
	return s
}

// wrapText wraps at width, indenting continuation lines, so a long fixture reason
// stays readable in a terminal.
func wrapText(s string, width int, indent string) string {
	words := splitWords(s)
	if len(words) == 0 {
		return s
	}
	var out, line string
	for _, w := range words {
		if line == "" {
			line = w
			continue
		}
		if len(line)+1+len(w) > width {
			out += line + "\n" + indent
			line = w
			continue
		}
		line += " " + w
	}
	return out + line
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
