// lc-bench drives the loomcycle model-capability benchmark. See
// bench/README.md for run instructions and design rationale.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/denn-gubsky/loomcycle/bench/internal/cases"
	"github.com/denn-gubsky/loomcycle/bench/internal/cost"
	"github.com/denn-gubsky/loomcycle/bench/internal/discover"
	"github.com/denn-gubsky/loomcycle/bench/internal/grader"
	"github.com/denn-gubsky/loomcycle/bench/internal/report"
	"github.com/denn-gubsky/loomcycle/bench/internal/runner"
)

const benchVersion = "0.1.0"

func main() {
	var (
		loomcycleURL = flag.String("loomcycle", envOrDefault("LOOMCYCLE_URL", "http://127.0.0.1:8787"), "loomcycle base URL")
		providersCSV = flag.String("providers", "deepseek,gemini,ollama,ollama-local", "comma-separated provider keys (must match loomcycle's registered provider IDs)")
		modelsFilter = flag.String("models", "", "optional regexp; only models matching are tested")
		tierFlag     = flag.String("tier", "", "limit to a tier (low|middle); empty = both")
		budgetUSD    = flag.Float64("budget", 25.0, "hard ceiling on aggregate USD cost; sweep halts when exceeded")
		quick        = flag.Bool("quick", false, "smoke mode: 1 model per provider, 3 cases per tier")
		benchRoot    = flag.String("bench-root", autoRoot(), "path to the bench/ directory (cases + agents)")
		outRoot      = flag.String("out", "", "output directory (default: bench/results/<timestamp>)")
		caseTimeout  = flag.Duration("case-timeout", 4*time.Minute, "per-case timeout")
		noSemantic   = flag.Bool("no-semantic", false, "skip judge-model grading (pass-through semantic = 1.0)")
		dryRun       = flag.Bool("dry-run", false, "show what would run without executing")
		userTier     = flag.String("user-tier", "", "loomcycle user_tier name (recommended: 'bench' with fallback_on_error=false so first-turn failures don't leak fallback-provider errors into the matrix)")
		repeats      = flag.Int("repeats", 1, "run each (model, case) tuple N times; per-axis pass thresholds use the MEDIAN result across repeats. Use >1 for variance studies (N=3 is the operator's typical setting for A/B verification).")
		judges       = flag.String("judges", "anthropic", "CSV list of judge providers (anthropic,deepseek,gemini). Multiple judges → consensus = median score + concatenated notes, mitigates single-provider bias.")
	)
	flag.Parse()

	if *outRoot == "" {
		*outRoot = filepath.Join(*benchRoot, "results", time.Now().Format("2006-01-02-1504"))
	}
	if err := os.MkdirAll(*outRoot, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *outRoot, err)
	}

	bearer := os.Getenv("LOOMCYCLE_AUTH_TOKEN")
	if bearer == "" {
		log.Fatal("LOOMCYCLE_AUTH_TOKEN env var is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	providerKeys := splitCSV(*providersCSV)

	// --- 1. Load cases ---
	allCases, err := cases.LoadAll(*benchRoot, *tierFlag)
	if err != nil {
		log.Fatalf("load cases: %v", err)
	}
	if *quick {
		allCases = subsetForSmoke(allCases)
	}
	log.Printf("loaded %d cases (tier=%q, quick=%v)", len(allCases), *tierFlag, *quick)

	// --- 2. Discover models per provider ---
	var modelRe *regexp.Regexp
	if *modelsFilter != "" {
		re, err := regexp.Compile(*modelsFilter)
		if err != nil {
			log.Fatalf("invalid --models regexp: %v", err)
		}
		modelRe = re
	}
	discoveries := discover.Discover(ctx, providerKeys, modelRe)
	for _, d := range discoveries {
		if d.Err != nil {
			log.Printf("discover %s: %v", d.Provider, d.Err)
		} else {
			log.Printf("discover %s: %d models — %s", d.Provider, len(d.Models), strings.Join(d.Models, ", "))
		}
	}

	if *quick {
		discoveries = trimForSmoke(discoveries)
	}

	// --- 3. Open one HTTP MCP session ---
	cli := runner.NewClient(*loomcycleURL, bearer, nil)
	initCtx, cancelInit := context.WithTimeout(ctx, 30*time.Second)
	if err := cli.Initialize(initCtx); err != nil {
		cancelInit()
		log.Fatalf("loomcycle MCP initialize: %v", err)
	}
	cancelInit()
	defer func() {
		closeCtx, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_ = cli.Close(closeCtx)
	}()
	log.Printf("MCP session: %s (server: %s)", cli.SessionID(), cli.SessionID())

	// --- 4. Optional: judge(s) for semantic grading ---
	var judge grader.Judge
	if !*noSemantic {
		judgeNames := splitCSV(*judges)
		judge = newJudge(judgeNames)
		if judge == nil {
			log.Printf("⚠ no judge active — none of [%s] have an API key configured. Semantic axis will be pass-through.", strings.Join(judgeNames, ", "))
		} else if len(judgeNames) > 1 {
			log.Printf("judge consensus enabled: %s (median score + concatenated notes)", strings.Join(judgeNames, ", "))
		} else {
			log.Printf("judge: %s", judgeNames[0])
		}
	}

	// --- 5. Read agent system prompts once ---
	lowPrompt, err := readAgentPrompt(filepath.Join(*benchRoot, "agents", "low-tier-eval.md"))
	if err != nil {
		log.Fatalf("read low-tier-eval.md: %v", err)
	}
	middlePrompt, err := readAgentPrompt(filepath.Join(*benchRoot, "agents", "middle-tier-eval.md"))
	if err != nil {
		log.Fatalf("read middle-tier-eval.md: %v", err)
	}

	// --- 6. Iterate (provider, model) ---
	tracesDir := filepath.Join(*outRoot, "traces")
	if err := os.MkdirAll(tracesDir, 0o755); err != nil {
		log.Fatalf("mkdir traces: %v", err)
	}

	// Total expected runs = sum(models per provider) * cases * repeats.
	// Used for ETA reporting. Discoveries with errors are skipped so
	// their case-count contribution is the placeholder INCONCLUSIVE
	// rows below, NOT real runs in the ETA.
	totalRuns := 0
	for _, d := range discoveries {
		if d.Err == nil {
			totalRuns += len(d.Models) * len(allCases) * (*repeats)
		}
	}
	prog := &progressTracker{total: totalRuns, start: time.Now()}

	var outcomes []report.CaseOutcome
	var totalCost float64

	for _, d := range discoveries {
		if d.Err != nil {
			// Surface as INCONCLUSIVE rows so the matrix shows the provider tried.
			for _, c := range allCases {
				outcomes = append(outcomes, report.CaseOutcome{
					Provider: d.Provider, Model: "<unreachable>",
					Tier: c.Tier, CaseID: c.ID,
					Status: "failed", Error: d.Err.Error(),
				})
			}
			continue
		}
		for _, model := range d.Models {
			if totalCost >= *budgetUSD {
				log.Printf("budget reached ($%.2f >= $%.2f); halting", totalCost, *budgetUSD)
				goto done
			}
			modelOutcomes, modelCost := runOneModel(ctx, cli, judge, runOneModelInput{
				provider:     d.Provider,
				model:        model,
				benchCases:   allCases,
				lowPrompt:    lowPrompt,
				middlePrompt: middlePrompt,
				tracesDir:    tracesDir,
				caseTimeout:  *caseTimeout,
				budgetLeft:   *budgetUSD - totalCost,
				dryRun:       *dryRun,
				userTier:     *userTier,
				repeats:      *repeats,
				progress:     prog,
			})
			outcomes = append(outcomes, modelOutcomes...)
			totalCost += modelCost
		}
	}

done:
	// --- 7. Build + render the matrix ---
	matrix := report.Build(outcomes, benchVersion)
	if err := report.WriteAll(matrix, *outRoot); err != nil {
		log.Fatalf("write matrix: %v", err)
	}
	log.Printf("matrix written to %s (cost ≈ $%.2f)", *outRoot, totalCost)
	fmt.Println()
	_ = report.WriteMarkdown(os.Stdout, matrix)
}

type runOneModelInput struct {
	provider, model         string
	benchCases              []cases.Case
	lowPrompt, middlePrompt string
	tracesDir               string
	caseTimeout             time.Duration
	budgetLeft              float64
	dryRun                  bool
	userTier                string
	repeats                 int
	progress                *progressTracker
}

// progressTracker reports cumulative completion + ETA across the
// whole sweep. The Bench tool calls Tick() at the end of every
// case-run; Tick logs "i/N (P%, ETA T)" so background sweeps signal
// progress without waiting for matrix-write.
type progressTracker struct {
	total int
	done  int
	start time.Time
}

// Tick records one completed run and emits a progress log line.
// Called from runOneCase after the run + grade is finished.
func (p *progressTracker) Tick() {
	if p == nil || p.total <= 0 {
		return
	}
	p.done++
	elapsed := time.Since(p.start)
	pct := float64(p.done) / float64(p.total) * 100
	var eta time.Duration
	if p.done > 0 {
		per := elapsed / time.Duration(p.done)
		eta = per * time.Duration(p.total-p.done)
	}
	log.Printf("  progress: %d/%d (%.0f%%, ETA %s)", p.done, p.total, pct, eta.Round(time.Second))
}

func runOneModel(ctx context.Context, cli *runner.Client, judge grader.Judge, in runOneModelInput) ([]report.CaseOutcome, float64) {
	var outcomes []report.CaseOutcome
	var spent float64

	// One dynamic agent per (model, tier). Reused across all cases
	// for that tier. TTL of 7200s easily covers a single model sweep.
	registered := map[string]string{} // tier -> agent name

	for _, c := range in.benchCases {
		if spent >= in.budgetLeft {
			log.Printf("budget for this model exhausted; skipping remaining cases")
			break
		}
		// Register the per-tier agent on first use.
		agentName, ok := registered[c.Tier]
		if !ok {
			n, err := registerForTier(ctx, cli, in.provider, in.model, c.Tier, in.lowPrompt, in.middlePrompt, allowedToolsUnion(in.benchCases, c.Tier))
			if err != nil {
				log.Printf("register %s/%s/%s: %v", in.provider, in.model, c.Tier, err)
				outcomes = append(outcomes, report.CaseOutcome{
					Provider: in.provider, Model: in.model, Tier: c.Tier,
					CaseID: c.ID, Status: "failed",
					Error: "register_agent: " + err.Error(),
				})
				continue
			}
			registered[c.Tier] = n
			agentName = n
		}

		// Run + grade. When --repeats > 1, run the case N times and
		// aggregate via the median-pass rule (see aggregateRepeats).
		repeats := in.repeats
		if repeats <= 0 {
			repeats = 1
		}
		var runs []report.CaseOutcome
		for i := 0; i < repeats; i++ {
			repeatLabel := ""
			if repeats > 1 {
				repeatLabel = fmt.Sprintf(" repeat=%d/%d", i+1, repeats)
			}
			log.Printf("→ %s/%s %s/%s%s", in.provider, in.model, c.Tier, c.ID, repeatLabel)
			o := runOneCase(ctx, cli, judge, in.provider, in.model, agentName, c, in.tracesDir, in.caseTimeout, in.dryRun, in.userTier, repeats, i)
			spent += o.CostUSD
			runs = append(runs, o)
			in.progress.Tick()
		}
		outcomes = append(outcomes, aggregateRepeats(runs))
	}
	return outcomes, spent
}

// aggregateRepeats collapses N repeats of the same (model, case) into
// a single CaseOutcome using median-pass semantics:
//
//   - Structural / Functional: pass if MAJORITY of repeats pass.
//   - Semantic: median score across repeats.
//   - Cost / DurationMS: SUM (total spent / wall time).
//   - Status: "completed" if every repeat completed, else the first
//     non-completed status.
//   - Error / reasons: concatenated across repeats (truncated).
//
// Majority-pass on binary axes avoids the "1 flake out of 3 sinks
// the row" failure mode; median on semantic is robust against the
// judge giving one wild score.
func aggregateRepeats(runs []report.CaseOutcome) report.CaseOutcome {
	if len(runs) == 1 {
		return runs[0]
	}
	base := runs[0]
	structPass, funcPass, semScores := 0, 0, make([]float64, 0, len(runs))
	var costSum float64
	var durSum int64
	var firstNonCompletedStatus string
	for _, r := range runs {
		if r.Result.Structural.Pass {
			structPass++
		}
		if r.Result.Functional.Pass {
			funcPass++
		}
		semScores = append(semScores, r.Result.Semantic.Score)
		costSum += r.CostUSD
		durSum += r.DurationMS
		if r.Status != "completed" && firstNonCompletedStatus == "" {
			firstNonCompletedStatus = r.Status
		}
	}
	sort.Float64s(semScores)
	median := semScores[len(semScores)/2]
	majority := len(runs)/2 + 1
	base.Result.Structural.Pass = structPass >= majority
	base.Result.Functional.Pass = funcPass >= majority
	base.Result.Semantic.Score = median
	base.Result.Semantic.Pass = median >= 0.7
	if !base.Result.Structural.Pass {
		base.Result.Structural.Score = 0
	}
	if !base.Result.Functional.Pass {
		base.Result.Functional.Score = 0
	}
	base.CostUSD = costSum
	base.DurationMS = durSum
	if firstNonCompletedStatus != "" {
		base.Status = firstNonCompletedStatus
	}
	// Surface that this was an aggregated row so the reasons column
	// in the markdown matrix makes the N visible.
	note := fmt.Sprintf("aggregated %d repeats: S=%d/%d F=%d/%d sem median=%.2f",
		len(runs), structPass, len(runs), funcPass, len(runs), median)
	base.Result.Semantic.Reasons = append([]string{note}, base.Result.Semantic.Reasons...)
	return base
}

func runOneCase(ctx context.Context, cli *runner.Client, judge grader.Judge,
	provider, model, agentName string, c cases.Case,
	tracesDir string, perCaseTimeout time.Duration, dryRun bool, userTier string,
	repeats, repeatIdx int,
) report.CaseOutcome {
	o := report.CaseOutcome{
		Provider: provider, Model: model,
		Tier: c.Tier, CaseID: c.ID,
	}
	if dryRun {
		o.Status = "completed"
		o.Result.Structural.Pass = true
		o.Result.Structural.Score = 1
		o.Result.Functional.Pass = true
		o.Result.Functional.Score = 1
		o.Result.Semantic.Pass = true
		o.Result.Semantic.Score = 1
		return o
	}

	runCtx, cancel := context.WithTimeout(ctx, perCaseTimeout)
	defer cancel()

	start := time.Now()
	// Per-case allowed_tools narrowing: pass the case-declared
	// allow-list as the per-run AllowedTools. When the case declares
	// `allowed_tools: []` this disables tools entirely for the run.
	// When non-empty, loomcycle intersects with the registered
	// dynamic-agent allowlist (which is the union of all cases'
	// tools, since one agent serves all cases at a tier).
	result, err := cli.SpawnRun(runCtx, runner.SpawnRunArgs{
		Agent:        agentName,
		Segments:     []runner.PromptSegment{runner.UserTextSegment(c.InputText)},
		UserID:       "bench-user-fixture-001",
		UserTier:     userTier,
		AllowedTools: c.AllowedTools,
	})
	o.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		o.Status = "failed"
		o.Error = err.Error()
		if looksLikeDeprecatedModel(err.Error()) {
			log.Printf("⚠ DEPRECATED MODEL: %s/%s returned a 'no longer available' error. Exclude this model from --models filter on the next sweep. Full error: %s", provider, model, err.Error())
		}
		return o
	}
	o.Status = result.Status
	o.CostUSD = cost.EstimateUSD(provider, model, result)

	// Trace dump per (model, case[, repeat]). When repeats > 1, each
	// repeat gets its own trace file so we can inspect variance later.
	traceDir := filepath.Join(tracesDir, sanitize(provider)+"-"+sanitize(model))
	_ = os.MkdirAll(traceDir, 0o755)
	traceName := c.ID + ".json"
	if repeats > 1 {
		traceName = fmt.Sprintf("%s.repeat-%d.json", c.ID, repeatIdx+1)
	}
	if f, err := os.Create(filepath.Join(traceDir, traceName)); err == nil {
		_ = json.NewEncoder(f).Encode(result)
		f.Close()
	}

	// Grade.
	o.Result.Structural = grader.Structural(result.FinalText, c.Expected.Structural)
	o.Result.Functional = grader.Functional(result.Events, c.Expected.Functional)
	o.Result.Semantic = grader.Semantic(runCtx, judge, result.FinalText, c.Expected.Semantic)

	// If the provider emitted an EventError mid-run (content filter,
	// rate limit, stream stall), surface its message on whichever
	// axis is failing so the operator can tell "model couldn't do
	// it" from "provider rejected the prompt".
	if msg := firstProviderError(result.Events); msg != "" {
		o.Error = msg
		if !o.Result.Structural.Pass {
			o.Result.Structural.Reasons = append(o.Result.Structural.Reasons, "provider error: "+msg)
		}
		if !o.Result.Functional.Pass {
			o.Result.Functional.Reasons = append(o.Result.Functional.Reasons, "provider error: "+msg)
		}
	}
	return o
}

