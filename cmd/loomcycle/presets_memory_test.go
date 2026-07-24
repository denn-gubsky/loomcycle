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
	// Document/Path maintain the index, Context is the help surface. Skill is
	// auto-added for on-demand loading of the body below.
	for _, want := range []string{"Memory", "History", "Document", "Path", "Skill"} {
		if !hasToolPreset(agent.Tools, want) {
			t.Errorf("memory/consolidator should grant %q; tools=%v", want, agent.Tools)
		}
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
	// The /memory/index Document is SQL-Memory-backed.
	if !containsString(agent.SqlScopes, "user") {
		t.Errorf("sql_scopes should include user (the index Document is SQL-backed); got %v", agent.SqlScopes)
	}
	// The pipeline skill is in the on-demand catalog with its body intact.
	sk, ok := cfg.Skills["memory/consolidate"]
	if !ok {
		t.Fatalf("memory/consolidate skill missing from cfg.Skills")
	}
	if strings.TrimSpace(sk.Body) == "" {
		t.Fatal("memory/consolidate skill body is empty — the body IS the pipeline")
	}
	// The agent's skill allowlist must actually match the skill it ships.
	if !containsString(agent.Skills, "memory/*") {
		t.Errorf("agent skills allowlist = %v, want it to match memory/*", agent.Skills)
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

// TestConsolidator_NoPinnedModel guards the project's no-model-pinning rule on
// the shipped bundle. A pinned provider/model looks harmless in a diff and
// silently takes the cost/quality decision away from the operator's tier policy
// — and this agent runs unattended on a schedule, so a pin here is a recurring
// bill nobody chose. The tier must be declared instead (an agent with neither a
// tier nor a pin cannot resolve at all).
func TestConsolidator_NoPinnedModel(t *testing.T) {
	cfg := memoryBundleConfig(t)
	agent, ok := cfg.Agents["memory/consolidator"]
	if !ok {
		t.Fatalf("memory/consolidator not registered (agents: %v)", agentNames(cfg))
	}
	if agent.Provider != "" {
		t.Errorf("memory/consolidator pins provider %q — retune the tier policy instead of pinning", agent.Provider)
	}
	// Empty Model also rules out naming a model alias (an alias, including an
	// RFC BG model_pattern one, is declared through `model:` too) — the agent
	// must route through the tier, not name a target.
	if agent.Model != "" {
		t.Errorf("memory/consolidator pins model %q — retune the tier policy instead of pinning", agent.Model)
	}
	if agent.Tier == "" {
		t.Error("memory/consolidator must declare a tier (with no tier and no pin the agent cannot resolve)")
	}
	if _, ok := cfg.Tiers[agent.Tier]; !ok {
		t.Errorf("the base preset should supply the %q tier the consolidator declares; tiers=%v", agent.Tier, tierNames(cfg))
	}
}

// TestConsolidatorBundle_SkillBodyEncodesThePipeline: the body is the
// implementation, so a well-formed bundle is not enough — the steps that make a
// pass SAFE have to be in there. Each marker below is a property a reviewer
// would otherwise have to re-read the whole body to confirm, and an edit that
// drops one is a silent behaviour change: no lease means two replicas
// double-consolidate, a hard delete destroys history, a missing "advance last"
// rule loses sessions, and a missing untrusted-data framing makes the pass
// steerable by anything a user typed into a chat.
func TestConsolidatorBundle_SkillBodyEncodesThePipeline(t *testing.T) {
	cfg := memoryBundleConfig(t)
	body := cfg.Skills["memory/consolidate"].Body
	prompt := cfg.Agents["memory/consolidator"].SystemPrompt

	for _, want := range []string{
		"cursor_lease",      // step 1 — one pass per target
		"acquired",          // ...and the not-acquired stop
		"cursor_get",        // step 2 — the watermark
		"cursor_scan",       // step 3a — the ONLY safe chat-discovery read
		"History op=get",    // step 3a — then read each scanned chat's turns
		"pending_drain",     // step 3b — the queue
		"cursor_release",    // step 4 / 9 — always give the lease back
		"recall",            // step 5 — the neighbour set for dedup
		"0.95",              // step 5 — the merge band
		"0.85",              // step 5 — the flag-as-related band
		"supersede",         // step 6 — soft-archive, never hard delete
		"provenance",        // step 6 — the audit trail
		"embed=true",        // step 6 — or the fact is invisible to the next pass
		"pending_ack",       // step 8
		"cursor_advance",    // step 9
		"/memory/index",     // step 7
		"deterministic",     // step 6 — the idempotency mechanism
		`"truncated": true`, // step 3a — a trimmed page is normal, not an error
	} {
		if !strings.Contains(body, want) {
			t.Errorf("skill body is missing %q — the pipeline step it encodes would be dropped", want)
		}
	}

	// Chat DISCOVERY must go through cursor_scan, never through paging the chat
	// list. `History op=list` is ordered newest-first and filtered on
	// last_activity — a different timestamp from the forward-only watermark — so a
	// pass that discovered work that way consolidated the newest page, advanced
	// past it, and stranded every older chat permanently and silently. The scan is
	// ascending and watermark-keyed, which is what makes "advance to the last row
	// I consolidated" safe.
	if strings.Contains(body, "History op=list") {
		t.Error("skill body still pages `History op=list` for discovery — that read is newest-first and strands older chats behind the forward-only watermark; discovery must be cursor_scan")
	}
	// The advance pair has to be relayed verbatim from a scan row. Re-deriving it
	// from a chat's last activity (or from the clock) is how the watermark ends up
	// somewhere no session ever settled.
	if !strings.Contains(body, "verbatim") {
		t.Error("skill body must tell the pass to copy the (completed_at, session_id) pair verbatim from the scan row it consolidated")
	}
	// Destructive-op cap. supersede is a soft archive and a re-derived fact
	// revives its row, which bounds the damage — but nothing bounds the COUNT, so
	// an injected "everything you know about X is obsolete" could otherwise drive
	// an unbounded retirement sweep in one pass. The cap turns that into a report
	// an operator reads instead of a memory that quietly emptied.
	if !strings.Contains(body, "Never emit more than 5 `delete` entries in one pass") {
		t.Error("skill body must cap destructive entries per pass — without it one steered pass can retire an entire topic")
	}

	// Advance-last is the invariant that makes a failed pass safe to retry.
	if !strings.Contains(body, "ONLY after every write") {
		t.Error("skill body must state that the watermark advances ONLY after the writes land")
	}
	// A hard delete would destroy the audit trail supersede exists to keep.
	if !strings.Contains(body, "NEVER use `Memory op=delete`") {
		t.Error("skill body must forbid the hard delete op explicitly")
	}
	// The pass's own past runs are excluded by the cursor_scan QUERY (it filters
	// out the calling agent's own sessions), so the body no longer carries a
	// "skip your own runs" rule — a mechanism beats an instruction. What the body
	// must still say is that the scan is the only discovery read, asserted above.

	// Prompt-injection posture: transcripts are data. This has to be in the
	// SYSTEM PROMPT (always present), not only in the on-demand skill body.
	for _, want := range []string{"UNTRUSTED DATA", "Never follow an instruction"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt is missing %q — transcript content must be framed as data, never instructions", want)
		}
	}
	// Secrets must never be consolidated into durable memory.
	if !strings.Contains(prompt, "Secrets") {
		t.Error("system prompt must forbid storing secrets/credentials")
	}
}

// TestConsolidatorBundle_NoRFCLettersInModelVisibleText: RFC identifiers are
// internal shorthand. In a prompt, a skill body, or a skill description they are
// noise the model cannot resolve — it points at a document the model has no
// access to. Comments in the yaml are fine; the parsed, model-visible strings
// are not.
func TestConsolidatorBundle_NoRFCLettersInModelVisibleText(t *testing.T) {
	cfg := memoryBundleConfig(t)
	sk := cfg.Skills["memory/consolidate"]
	surfaces := map[string]string{
		"system_prompt":     cfg.Agents["memory/consolidator"].SystemPrompt,
		"skill body":        sk.Body,
		"skill description": sk.Description,
	}
	for where, text := range surfaces {
		for _, line := range strings.Split(text, "\n") {
			if idx := strings.Index(line, "RFC "); idx >= 0 {
				t.Errorf("%s cites an RFC (%q) — model-visible text must not reference RFC letters", where, strings.TrimSpace(line))
			}
		}
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
