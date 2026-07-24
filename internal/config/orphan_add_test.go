package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsolidationCaps_AreOperatorTunable: both fan-out caps were DOCUMENTED as
// operator-tunable while nothing populated them — the scheduler always fell through
// to its hardcoded defaults, so an operator setting the knob saw no effect and the
// doc was simply wrong. This asserts the env→Config path exists, and that leaving
// them unset stays 0 (which the scheduler reads as "use my documented defaults",
// NOT as unbounded).
func TestConsolidationCaps_AreOperatorTunable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("agents: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOOMCYCLE_MAX_CONSOLIDATION_TARGETS", "9")
	t.Setenv("LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY", "2")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Env.MaxConsolidationTargets != 9 {
		t.Errorf("MaxConsolidationTargets = %d, want 9 — the env var is not wired", cfg.Env.MaxConsolidationTargets)
	}
	if cfg.Env.MaxConsolidationConcurrency != 2 {
		t.Errorf("MaxConsolidationConcurrency = %d, want 2 — the env var is not wired", cfg.Env.MaxConsolidationConcurrency)
	}

	// Unset → 0, the sentinel the scheduler turns into its own defaults.
	t.Setenv("LOOMCYCLE_MAX_CONSOLIDATION_TARGETS", "")
	t.Setenv("LOOMCYCLE_MAX_CONSOLIDATION_CONCURRENCY", "")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load (unset): %v", err)
	}
	if cfg.Env.MaxConsolidationTargets != 0 || cfg.Env.MaxConsolidationConcurrency != 0 {
		t.Errorf("unset caps = (%d, %d), want (0, 0) so the scheduler applies its documented defaults",
			cfg.Env.MaxConsolidationTargets, cfg.Env.MaxConsolidationConcurrency)
	}
}

// memAgent builds a memory-capable agent def for the orphan-add advisory tests.
func memAgent(scopes []string, consolidation bool) AgentDef {
	return AgentDef{
		Tools:               []string{"Memory", "Read"},
		MemoryScopes:        scopes,
		MemoryConsolidation: consolidation,
	}
}

// TestOrphanAddWarning_WarnsWhenNoConsolidatorDrainsTheScope is the advisory's
// reason to exist. `add` ENQUEUES — it returns "pending" and hands the turns to
// a background pass. With no pass configured the queue grows and nothing becomes
// durable memory, so the agent looks like it is remembering while a later
// `recall` finds nothing. Silent and slow, which is exactly why it warrants a
// boot-time advisory.
//
// Fails-before without orphanAddWarnings: no warning is emitted at all.
func TestOrphanAddWarning_WarnsWhenNoConsolidatorDrainsTheScope(t *testing.T) {
	got := orphanAddWarnings(
		map[string]AgentDef{"chat": memAgent([]string{"user"}, false)},
		nil,
	)
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want exactly 1 for an undrained user scope", got)
	}
	for _, want := range []string{`memory scope "user"`, "chat", "no enabled scheduled run drains it", "memory_consolidation: true"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q is missing %q — the advisory has to name the scope, the culprits, and the fix", got[0], want)
		}
	}
}

// TestOrphanAddWarning_SilentWhenAConsolidatorCoversTheScope: the whole point of
// the advisory is that it goes away once the operator wires a consolidator. A
// warning that stays put after the fix trains operators to ignore warnings.
func TestOrphanAddWarning_SilentWhenAConsolidatorCoversTheScope(t *testing.T) {
	agents := map[string]AgentDef{
		"chat":                memAgent([]string{"user"}, false),
		"memory/consolidator": memAgent([]string{"user"}, true),
	}
	schedules := map[string]ScheduledRun{
		"memory-consolidation": {Agent: "memory/consolidator", Enabled: true, Schedule: "0 * * * *"},
	}
	if got := orphanAddWarnings(agents, schedules); len(got) != 0 {
		t.Errorf("warnings = %v, want none — a scheduled consolidator covers the user scope", got)
	}
}

// TestOrphanAddWarning_ADisabledScheduleGetsTheSofterAdvisory: a schedule with
// enabled:false is skipped by the sweeper entirely, so it is NOT coverage — the
// queue still grows and the operator still needs to hear about it. But the advice
// is different, and this is the common case (a consolidation bundle ships staged
// off because a pass is real spend): telling that operator to "add a scheduled
// run" is wrong and reads as the advisory not noticing what they installed. Name
// the schedule instead, so the fix is one flag flip.
func TestOrphanAddWarning_ADisabledScheduleGetsTheSofterAdvisory(t *testing.T) {
	agents := map[string]AgentDef{
		"chat":                memAgent([]string{"user"}, false),
		"memory/consolidator": memAgent([]string{"user"}, true),
	}
	schedules := map[string]ScheduledRun{
		"memory-consolidation": {Agent: "memory/consolidator", Enabled: false, Schedule: "0 * * * *"},
	}
	got := orphanAddWarnings(agents, schedules)
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want 1 — a disabled schedule drains nothing", got)
	}
	for _, want := range []string{`memory scope "user"`, "is disabled", "memory-consolidation"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("advisory %q is missing %q — it has to name the schedule to enable", got[0], want)
		}
	}
	if strings.Contains(got[0], "no enabled scheduled run drains it") {
		t.Errorf("advisory %q still tells the operator to add a consolidator they already have", got[0])
	}
}

