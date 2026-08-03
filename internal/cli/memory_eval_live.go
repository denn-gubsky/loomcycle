package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"io"
	"os"
	"regexp"
	"strings"
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
//	--timeout <dur>          wall clock PER CASE (default 5m; local models are slow)
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
	timeout := fs.Duration("timeout", 5*time.Minute, "wall clock PER CASE (not for the whole run)")
	maxTokens := fs.Int("max-tokens", 0, "per-call max_tokens (0 = provider default)")
	noGate := fs.Bool("no-gate", false, "report only; exit 0 even when the gate fails")
	ontologyTerms := fs.String("ontology-terms", "",
		"comma-separated tenant entity types to add to the base seed when expanding "+
			"{{memory:ontology}} (a deployment with a CONFIRMED tenant ontology adds its own; "+
			"empty = the base seed alone, which is what an unconfirmed deployment gets)")
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

	// EXPAND THE PLACEHOLDERS THE RUNTIME WOULD EXPAND, or the eval scores a prompt
	// nobody runs — the exact failure the bundle drift-test was built to prevent,
	// arriving by a route that test cannot see.
	//
	// The drift test compares this harness's copy against the bundle's, and both
	// have been identical throughout. What differs is that the RUNTIME expands
	// {{memory:ontology}} into ~520 characters of entity types before the model sees
	// it, and this path sent the nineteen literal characters. So since the ontology
	// landed in the extractor prompt, every score here was measured against a prompt
	// telling the model to "use one of the types above" where above was a
	// placeholder — which is a complete explanation for the eval's finding that half
	// the emitted types were outside the ontology, and for why the live deployment,
	// where expansion does happen, used its ontology's types correctly.
	systemPrompt, err := expandEvalPlaceholders(agent.SystemPrompt, *ontologyTerms)
	if err != nil {
		return fail(stderr, "memory-eval-live: %v", err)
	}

	prov, err := providerbuild.Provider(cfg, *providerID)
	if err != nil {
		return fail(stderr, "memory-eval-live: %v", err)
	}

	// PER-CASE, not per-run. A total budget makes the LAST cases the victims of a
	// slow model: a 36B local model got through 8 of 13 and the remaining 5 came
	// back as timeouts, which scored as zero recall on two abilities and a gate
	// failure blaming the model for a question it was never asked. Which cases get
	// measured should not depend on where the wall clock happens to land.
	rep, err := eval.RunExtraction(context.Background(), prov, eval.ExtractionInput{
		CaseTimeout:  *timeout,
		Corpus:       eval.ExtractionFixture(),
		SystemPrompt: systemPrompt,
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
		// Say when the baseline gate is NOT active for this configuration although
		// it once was. A prompt or corpus change un-gates every measured
		// model/effort at once, and Regressions is silent about it by design — so
		// without this the run reports a clean pass with no comparison behind it.
		if stale, had := base.StaleMatch(rep); had {
			fmt.Fprintf(stderr,
				"\nNOTE: no baseline for this exact configuration, so NOTHING was compared.\n"+
					"      %s/%s effort=%q was measured on %s under a different prompt/corpus\n"+
					"      (prompt %s… vs now %s…). Re-record with --update-baseline to gate it again.\n",
				rep.Provider, rep.Model, rep.Effort, stale.MeasuredAt,
				shortSHA(stale.SystemPromptSHA256), shortSHA(rep.SystemPromptSHA256))
		}
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

	if r.TotalErrors > 0 {
		// Stated ABOVE the table, because the table's numbers describe a subset of
		// the corpus and a reader who skims the columns first will otherwise take
		// them for a full measurement.
		msg := fmt.Sprintf("!! INCOMPLETE — %d case(s) never produced an answer; the figures below cover only the rest", r.TotalErrors)
		if r.BudgetExhausted {
			msg += ", and the per-case wall clock expired (raise --timeout)"
		}
		fmt.Fprintf(w, "\n  %s\n", wrapText(msg, 74, "  "))
	}

	fmt.Fprintf(w, "\nper-ability\n")
	fmt.Fprintf(w, "  %-12s %6s %9s %11s %8s %6s %7s\n",
		"ability", "cases", "recall", "violations", "clean", "typed", "errors")
	for _, s := range r.Abilities {
		recall := "     n/a"
		if s.Recall >= 0 {
			recall = fmt.Sprintf("%8.2f", s.Recall)
		}
		errs := "       "
		if s.Errors > 0 {
			errs = fmt.Sprintf("%7d", s.Errors)
		}
		delta := ""
		if base != nil {
			delta = base.DeltaFor(r, s)
		}
		// typed = the entity-pair rate. "n/a" when nothing was emitted, never 0.00
		// — a corpus with nothing to type would otherwise read as a model that
		// refused to type, which is the opposite diagnosis.
		typed := "   n/a"
		if tr := s.TypedRate(); tr >= 0 {
			typed = fmt.Sprintf("%6.2f", tr)
		}
		answered := s.Cases - s.Errors
		fmt.Fprintf(w, "  %-12s %6d %9s %11d %5d/%d %s %s%s\n",
			s.Ability, s.Cases, recall, s.Violations, s.CleanCases, answered, typed, errs, delta)
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
			// The entity pair is appended rather than always shown, so an untyped
			// fact reads exactly as it did before and a typed one is unmistakable.
			// Without this the pair was invisible here even once the struct carried
			// it, which is how it went unmeasured in the first place.
			entity := ""
			if f.HasEntity() {
				entity = fmt.Sprintf("  → %s:%s", f.Type, f.Subject)
			}
			fmt.Fprintf(w, "  emitted    [%s] %s%s\n", f.Class, f.Text, entity)
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

// expandEvalPlaceholders renders the {{memory:...}} placeholders the RUNTIME would
// render, and refuses a prompt still carrying one.
//
// Only `ontology` is expandable here, and that is a real limit rather than an
// oversight: the other variants read a live store (a user's core blocks, a tenant's
// deployment-context document) that a CLI with no store cannot produce. A prompt
// referencing one of those is therefore not scoreable — and it fails LOUDLY rather
// than being sent with the literal text, which is what quietly happened to
// `ontology` and cost a whole baseline's worth of meaning.
//
// tenantTerms lets a run reproduce a deployment whose operator has CONFIRMED an
// ontology. Empty is the unconfirmed case — the base seed alone — and the two give
// materially different results, which is the entire question the flag exists to ask.
func expandEvalPlaceholders(prompt, tenantTerms string) (string, error) {
	var extra []meminject.OntologyTerm
	for _, name := range strings.Split(tenantTerms, ",") {
		if name = strings.TrimSpace(name); name != "" {
			extra = append(extra, meminject.OntologyTerm{Name: name})
		}
	}
	// confirmed=true only when terms were supplied: EffectiveOntology discards a
	// tenant layer that is not confirmed, so passing true with no terms would render
	// the "this deployment has confirmed additions" wording over the bare seed.
	body := meminject.RenderOntology(
		meminject.EffectiveOntology(extra, len(extra) > 0), len(extra) > 0)
	out := strings.ReplaceAll(prompt, "{{memory:"+string(meminject.VariantOntology)+"}}", body)

	// Anything left is a variant this harness cannot render. Refusing is the point:
	// sending it verbatim scores a prompt the runtime never produces, and the score
	// looks entirely normal.
	if m := unexpandedPlaceholder.FindString(out); m != "" {
		return "", fmt.Errorf("the extractor prompt still contains %s, which this harness cannot render "+
			"(it reads a live store). Scoring it would measure a prompt the runtime never sends", m)
	}
	return out, nil
}

// unexpandedPlaceholder matches any surviving {{...}} of either family.
var unexpandedPlaceholder = regexp.MustCompile(`\{\{\s*(memory|tool)\s*:[^}]*\}\}`)
