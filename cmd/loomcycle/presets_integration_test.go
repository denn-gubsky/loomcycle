package main

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// layersFor resolves embedded unit names to config layers, the same mapping
// main() does for LOOMCYCLE_PRESETS (kept in lockstep with selectPresetLayers).
func layersFor(t *testing.T, names ...string) []config.Layer {
	t.Helper()
	units, err := embedded.ResolveUnits(names)
	if err != nil {
		t.Fatalf("ResolveUnits(%v): %v", names, err)
	}
	layers := make([]config.Layer, len(units))
	for i, u := range units {
		layers[i] = config.Layer{Name: u.Name, Data: u.Data}
	}
	return layers
}

// TestEmbedded_DocumentAgentResolvesWithInlineSkills is the RFC AQ §7 Phase-1
// headline, updated for RFC BA on-demand skills: selecting `base,document-agent`
// registers doc/manager AND carries its four inline skills in cfg.Skills (the
// on-demand catalog) with NO LOOMCYCLE_SKILLS_ROOT — the bundle is a pure config
// layer. The bodies are loaded via the Skill tool at runtime, NOT baked into the
// prompt; the agent gets the auto-added Skill tool.
func TestEmbedded_DocumentAgentResolvesWithInlineSkills(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "") // no skills directory — inline only

	cfg, err := config.LoadLayers(layersFor(t, "base", "document-agent")...)
	if err != nil {
		t.Fatalf("LoadLayers(base, document-agent): %v", err)
	}
	dm, ok := cfg.Agents["doc/manager"]
	if !ok {
		t.Fatalf("doc/manager not registered (agents: %v)", agentNames(cfg))
	}
	// All four skills are registered in the on-demand catalog (cfg.Skills),
	// with their bodies intact for the Skill tool to load.
	for _, name := range []string{"doc/semantic-chunking", "doc/edge-linking", "doc/restructuring", "doc/md-import"} {
		sk, ok := cfg.Skills[name]
		if !ok {
			t.Errorf("inline skill %q missing from cfg.Skills", name)
			continue
		}
		if strings.TrimSpace(sk.Body) == "" {
			t.Errorf("inline skill %q has an empty body", name)
		}
	}
	// On-demand: the skill bodies are NOT baked into the agent prompt.
	for _, marker := range []string{"Semantic chunking", "Edge linking", "Markdown import"} {
		if strings.Contains(dm.SystemPrompt, marker) {
			t.Errorf("skill body %q must not be baked into the prompt (RFC BA on-demand)", marker)
		}
	}
	// The agent's whitelist gets the auto-added Skill tool for on-demand loading.
	if !hasToolPreset(dm.Tools, "Skill") {
		t.Errorf("doc/manager should get the auto-added Skill tool; tools=%v", dm.Tools)
	}
	// base supplied the middle tier the agent declares.
	if _, ok := cfg.Tiers["middle"]; !ok {
		t.Errorf("base preset should supply the middle tier doc/manager needs")
	}
}

// TestEmbedded_OperatorOverridesBundleSkill: an operator's later inline skill of
// the same name wins (RFC AN merge-by-key) in the on-demand catalog cfg.Skills,
// so the override is just re-declaring the key — no skills root, no fork.
func TestEmbedded_OperatorOverridesBundleSkill(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")

	overlay := config.Layer{Name: "operator", Data: []byte(`
skills:
  doc/restructuring:
    tools: [Document]
    body: |
      OVERRIDDEN RESTRUCTURING BODY
`)}
	layers := append(layersFor(t, "base", "document-agent"), overlay)
	cfg, err := config.LoadLayers(layers...)
	if err != nil {
		t.Fatalf("LoadLayers with override: %v", err)
	}
	sk, ok := cfg.Skills["doc/restructuring"]
	if !ok {
		t.Fatalf("restructuring skill missing from cfg.Skills")
	}
	if !strings.Contains(sk.Body, "OVERRIDDEN RESTRUCTURING BODY") {
		t.Errorf("operator override of the restructuring skill did not win; body=%q", sk.Body)
	}
	if strings.Contains(sk.Body, "deliberately has no drag-edit") {
		t.Errorf("the original restructuring body should be gone after override; body=%q", sk.Body)
	}
}