// TestOrphanAddWarning_CoverageIsPerScope: a consolidator scheduled for the user
// scope does not drain the agent scope. Treating any consolidator as blanket
// coverage would hide a genuinely orphaned scope.
func TestOrphanAddWarning_CoverageIsPerScope(t *testing.T) {
	agents := map[string]AgentDef{
		"chat":                memAgent([]string{"user", "agent"}, false),
		"memory/consolidator": memAgent([]string{"user"}, true), // user only
	}
	schedules := map[string]ScheduledRun{
		"memory-consolidation": {Agent: "memory/consolidator", Enabled: true, Schedule: "0 * * * *"},
	}
	got := orphanAddWarnings(agents, schedules)
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want exactly 1 (the agent scope is still orphaned)", got)
	}
	if !strings.Contains(got[0], `memory scope "agent"`) {
		t.Errorf("warning = %q, want it to name the uncovered agent scope", got[0])
	}
}

// TestOrphanAddWarning_AggregatesPerScope: a deployment with many memory-capable
// agents needs ONE line per orphaned scope, not one per agent — a wall of
// warnings at boot is the same as no warning. The agent names are listed inside
// the single line, sorted so the output is stable.
func TestOrphanAddWarning_AggregatesPerScope(t *testing.T) {
	agents := map[string]AgentDef{
		"zeta":  memAgent([]string{"user"}, false),
		"alpha": memAgent([]string{"user"}, false),
		"mid":   memAgent([]string{"user"}, false),
	}
	got := orphanAddWarnings(agents, nil)
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want 1 aggregated line for the shared user scope", got)
	}
	if !strings.Contains(got[0], "3 agent(s)") {
		t.Errorf("warning = %q, want the agent count", got[0])
	}
	if !strings.Contains(got[0], "alpha, mid, zeta") {
		t.Errorf("warning = %q, want the agent names in sorted order (stable output)", got[0])
	}
}

// TestOrphanAddWarning_SilentWithoutAMemoryCapableAgent: no Memory tool, or the
// tool with no scopes (already default-denied and warned about separately),
// means nothing can enqueue — so there is nothing to advise about.
func TestOrphanAddWarning_SilentWithoutAMemoryCapableAgent(t *testing.T) {
	cases := map[string]AgentDef{
		"no memory tool":       {Tools: []string{"Read"}, MemoryScopes: []string{"user"}},
		"memory but no scopes": {Tools: []string{"Memory"}},
	}
	for name, agent := range cases {
		if got := orphanAddWarnings(map[string]AgentDef{"a": agent}, nil); len(got) != 0 {
			t.Errorf("%s: warnings = %v, want none", name, got)
		}
	}
}

// TestOrphanAddWarning_ConsolidatorIsNotItsOwnOrphan: the consolidator holds the
// Memory tool and scopes too, so a naive check would report it as an agent that
// can enqueue into an undrained scope — warning about the very thing that does
// the draining.
func TestOrphanAddWarning_ConsolidatorIsNotItsOwnOrphan(t *testing.T) {
	agents := map[string]AgentDef{"memory/consolidator": memAgent([]string{"user"}, true)}
	if got := orphanAddWarnings(agents, nil); len(got) != 0 {
		t.Errorf("warnings = %v, want none — the consolidator must not be flagged as its own orphan", got)
	}
}

// TestOrphanAddWarning_SubstrateOnlyScheduleAgentIsNotCoverage: a schedule may
// name an agent that exists only as a runtime substrate def, invisible at config
// load. Config cannot confirm it holds the consolidation grant, so it must not
// be counted as coverage — a false "you're covered" is worse than a warning the
// operator can dismiss.
func TestOrphanAddWarning_SubstrateOnlyScheduleAgentIsNotCoverage(t *testing.T) {
	agents := map[string]AgentDef{"chat": memAgent([]string{"user"}, false)}
	schedules := map[string]ScheduledRun{
		"runtime-consolidation": {Agent: "not-in-yaml", Enabled: true, Schedule: "0 * * * *"},
	}
	if got := orphanAddWarnings(agents, schedules); len(got) != 1 {
		t.Errorf("warnings = %v, want 1 — an unresolvable schedule agent cannot be proven to drain anything", got)
	}
}

// TestOrphanAddWarning_ScheduleWithoutTheGrantIsNotCoverage: a scheduled run for
// an agent that lacks memory_consolidation cannot drain the queue — every
// consolidation op default-denies for it.
func TestOrphanAddWarning_ScheduleWithoutTheGrantIsNotCoverage(t *testing.T) {
	agents := map[string]AgentDef{
		"chat":     memAgent([]string{"user"}, false),
		"reporter": memAgent([]string{"user"}, false), // no grant
	}
	schedules := map[string]ScheduledRun{
		"nightly": {Agent: "reporter", Enabled: true, Schedule: "0 3 * * *"},
	}
	if got := orphanAddWarnings(agents, schedules); len(got) != 1 {
		t.Errorf("warnings = %v, want 1 — an agent without the consolidation grant drains nothing", got)
	}
}
