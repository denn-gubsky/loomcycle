package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memoryBundleConfig loads base + the memory bundle, the way an operator selects
// it (LOOMCYCLE_PRESETS=base,memory). LoadLayers runs validate(), so a bad ACL
// grant or a malformed cron fails right here.
func memoryBundleConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "") // inline skills only
	cfg, err := config.LoadLayers(layersFor(t, "base", "memory")...)
	if err != nil {
		t.Fatalf("base + memory must load + validate cleanly: %v", err)
	}
	return cfg
}

// TestConsolidatorBundle_Validates: the memory bundle loads on top of base and
// registers the consolidator agent with the grants its pipeline actually needs.
// Each grant here is a silent-failure trap if missing — the agent boots either
// way and the tool just refuses every call, which reads as "the model chose not
// to use it". Also guards the bundle's `scheduled_runs:` block, the first one any
// bundle ships: an undeclared agent name or a bad cron is fatal at validate().
func TestConsolidatorBundle_Validates(t *testing.T) {
	cfg := memoryBundleConfig(t)

	agent, ok := cfg.Agents["memory/consolidator"]
	if !ok {
		t.Fatalf("memory/consolidator not registered (agents: %v)", agentNames(cfg))
	}

	// The tool ceiling: Memory drives the pipeline, History reads the chats,
	// Agent spawns the one model call, Context reports the calibrated bands.
	for _, want := range []string{"Memory", "History", "Agent", "Context"} {
		if !hasToolPreset(agent.Tools, want) {
			t.Errorf("memory/consolidator should grant %q; tools=%v", want, agent.Tools)
		}
	}
	// Document / Skill / Path must NOT be granted. Nothing in the code body calls
	// any of them, and an unused grant is not free: it is the capability an
	// injected instruction reaches for. `Path op=rm recursive=true` in particular
	// is a HARD delete sitting inside an agent whose one central safety rule is
	// that consolidation never destroys history — it would bypass the soft
	// archive (supersede) the whole pipeline is built around.
	for _, denied := range []string{"Document", "Skill", "Path"} {
		if hasToolPreset(agent.Tools, denied) {
			t.Errorf("memory/consolidator grants %q but the code body never calls it; tools=%v", denied, agent.Tools)
		}
	}
	// ...and for Skill, `skills: [-*]` is the ONLY thing that keeps it off: the
	// runtime auto-adds Skill to every agent that does not deny all skills, so
	// merely dropping the allowlist would leave the tool in place (an inert fix).
	if !containsString(agent.Skills, "-*") {
		t.Errorf("memory/consolidator must declare skills: [-*] to deny the auto-added Skill tool; skills=%v", agent.Skills)
	}
	// The consolidation control ops (cursor/supersede/pending queue) are gated by
	// a grant SEPARATE from memory_scopes — without it every one default-denies.
	if !agent.MemoryConsolidation {
		t.Error("memory/consolidator must set memory_consolidation: true, or every cursor/supersede/pending op default-denies")
	}
	// user = the fan-out target's scope; agent = the consolidator's own bookkeeping.
	for _, want := range []string{"agent", "user"} {
		if !containsString(agent.MemoryScopes, want) {
			t.Errorf("memory_scopes should include %q; got %v", want, agent.MemoryScopes)
		}
	}
	// history_scope is default-deny when empty: the pass could not read a single chat.
	if !containsString(agent.HistoryScope, "user") {
		t.Errorf("history_scope should include user (the narrowest scope that reads the target's chats); got %v", agent.HistoryScope)
	}
	// sql_scopes is now GONE. It was carried "pending verification" through the
	// prompt rewrite because revoking a capability is wider than editing text —
	// but the pipeline is code now, and the code demonstrably never issues a SQL
	// op. There is nothing left to verify.
	if len(agent.SqlScopes) != 0 {
		t.Errorf("memory/consolidator still grants sql_scopes=%v — the code body issues no SQL op, and an unused grant is the capability an injection reaches for", agent.SqlScopes)
	}
	// The procedure lives in the code body, so neither the old skill nor the old
	// system prompt may survive as a second, drifting copy that nothing runs.
	// That duplication is exactly what produced the orphaned memory/consolidate
	// skill in the first place.
	if _, ok := cfg.Skills["memory/consolidate"]; ok {
		t.Error("the memory/consolidate skill still ships — an unloaded duplicate of a procedure that now lives in code")
	}
	if strings.TrimSpace(agent.SystemPrompt) != "" {
		t.Errorf("memory/consolidator still carries a system_prompt — the code body IS the pipeline and a code-js agent never reads one, so this can only rot:\n%s", agent.SystemPrompt)
	}
	if strings.TrimSpace(agent.Code) == "" {
		t.Fatal("memory/consolidator has no code body — the body IS the pipeline")
	}

	// The example schedule: present, pointing at the declared agent, and carrying
	// the fan-out marker that makes it dispatch per target.
	sr, ok := cfg.ScheduledRuns["memory-consolidation"]
	if !ok {
		t.Fatalf("the memory bundle should declare the memory-consolidation schedule; got %v", scheduleNames(cfg))
	}
	if sr.Agent != "memory/consolidator" {
		t.Errorf("schedule agent = %q, want memory/consolidator", sr.Agent)
	}
	// STAGED OFF. A pass is a real LLM run and the schedule fans out one per user
	// with unconsolidated chats, so shipping it enabled means selecting the bundle
	// (plus LOOMCYCLE_SCHEDULER_ENABLED=1) starts hourly spend across up to 32
	// targets immediately — with no operator decision in between. The bundle's own
	// header says to stage it off; this is what keeps the artifact honest.
	if sr.Enabled {
		t.Error("the memory-consolidation schedule must ship enabled: false — selecting the bundle must not start unattended LLM spend")
	}
	if sr.Schedule == "" {
		t.Error("schedule must carry a cron expression (a standalone entry, not a template)")
	}
	if len(sr.Prompt) == 0 {
		t.Error("schedule must carry a prompt")
	}
	if sr.Metadata["memory_consolidation_fanout"] != true {
		t.Errorf("schedule metadata must set memory_consolidation_fanout: true, or it fires one targetless run; got %v", sr.Metadata)
	}
	if sr.Metadata["memory_consolidation_scope"] != string(store.MemoryScopeUser) {
		t.Errorf("schedule metadata memory_consolidation_scope = %v, want %q", sr.Metadata["memory_consolidation_scope"], store.MemoryScopeUser)
	}
}