// firstProviderError returns the message of the first EventError in
// the trace, or "" if none. A non-empty result means the provider
// itself rejected something (content filter, 429, mid-stream cut) —
// distinct from a model that produced bad output.
func firstProviderError(events []runner.ProviderEvent) string {
	for _, e := range events {
		if e.Type == "error" {
			if e.ErrorMessage != "" {
				return e.ErrorMessage
			}
			return "provider emitted EventError with no message"
		}
	}
	return ""
}

// looksLikeDeprecatedModel returns true when the error message
// matches a provider's "this model is no longer available / has
// been deprecated" pattern. The bench logs a prominent warning
// when this fires so operators can prune the deprecated model
// from their filter regexp on the next sweep without digging
// through traces. (Encountered 2026-05-15 when gemini-2.0-flash
// began returning 404 NOT_FOUND with "no longer available to new
// users" — a deprecation, not a transient outage.)
//
// Match patterns are intentionally permissive — false positives
// here just produce an extra warning log, not a real failure.
func looksLikeDeprecatedModel(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "no longer available") ||
		strings.Contains(lower, "has been deprecated") ||
		strings.Contains(lower, "model is deprecated") ||
		(strings.Contains(lower, "not_found") && strings.Contains(lower, "model"))
}

// registerForTier registers one dynamic agent for this (model, tier).
// Name format keeps it traceable in the dynamic_agents table without
// colliding with static agents.
func registerForTier(ctx context.Context, cli *runner.Client,
	provider, model, tier, lowPrompt, middlePrompt string, allowedTools []string,
) (string, error) {
	var sysPrompt string
	switch tier {
	case "low":
		sysPrompt = lowPrompt
	case "middle":
		sysPrompt = middlePrompt
	default:
		return "", errors.New("unknown tier")
	}
	name := fmt.Sprintf("bench-%s-%s-%s", tier, sanitize(provider), sanitize(model))
	if len(name) > 64 {
		name = name[:64]
	}
	regCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := cli.RegisterAgent(regCtx, runner.RegisterAgentArgs{
		Name:         name,
		SystemPrompt: sysPrompt,
		AllowedTools: allowedTools,
		Provider:     provider,
		Model:        model,
		Tier:         tier,
		MaxTokens:    maxTokensForTier(tier),
		Description:  "bench: " + tier + " tier eval candidate " + provider + "/" + model,
		TTLSeconds:   7200,
	})
	return name, err
}