// TestEmbedded_DefaultStackValidates is the v1.20.1 regression: the full default
// preset stack the TrueNAS/Docker deploy ships (base + the four bundles) must
// LOAD AND VALIDATE cleanly. v1.20.0 shipped a fatal boot error here —
// team/orchestrator granted `channels: [team/*]` but no team/* channel was
// declared, so validate() rejected the whole config ("no declared channel
// matches the prefix") and the server crash-looped on boot. LoadLayers runs
// validate(), so this fails on the unfixed bundles.
func TestEmbedded_DefaultStackValidates(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "document-agent", "chat", "agent-teams", "team-examples")...)
	if err != nil {
		t.Fatalf("the default preset stack must load + validate cleanly: %v", err)
	}
	// The orchestrator's team/* channel ACL now resolves to a declared channel.
	if _, ok := cfg.Channels["team/coordination"]; !ok {
		names := make([]string, 0, len(cfg.Channels))
		for n := range cfg.Channels {
			names = append(names, n)
		}
		t.Errorf("agent-teams bundle should declare the team/coordination channel; channels=%v", names)
	}
	if _, ok := cfg.Agents["team/orchestrator"]; !ok {
		t.Errorf("team/orchestrator should be registered")
	}
}

// TestEmbedded_SandboxBundleValidates: the opt-in sandbox bundle loads + validates
// on top of base. Post-refactor it carries NO agent (the LLM dev/sandbox was retired
// for the deterministic code-js dev/exec, shipped in the dev-exec bundle) — it ships
// the sandbox MCP wiring + the caller-facing dev/sandbox-usage delegation skill, so
// it stays flag-independent (usable without LOOMCYCLE_CODE_AGENTS_ENABLED). An ACL
// grant without a matching declaration would fail validate() fatally, so this guards
// the bundle in isolation.
func TestEmbedded_SandboxBundleValidates(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "sandbox")...)
	if err != nil {
		t.Fatalf("base + sandbox must load + validate cleanly: %v", err)
	}
	// The LLM dev/sandbox agent + its tool-driving skill are GONE (replaced by the
	// code-js dev/exec in the dev-exec bundle).
	if _, ok := cfg.Agents["dev/sandbox"]; ok {
		t.Errorf("dev/sandbox agent should be removed from the sandbox bundle")
	}
	if _, ok := cfg.Skills["dev/sandbox"]; ok {
		t.Errorf("dev/sandbox tool-driving skill should be removed")
	}
	// The caller-facing delegation skill remains (rewritten to target dev/exec).
	if sk, ok := cfg.Skills["dev/sandbox-usage"]; !ok || strings.TrimSpace(sk.Body) == "" {
		t.Errorf("dev/sandbox-usage skill missing or empty in cfg.Skills")
	}
	// The MCP server the tools resolve through is declared (http transport + url).
	sv, ok := cfg.MCPServers["sandbox"]
	if !ok {
		t.Fatalf("sandbox MCP server not declared")
	}
	if sv.Transport != "http" || sv.URL == "" {
		t.Errorf("sandbox MCP server should be http with a url; got transport=%q url=%q", sv.Transport, sv.URL)
	}
	// The git-token injection header rides mcp_servers.sandbox (resolved server-side).
	if got := sv.Headers["X-Loom-Sandbox-Env-Gh-Token"]; got != "$cred:sandbox_github" {
		t.Errorf("sandbox git-token header = %q, want $cred:sandbox_github", got)
	}
	// One shared secret under the sidecar's OWN name: the Authorization header
	// expands SANDBOX_AUTH_TOKEN from the env (allowlisted), no ${run.user_bearer}.
	t.Setenv("SANDBOX_AUTH_TOKEN", "sekret-xyz")
	cfg2, err := config.LoadLayers(layersFor(t, "base", "sandbox")...)
	if err != nil {
		t.Fatalf("reload with SANDBOX_AUTH_TOKEN set: %v", err)
	}
	if got := cfg2.MCPServers["sandbox"].Headers["Authorization"]; got != "Bearer sekret-xyz" {
		t.Errorf("sandbox Authorization = %q, want \"Bearer sekret-xyz\" (SANDBOX_AUTH_TOKEN expanded, no user_bearer)", got)
	}
}