// TestConsolidator_NamesNoModel guards the project's no-model-pinning rule on
// the shipped bundle. `provider: code-js` is NOT a pin in that sense — it is a
// synthetic in-process provider that never calls a model and costs nothing, so
// naming it takes no cost/quality decision away from anyone. What must stay
// absent is a MODEL: an alias or a concrete id (both are declared through
// `model:`) would route the pass at a target the operator's tier policy did not
// choose, on an agent that runs unattended on a schedule.
//
// The actual model decision lives on memory/extractor, and its no-pin rule is
// asserted in TestExtractor_HasNoToolsAtAll alongside its tier.
func TestConsolidator_NamesNoModel(t *testing.T) {
	cfg := memoryBundleConfig(t)
	agent, ok := cfg.Agents["memory/consolidator"]
	if !ok {
		t.Fatalf("memory/consolidator not registered (agents: %v)", agentNames(cfg))
	}
	if agent.Provider != "code-js" {
		t.Errorf("memory/consolidator provider = %q, want code-js — the orchestration is deterministic, not a model call", agent.Provider)
	}
	if agent.Model != "" {
		t.Errorf("memory/consolidator names model %q — a code agent resolves no model at all, so this can only confuse", agent.Model)
	}
	// A tier is meaningless on a code agent (nothing resolves through it) and
	// misleads a reader into thinking the pass costs tier-priced tokens.
	if agent.Tier != "" {
		t.Errorf("memory/consolidator declares tier %q — a code agent never resolves a model, so the tier is dead config", agent.Tier)
	}
}

// TestConsolidatorBundle_SourceMatchesEmbedded: the bundle exists twice — the
// readable source tree at bundles/memory/loomcycle.yaml and the flat file the
// binary go:embeds. Only the embedded copy ships, so editing the source alone
// changes nothing at runtime while looking, in a diff, like it changed
// everything. Scoped to this bundle deliberately: a repo-wide parity guard is a
// separate change (one existing bundle has pre-existing drift).
func TestConsolidatorBundle_SourceMatchesEmbedded(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "bundles", "memory", "loomcycle.yaml"))
	if err != nil {
		t.Fatalf("read bundle source: %v", err)
	}
	embeddedCopy, err := os.ReadFile(filepath.Join("embedded", "bundles", "memory.yaml"))
	if err != nil {
		t.Fatalf("read embedded bundle: %v", err)
	}
	if !bytes.Equal(src, embeddedCopy) {
		t.Error("bundles/memory/loomcycle.yaml and cmd/loomcycle/embedded/bundles/memory.yaml differ — only the embedded copy ships, so copy the source over it")
	}
}