func maxTokensForTier(tier string) int {
	if tier == "middle" {
		return 32768
	}
	return 8192
}

// allowedToolsUnion gathers every distinct tool referenced by any
// case of this tier, plus the always-on Read tool. Dynamic agents
// must declare at least one allowed_tool per the v0.8.15 schema.
func allowedToolsUnion(allCases []cases.Case, tier string) []string {
	seen := map[string]bool{"Read": true}
	for _, c := range allCases {
		if c.Tier != tier {
			continue
		}
		for _, t := range c.AllowedTools {
			seen[t] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// subsetForSmoke picks 3 cases per tier — the first three by sort
// order. Plan calls for 1 model × 3 cases × 2 tiers ≈ $1.
func subsetForSmoke(in []cases.Case) []cases.Case {
	byTier := map[string][]cases.Case{}
	for _, c := range in {
		byTier[c.Tier] = append(byTier[c.Tier], c)
	}
	var out []cases.Case
	for _, tier := range []string{"low", "middle"} {
		cs := byTier[tier]
		if len(cs) > 3 {
			cs = cs[:3]
		}
		out = append(out, cs...)
	}
	return out
}

// trimForSmoke keeps only the first discovered model per provider.
func trimForSmoke(d []discover.Discovery) []discover.Discovery {
	out := make([]discover.Discovery, len(d))
	for i, x := range d {
		if x.Err == nil && len(x.Models) > 1 {
			x.Models = x.Models[:1]
		}
		out[i] = x
	}
	return out
}

func readAgentPrompt(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	body := string(b)
	// Strip YAML frontmatter if present.
	if strings.HasPrefix(body, "---\n") {
		if end := strings.Index(body[4:], "\n---\n"); end >= 0 {
			body = body[4+end+len("\n---\n"):]
		}
	}
	return strings.TrimSpace(body), nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func autoRoot() string {
	// Default to the bench/ directory containing this binary's source
	// tree. Fall back to "./bench" when the executable is run from
	// the repo root.
	wd, _ := os.Getwd()
	for _, candidate := range []string{
		filepath.Join(wd, "bench"),
		wd,
	} {
		if _, err := os.Stat(filepath.Join(candidate, "cases")); err == nil {
			return candidate
		}
	}
	return "./bench"
}

var sanitizeRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func sanitize(s string) string {
	return sanitizeRE.ReplaceAllString(s, "_")
}