// TestEmbedded_DevExecBundleValidates: the opt-in dev-exec bundle loads + validates
// on top of base+sandbox, registering the deterministic code-js dev/exec agent with
// the mcp__sandbox__* ceiling + an inline code body. It is NOT in the default stack
// (it needs LOOMCYCLE_CODE_AGENTS_ENABLED, boot-fatal when disabled) — but, like
// doc-colorizer, config load/validate does NOT require the flag (the gate is a
// server-boot provider-registration check), so this validates in CI unchanged.
func TestEmbedded_DevExecBundleValidates(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "sandbox", "dev-exec")...)
	if err != nil {
		t.Fatalf("base + sandbox + dev-exec must load + validate cleanly: %v", err)
	}
	agent, ok := cfg.Agents["dev/exec"]
	if !ok {
		t.Fatalf("dev/exec agent not registered (agents: %v)", agentNames(cfg))
	}
	if agent.Provider != "code-js" {
		t.Errorf("dev/exec provider = %q, want code-js", agent.Provider)
	}
	// It drives the sandbox tool family via the mcp__sandbox__* glob (no LLM tier).
	if !hasToolPreset(agent.Tools, "mcp__sandbox__*") {
		t.Errorf("dev/exec should grant mcp__sandbox__*; tools=%v", agent.Tools)
	}
	// It ships its logic inline (no filesystem agent_code/ bind) — an empty Code
	// would mean the bundle shipped nothing.
	if strings.TrimSpace(agent.Code) == "" {
		t.Errorf("dev/exec must carry an inline code body")
	}
}

// TestEmbedded_DocColorizerBundleValidates: the opt-in doc-colorizer bundle loads
// + validates on top of base, registering the doc/colorizer code-js agent with
// the [Path, Document] tool ceiling and user memory/sql scopes. It is NOT in the
// default stack (it needs LOOMCYCLE_CODE_AGENTS_ENABLED, and a code-js agent is
// boot-fatal when disabled), so it gets its own guard here — like sandbox. Note:
// config load/validate does NOT require code agents enabled (the enable gate is a
// server-boot provider-registration check), so this validates in CI unchanged.
func TestEmbedded_DocColorizerBundleValidates(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "doc-colorizer")...)
	if err != nil {
		t.Fatalf("base + doc-colorizer must load + validate cleanly: %v", err)
	}
	agent, ok := cfg.Agents["doc/colorizer"]
	if !ok {
		t.Fatalf("doc/colorizer agent not registered (agents: %v)", agentNames(cfg))
	}
	if agent.Provider != "code-js" {
		t.Errorf("doc/colorizer provider = %q, want code-js", agent.Provider)
	}
	// The [Path, Document] ceiling the JS needs — Path to walk the subtree,
	// Document to read+stamp the root chunk.
	for _, want := range []string{"Path", "Document"} {
		if !hasToolPreset(agent.Tools, want) {
			t.Errorf("doc/colorizer should grant %q; tools=%v", want, agent.Tools)
		}
	}
	// Documents live on Memory (chunk bodies) + SQL Memory (structure), both at
	// user scope — the scopes the tool dispatch checks.
	if !sliceHas(agent.MemoryScopes, "user") {
		t.Errorf("doc/colorizer should declare memory_scopes: [user]; got %v", agent.MemoryScopes)
	}
	if !sliceHas(agent.SqlScopes, "user") {
		t.Errorf("doc/colorizer should declare sql_scopes: [user]; got %v", agent.SqlScopes)
	}
	// It ships its logic inline (no filesystem agent_code/ bind) — the runtime
	// prefers the inline body; an empty Code would mean the bundle shipped nothing.
	if strings.TrimSpace(agent.Code) == "" {
		t.Errorf("doc/colorizer must carry an inline code body")
	}
}