// TestConsolidatorBundle_ComposesWithTheDefaultStack: the memory bundle is
// opt-in, but an operator running the shipped default stack will select it
// ALONGSIDE those bundles. A grant, channel, or schedule name that collides only
// in that combination is fatal at boot, and validating the bundle in isolation
// would never surface it — so validate the whole stack with memory layered on.
func TestConsolidatorBundle_ComposesWithTheDefaultStack(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "document-agent", "chat", "agent-teams", "team-examples", "memory")...)
	if err != nil {
		t.Fatalf("the default preset stack plus memory must load + validate cleanly: %v", err)
	}
	if _, ok := cfg.Agents["memory/consolidator"]; !ok {
		t.Errorf("memory/consolidator missing from the full stack; agents=%v", agentNames(cfg))
	}
	// The other bundles' agents survive the extra layer.
	for _, name := range []string{"doc/manager", "chat/medium", "team/orchestrator"} {
		if _, ok := cfg.Agents[name]; !ok {
			t.Errorf("%s went missing once memory was layered on", name)
		}
	}
}

// TestConsolidatorBundle_SoftensTheOrphanAddWarning closes the loop between the
// advisory and the bundle that answers it.
//
// The default stack ships agents that can enqueue with Memory op=add and nothing
// that drains the queue, so it earns the full "nothing drains this scope" line.
// Selecting the memory bundle must CHANGE that advice rather than silence it: the
// bundle's schedule ships staged off (a pass is real spend), so the operator does
// have a consolidator — they have one flag left to flip. Telling them to "add a
// scheduled run" at that point is wrong and reads as the advisory not noticing
// what they just installed; telling them nothing hides a queue that is still
// growing. Naming the schedule is the useful third answer.
func TestConsolidatorBundle_SoftensTheOrphanAddWarning(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	orphanWarnings := func(cfg *config.Config) []string {
		var out []string
		for _, w := range cfg.Warnings {
			if strings.Contains(w, "can enqueue with Memory op=add") {
				out = append(out, w)
			}
		}
		return out
	}

	defaultStack := []string{"base", "document-agent", "chat", "agent-teams", "team-examples"}
	without, err := config.LoadLayers(layersFor(t, defaultStack...)...)
	if err != nil {
		t.Fatalf("default stack: %v", err)
	}
	got := orphanWarnings(without)
	if len(got) != 1 {
		t.Fatalf("default stack orphan-add warnings = %d, want 1 aggregated line: %v", len(got), got)
	}
	if !strings.Contains(got[0], "no enabled scheduled run drains it") {
		t.Errorf("without the bundle the advisory should be the full one; got %q", got[0])
	}

	with, err := config.LoadLayers(layersFor(t, append(defaultStack, "memory")...)...)
	if err != nil {
		t.Fatalf("default stack + memory: %v", err)
	}
	got = orphanWarnings(with)
	if len(got) != 1 {
		t.Fatalf("with the memory bundle staged off, orphan-add warnings = %d, want 1 softer line: %v", len(got), got)
	}
	if strings.Contains(got[0], "no enabled scheduled run drains it") {
		t.Errorf("the advisory still says nothing drains the scope, but the bundle's consolidator IS installed (just disabled); got %q", got[0])
	}
	for _, want := range []string{"is disabled", "memory-consolidation"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the softer advisory must name the disabled schedule; %q is missing %q", got[0], want)
		}
	}

	// Flipping the schedule on is what actually silences it — the advisory has to
	// have an end state, or operators learn to ignore it.
	enabled, err := config.LoadLayers(append(layersFor(t, append(defaultStack, "memory")...), config.Layer{
		Name: "enable-consolidator",
		Data: []byte("scheduled_runs:\n  memory-consolidation:\n    enabled: true\n"),
	})...)
	if err != nil {
		t.Fatalf("default stack + memory + enable overlay: %v", err)
	}
	if got := orphanWarnings(enabled); len(got) != 0 {
		t.Errorf("with the consolidator ENABLED the advisory must go away entirely, still got: %v", got)
	}
}

// containsString reports whether needle is in haystack.
func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// scheduleNames lists the declared schedule names, for failure messages.
func scheduleNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.ScheduledRuns))
	for n := range cfg.ScheduledRuns {
		out = append(out, n)
	}
	return out
}

// tierNames lists the declared tier names, for failure messages.
func tierNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Tiers))
	for n := range cfg.Tiers {
		out = append(out, n)
	}
	return out
}
