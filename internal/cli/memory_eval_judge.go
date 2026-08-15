package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
)

// judgeAgentName is the bundled agent whose judgement this command scores. Its system
// prompt is READ from the loaded config, never inlined — an eval that scored its own copy
// of a prompt reports on something nobody runs.
const judgeAgentName = "memory/judge"

// RunMemoryEvalJudge implements `loomcycle memory-eval-judge`: the write-time judge
// scored against a REAL model.
//
// WHY A SEPARATE COMMAND from memory-eval-live rather than another --corpus on it. That
// flag selects a fixture set for ONE prompt and one scoring rule; a judge run reads a
// different agent's prompt, asks a different question, and is scored on an asymmetry the
// extraction gate does not have (a false refusal fails, a false admission does not).
// Folding it in would make --corpus silently swap all three.
//
// WHAT IT IS FOR. The judge decides which facts stop being returned, so its failures are
// invisible in a way the extractor's are not: a bad extractor writes junk somebody sees,
// while a bad judge makes a true fact quietly stop appearing. That is why the judge shipped
// with this command rather than before it.
//
// Flags mirror memory-eval-live, minus the ones that only mean something for extraction.
func RunMemoryEvalJudge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory-eval-judge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerID := fs.String("provider", "", "provider id to score (must be declared in the config)")
	model := fs.String("model", "", "wire model name to score")
	effort := fs.String("effort", "", "reasoning effort hint (default: the judge agent's configured effort)")
	cfgPath := fs.String("config", "", "optional extra config layer")
	baseline := fs.String("baseline", "", "compare against this baseline file and gate on regressions")
	updateBaseline := fs.String("update-baseline", "", "write this run's scores into this baseline file")
	output := fs.String("output", "", "write the JSON report here (default: human-readable to stdout)")
	timeout := fs.Duration("timeout", 5*time.Minute, "wall clock per CANDIDATE (a batch gets this times its size)")
	maxTokens := fs.Int("max-tokens", 0, "per-call max_tokens (0 = provider default)")
	batchSize := fs.Int("batch", 0, "candidates per call (0 = the shipped consolidator's batch)")
	noGate := fs.Bool("no-gate", false, "report only; exit 0 even when the gate fails")
	ontologyTerms := fs.String("ontology-terms", "",
		"comma-separated tenant entity types to add to the base seed when expanding "+
			"{{memory:ontology}}. The mistyping cases need the type list the judge is meant to "+
			"check against, so this is defaulted for you when omitted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *providerID == "" || *model == "" {
		return fail(stderr, "memory-eval-judge: --provider and --model are required — a score is only "+
			"meaningful against a named (provider, model, effort) triple")
	}

	cfg, err := loadLayeredConfig(*cfgPath)
	if err != nil {
		return fail(stderr, "memory-eval-judge: %v", err)
	}
	agent, ok := cfg.Agents[judgeAgentName]
	if !ok {
		return fail(stderr, "memory-eval-judge: agent %q is not in this config — select the memory "+
			"bundle (LOOMCYCLE_PRESETS=base,memory) so the eval can read the SHIPPED judge prompt",
			judgeAgentName)
	}
	if agent.SystemPrompt == "" {
		return fail(stderr, "memory-eval-judge: agent %q has no system_prompt", judgeAgentName)
	}
	if *effort == "" {
		*effort = agent.Effort
	}

	// THE CORPUS CHOOSES THE ONTOLOGY when the operator did not, for the reason the
	// hierarchy corpus does: a `mistyped` case scored against a prompt that never listed
	// the types asks the model to check against something it was not shown, and then
	// reports its failure as a model weakness.
	corpus := eval.JudgeFixture()
	if strings.TrimSpace(*ontologyTerms) == "" {
		*ontologyTerms = eval.JudgeCorpusTerms
		fmt.Fprintf(stdout, "using -ontology-terms %q\n", *ontologyTerms)
	}
	systemPrompt, err := expandEvalPlaceholders(agent.SystemPrompt, *ontologyTerms)
	if err != nil {
		return fail(stderr, "memory-eval-judge: %v", err)
	}

	prov, err := providerbuild.Provider(cfg, *providerID)
	if err != nil {
		return fail(stderr, "memory-eval-judge: %v", err)
	}

	rep, err := eval.RunJudge(context.Background(), prov, eval.JudgeInput{
		Corpus:       corpus,
		SystemPrompt: systemPrompt,
		Provider:     *providerID,
		Model:        *model,
		Effort:       *effort,
		MaxTokens:    *maxTokens,
		BatchSize:    *batchSize,
		CaseTimeout:  *timeout,
	})
	if err != nil {
		return failOp(stderr, "memory-eval-judge: %v", err)
	}

	var base *eval.Baseline
	if *baseline != "" {
		b, berr := eval.LoadBaseline(*baseline)
		if berr != nil {
			return fail(stderr, "memory-eval-judge: baseline: %v", berr)
		}
		base = &b
	}

	if *output != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if werr := os.WriteFile(*output, append(b, '\n'), 0o644); werr != nil {
			return failOp(stderr, "memory-eval-judge: write report: %v", werr)
		}
		fmt.Fprintf(stdout, "wrote report to %s\n", *output)
	} else {
		printJudgeReport(stdout, rep, base)
	}

	if *updateBaseline != "" {
		if werr := eval.SaveBaselineEntry(*updateBaseline, rep); werr != nil {
			return failOp(stderr, "memory-eval-judge: update baseline: %v", werr)
		}
		fmt.Fprintf(stdout, "\nbaseline updated: %s\n", *updateBaseline)
	}

	fails := eval.DefaultJudgeGate().Check(rep)
	if base != nil {
		fails = append(fails, base.Regressions(rep)...)
		if stale, had := base.StaleMatch(rep); had {
			fmt.Fprintf(stderr,
				"\nNOTE: no baseline for this exact configuration, so NOTHING was compared.\n"+
					"      %s/%s effort=%q was measured on %s under a different prompt/corpus\n"+
					"      (prompt %s… vs now %s…). Re-record with --update-baseline to gate it again.\n",
				rep.Provider, rep.Model, rep.Effort, stale.MeasuredAt,
				shortSHA(stale.SystemPromptSHA256), shortSHA(rep.SystemPromptSHA256))
		}
	}
	// A harness fault always fails, even under --no-gate: the flag means "do not block
	// on scores", and a faulted run has no scores to not-block on.
	if rep.HarnessFault != "" {
		fmt.Fprintf(stderr, "\nHARNESS FAULT: %s\n", rep.HarnessFault)
		return failOp(stderr, "memory-eval-judge: no scores produced")
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

// printJudgeReport renders the per-ability table and then every case that did not match.
//
// FALSE REFUSALS ARE PRINTED FIRST and marked, because they are the only failures that
// lose data. An operator skimming this needs to see them before the fabrication misses,
// which are merely the status quo.
func printJudgeReport(w io.Writer, r eval.JudgeReport, base *eval.Baseline) {
	fmt.Fprintf(w, "\nmemory judge eval\n")
	fmt.Fprintf(w, "  provider/model       %s/%s\n", r.Provider, r.Model)
	fmt.Fprintf(w, "  effort               %q\n", r.Effort)
	fmt.Fprintf(w, "  judge prompt         %s\n", shortSHA(r.SystemPromptSHA256))
	fmt.Fprintf(w, "  corpus               %s\n", shortSHA(r.CorpusSHA256))
	if r.HarnessFault != "" {
		fmt.Fprintf(w, "\n  ⚠️  HARNESS FAULT: %s\n", r.HarnessFault)
		return
	}

	fmt.Fprintf(w, "\n  %-14s %5s %8s %11s %7s %8s\n",
		"ability", "cases", "recall", "refusals", "clean", "delta")
	for _, s := range r.Abilities {
		recall := "     -"
		if s.Recall >= 0 {
			recall = fmt.Sprintf("%6.2f", s.Recall)
		}
		delta := ""
		if base != nil {
			delta = base.DeltaFor(r, s)
		}
		fmt.Fprintf(w, "  %-14s %5d %8s %11d %7d %8s\n",
			s.Ability, s.Cases, recall, s.Violations, s.CleanCases, delta)
	}

	// The two directions, side by side and named, so neither reads as the other.
	fmt.Fprintf(w, "\n  false refusals       %d   (true facts withheld — the gate)\n", r.TotalViolations)
	fmt.Fprintf(w, "  fabrications kept    %d   (reported only: this is the status quo)\n",
		r.AdmittedFabrications)

	var refusals, other []eval.JudgeCaseResult
	for _, c := range r.Cases {
		switch {
		case c.FalseRefusal():
			refusals = append(refusals, c)
		case !c.Matched():
			other = append(other, c)
		}
	}
	for _, group := range []struct {
		label string
		cases []eval.JudgeCaseResult
	}{{"FALSE REFUSALS — a true fact was withheld", refusals}, {"other misses", other}} {
		if len(group.cases) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", group.label)
		for _, c := range group.cases {
			got := c.Got
			switch {
			case c.Err != "":
				got = "ERROR " + c.Err
			case c.Unreadable:
				got = "unreadable reply"
			case got == "":
				got = "no verdict for this candidate"
			}
			fmt.Fprintf(w, "\n  %s [%s]\n", c.Name, c.Case.Ability)
			fmt.Fprintf(w, "    want %s, got %s\n", c.Case.Want, got)
			fmt.Fprintf(w, "    why  %s\n", c.Case.Why)
			if c.Reason != "" {
				fmt.Fprintf(w, "    said %q\n", c.Reason)
			}
		}
	}
}