// hasToolPreset reports whether tools contains name.
func hasToolPreset(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

// TestEmbedded_BundleAloneDegradesGracefully: document-agent WITHOUT base still
// registers doc/manager (no load error) — it's a registered-but-idle def absent a
// middle tier, per the RFC's graceful-degradation note.
func TestEmbedded_BundleAloneDegradesGracefully(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")

	cfg, err := config.LoadLayers(layersFor(t, "document-agent")...)
	if err != nil {
		t.Fatalf("LoadLayers(document-agent) alone should not error: %v", err)
	}
	if _, ok := cfg.Agents["doc/manager"]; !ok {
		t.Fatalf("doc/manager should still be registered without base")
	}
}

// TestSelectPresetNames: --preset flags override LOOMCYCLE_PRESETS; an
// unset/empty env yields no presets (the opt-in default); order is preserved.
func TestSelectPresetNames(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		env   string
		want  []string
	}{
		{"unset is opt-in none", nil, "", nil},
		{"env comma-split ordered", nil, "base, document-agent", []string{"base", "document-agent"}},
		{"env trims blanks", nil, " base , , local ", []string{"base", "local"}},
		{"flags override env", []string{"local"}, "base,document-agent", []string{"local"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectPresetNames(tc.flags, tc.env)
			if len(got) != len(tc.want) {
				t.Fatalf("selectPresetNames(%v, %q) = %v, want %v", tc.flags, tc.env, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestEmbedded_OAuthPrependsWithoutRestatement is the RFC AQ §3 headline: the
// one-provider-per-file oauth preset composes onto base via !prepend — OAuth on
// top of provider_priority AND each tier, with base's providers retained as
// fallback and NO restatement.
func TestEmbedded_OAuthPrependsWithoutRestatement(t *testing.T) {
	cfg, err := config.LoadLayers(layersFor(t, "base", "oauth")...)
	if err != nil {
		t.Fatalf("LoadLayers(base, oauth): %v", err)
	}
	if len(cfg.ProviderPriority) == 0 || cfg.ProviderPriority[0] != "anthropic-oauth-dev" {
		t.Errorf("provider_priority[0] = %v, want anthropic-oauth-dev on top", cfg.ProviderPriority)
	}
	// base's matrix is retained (fallback) — the prepend didn't restate/replace it.
	for _, want := range []string{"deepseek", "openai", "anthropic"} {
		if !sliceHas(cfg.ProviderPriority, want) {
			t.Errorf("base provider %q dropped from provider_priority: %v", want, cfg.ProviderPriority)
		}
	}
	// Each tier has the OAuth alias on top (a bare alias parses to {Model: alias}).
	if len(cfg.Tiers["middle"]) == 0 || cfg.Tiers["middle"][0].Model != "oauth-sonnet" {
		t.Errorf("tiers.middle = %+v, want oauth-sonnet first", cfg.Tiers["middle"])
	}
	if len(cfg.Tiers["high"]) == 0 || cfg.Tiers["high"][0].Model != "oauth-opus" {
		t.Errorf("tiers.high = %+v, want oauth-opus first", cfg.Tiers["high"])
	}
}

// TestEmbedded_LocalPrepends: base + local puts ollama-local + the local tier
// candidates on top while keeping base's cloud providers as fallback.
func TestEmbedded_LocalPrepends(t *testing.T) {
	cfg, err := config.LoadLayers(layersFor(t, "base", "local")...)
	if err != nil {
		t.Fatalf("LoadLayers(base, local): %v", err)
	}
	if len(cfg.ProviderPriority) == 0 || cfg.ProviderPriority[0] != "ollama-local" {
		t.Errorf("provider_priority[0] = %v, want ollama-local on top", cfg.ProviderPriority)
	}
	if !sliceHas(cfg.ProviderPriority, "anthropic") {
		t.Errorf("base cloud fallback dropped from provider_priority: %v", cfg.ProviderPriority)
	}
	if len(cfg.Tiers["low"]) == 0 || cfg.Tiers["low"][0].Model != "local-fast" {
		t.Errorf("tiers.low = %+v, want local-fast first", cfg.Tiers["low"])
	}
}

func sliceHas(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func agentNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		out = append(out, n)
	}
	return out
}

// TestEmbedded_ChatAgentsOptIntoMemoryFlush pins the RFC BL P3 bundle wiring, and
// pins the PRECONDITION with it.
//
// memory_flush is inert without `user` in memory_scopes: consolidation fans out
// over user scopes only, so a span banked anywhere else is enqueued and then never
// drained by anything. That combination — flag on, user scope missing — is a silent
// no-op at the bundle level, so the two are asserted together rather than apart.
//
// The chat agents are the case the bridge exists for: they auto-compact and park
// at end_turn instead of terminating, so consolidation's all-runs-terminal
// predicate never selects their sessions.
func TestEmbedded_ChatAgentsOptIntoMemoryFlush(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "chat")...)
	if err != nil {
		t.Fatalf("base+chat must load: %v", err)
	}

	var checked int
	for name, def := range cfg.Agents {
		if !strings.HasPrefix(name, "chat/") {
			continue
		}
		checked++
		if def.Compaction == nil || def.Compaction.Enabled == nil || !*def.Compaction.Enabled {
			// Not a bridge candidate — nothing to bank if it never compacts.
			continue
		}
		if def.Compaction.MemoryFlush == nil || !*def.Compaction.MemoryFlush {
			t.Errorf("agent %q auto-compacts but does not set compaction.memory_flush, so everything it discards is lost to memory", name)
			continue
		}
		if !slicesContains(def.MemoryScopes, "user") {
			t.Errorf("agent %q sets memory_flush but has no `user` in memory_scopes %v — consolidation drains user scopes only, so banked spans would never be consolidated",
				name, def.MemoryScopes)
		}
	}
	if checked == 0 {
		t.Fatal("no chat/* agents found in the base+chat stack; this test asserts nothing")
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}
