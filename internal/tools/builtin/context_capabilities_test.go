package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// capabilitiesCfg builds an operator config with every reportable subsystem ON
// and, deliberately, several secret- and topology-shaped values planted in the
// places a careless implementation would reach for: a SearXNG base URL, a
// Postgres DSN, a Brave API key, and an embedder wired by env-var NAME.
func capabilitiesCfg() *config.Config {
	return &config.Config{
		Env: config.Env{
			ListenAddr:                  "0.0.0.0:8787",
			BashEnabled:                 true,
			BashboxEnabled:              true,
			SchedulerEnabled:            true,
			WebhooksEnabled:             true,
			RetentionEnabled:            true,
			CodeAgentsEnabled:           true,
			BraveAPIKey:                 "brave-secret-key-value",
			MaxRequestBytes:             16 << 20,
			MaxConsolidationTargets:     32,
			MaxConsolidationConcurrency: 4,
		},
		Storage: config.StorageConfig{
			Backend: "postgres",
			PgDSN:   "postgres://loom:hunter2@db.internal:5432/loomcycle",
		},
		SearchProviders: map[string]config.SearchProviderConfig{
			"searxng": {BaseURL: "http://192.168.0.77:8080"},
			"brave":   {},
		},
		Agents: map[string]config.AgentDef{
			"memory/consolidator": {MemoryConsolidation: true, Tools: []string{"Memory"}},
		},
		ScheduledRuns: map[string]config.ScheduledRun{
			"memory-consolidation": {Agent: "memory/consolidator", Enabled: true},
		},
	}
}

func capabilitiesOut(t *testing.T, tool *Context, ctx context.Context) map[string]any {
	t.Helper()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"capabilities"}`))
	if res.IsError {
		t.Fatalf("capabilities: %s", res.Text)
	}
	return decodeResult(t, res.Text)
}

// available digs out the `available` bool of a capability block.
func available(t *testing.T, out map[string]any, key string) bool {
	t.Helper()
	blk, ok := out[key].(map[string]any)
	if !ok {
		t.Fatalf("capability %q missing/wrong type: %v", key, out[key])
	}
	b, _ := blk["available"].(bool)
	return b
}

// TestContextTool_CapabilitiesReflectsSubsystemState: each flag must track the
// REAL subsystem, not a hardcoded truth. Flipping a subsystem off must flip its
// flag — that is the whole contract, since a client branches on these instead
// of calling and reading a refusal.
func TestContextTool_CapabilitiesReflectsSubsystemState(t *testing.T) {
	on := &Context{Cfg: capabilitiesCfg()}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1"})
	out := capabilitiesOut(t, on, ctx)

	for _, key := range []string{"bash", "bashbox", "retention", "code_js"} {
		if !available(t, out, key) {
			t.Errorf("%s should be available with the subsystem enabled", key)
		}
	}
	// Scheduler/webhooks mirror the boot condition, which also needs a store —
	// with no store they must report false even though the flag is set.
	for _, key := range []string{"scheduler", "webhooks"} {
		if available(t, out, key) {
			t.Errorf("%s reports available with no store — the boot condition requires one, so an agent would be told its schedule fires when it never will", key)
		}
	}
	// No embedder and no store: the memory legs must all be honestly false.
	for _, key := range []string{"vector_memory", "full_text_memory", "memory_layer", "sql_memory", "documents"} {
		if available(t, out, key) {
			t.Errorf("%s reports available with neither store nor embedder wired", key)
		}
	}

	// Flip the subsystems OFF and watch every flag follow.
	offCfg := capabilitiesCfg()
	offCfg.Env.BashEnabled = false
	offCfg.Env.BashboxEnabled = false
	offCfg.Env.RetentionEnabled = false
	offCfg.Env.CodeAgentsEnabled = false
	offOut := capabilitiesOut(t, &Context{Cfg: offCfg}, ctx)
	for _, key := range []string{"bash", "bashbox", "retention", "code_js"} {
		if available(t, offOut, key) {
			t.Errorf("%s still reports available after the subsystem was disabled — the flag is not reading real state", key)
		}
	}

	// Consolidation: configured + enabled, then staged off. "configured but not
	// enabled" is the state that matters — queued adds are durable but nothing
	// drains them, and an agent should be able to detect that directly.
	cons, _ := out["consolidation"].(map[string]any)
	if cons["available"] != true || cons["configured"] != true {
		t.Errorf("consolidation = %+v, want available+configured with an enabled consolidator schedule", cons)
	}
	stagedOff := capabilitiesCfg()
	sr := stagedOff.ScheduledRuns["memory-consolidation"]
	sr.Enabled = false
	stagedOff.ScheduledRuns["memory-consolidation"] = sr
	cons2, _ := capabilitiesOut(t, &Context{Cfg: stagedOff}, ctx)["consolidation"].(map[string]any)
	if cons2["available"] != false || cons2["configured"] != true {
		t.Errorf("consolidation = %+v with the schedule staged off, want available:false configured:true", cons2)
	}

	// Search: provider NAMES, and the Brave back-compat fallback.
	search, _ := out["search"].(map[string]any)
	names, _ := search["providers"].([]any)
	if len(names) != 2 {
		t.Errorf("search providers = %v, want the two configured names", search["providers"])
	}
	noProviders := capabilitiesCfg()
	noProviders.SearchProviders = nil
	s2, _ := capabilitiesOut(t, &Context{Cfg: noProviders}, ctx)["search"].(map[string]any)
	if n2, _ := s2["providers"].([]any); len(n2) != 1 || n2[0] != "brave" {
		t.Errorf("search providers = %v with only a Brave key set, want [brave]", s2["providers"])
	}

	// Sandbox is per-run: it arrives as MCP tools, so presence in the tool list
	// is the only truth available.
	if available(t, out, "sandbox") {
		t.Error("sandbox reports available with no sandbox tools in the run")
	}
	withSandbox := &Context{Cfg: capabilitiesCfg(), Tools: []tools.Tool{&stubNamedTool{name: "mcp__sandbox__exec"}}}
	if !available(t, capabilitiesOut(t, withSandbox, ctx), "sandbox") {
		t.Error("sandbox should report available when the run carries mcp__sandbox__* tools")
	}
}

// TestContextTool_CapabilitiesLeaksNoSecretsOrTopology is the security gate on
// this surface. It is readable by EVERY agent and every MCP client, so a
// planted secret or address must not survive into the output — including the
// NAME of a key-bearing env var, and including the SearXNG base_url that sits
// one field away from the provider names we do report.
func TestContextTool_CapabilitiesLeaksNoSecretsOrTopology(t *testing.T) {
	cfg := capabilitiesCfg()
	cfg.Memory.Embedder = config.EmbedderConfig{
		Provider:  "ollama-local",
		Model:     "embeddinggemma",
		BaseURL:   "http://192.168.0.77:11434",
		APIKeyEnv: "LOOMCYCLE_EMBEDDER_API_KEY",
	}
	tool := &Context{
		Cfg: cfg,
		// A configured embedder, reported by provider/model/dimension only.
		Embedder: &fakeEmbedder{provider: "ollama-local", model: "embeddinggemma", vocab: map[string]int{"a": 0, "b": 1}},
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1"})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"capabilities"}`))
	if res.IsError {
		t.Fatalf("capabilities: %s", res.Text)
	}
	text := res.Text

	// Secrets and secret-shaped names.
	for _, banned := range []string{
		"brave-secret-key-value",      // a raw API key
		"LOOMCYCLE_EMBEDDER_API_KEY",  // the env-var NAME is a hint too
		"hunter2",                     // a DSN password
		"postgres://",                 // the DSN itself
		"api_key", "apikey", "secret", // generic secret-shaped keys
		"password", "dsn", "bearer",
		"auth_token", "access_token", // (bare "token" would false-match max_tokens)
	} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(banned)) {
			t.Errorf("capabilities leaked %q:\n%s", banned, text)
		}
	}
	// Topology: no URLs, hosts, ports, or paths.
	for _, banned := range []string{
		"http://", "https://", // any URL at all
		"192.168.0.77",             // a private address
		"db.internal",              // a hostname
		":11434", ":8080", ":8787", // ports (incl. the listen addr)
		"/var/", "/etc/", // paths
	} {
		if strings.Contains(text, banned) {
			t.Errorf("capabilities leaked infrastructure topology %q:\n%s", banned, text)
		}
	}

	// ...while still reporting the embedder facts an agent legitimately needs.
	out := decodeResult(t, text)
	vec, _ := out["vector_memory"].(map[string]any)
	emb, ok := vec["embedder"].(map[string]any)
	if !ok {
		t.Fatalf("vector_memory.embedder missing: %v", out["vector_memory"])
	}
	if emb["provider"] != "ollama-local" || emb["model"] != "embeddinggemma" {
		t.Errorf("embedder identity = %+v, want provider/model reported", emb)
	}
	if d, _ := emb["dimension"].(float64); d != 2 {
		t.Errorf("embedder dimension = %v, want 2 — an agent needs it to know whether stored vectors are comparable", emb["dimension"])
	}
}

// TestContextTool_CapabilitiesTenantOutputIsSubsetOfAdmin: a substrate:tenant
// caller learns feature AVAILABILITY (it needs that to branch) but no
// operator-global deployment detail. The storage backend kind is the one field
// that is pure infrastructure, so it is admin-only — which makes "tenant ⊆
// admin" a claim with actual content.
func TestContextTool_CapabilitiesTenantOutputIsSubsetOfAdmin(t *testing.T) {
	tool := &Context{Cfg: capabilitiesCfg()}
	base := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a1"})

	adminCtx := auth.WithPrincipal(base, auth.Principal{
		Subject: "root", TenantID: "acme", Scopes: []string{auth.ScopeAdmin},
	})
	tenantCtx := auth.WithPrincipal(base, auth.Principal{
		Subject: "svc", TenantID: "acme", Scopes: []string{auth.ScopeTenant},
	})

	adminOut := capabilitiesOut(t, tool, adminCtx)
	tenantOut := capabilitiesOut(t, tool, tenantCtx)

	for key := range tenantOut {
		if _, ok := adminOut[key]; !ok {
			t.Errorf("tenant sees key %q that admin does not — tenant output must be a SUBSET of admin's", key)
		}
	}
	if _, ok := adminOut["storage"]; !ok {
		t.Error("admin should see the storage backend kind")
	}
	if _, ok := tenantOut["storage"]; ok {
		t.Errorf("a substrate:tenant caller must not learn the storage backend — it is operator infrastructure, not a capability: %v", tenantOut["storage"])
	}
	// Availability itself is NOT stripped: a tenant needs it to branch.
	for _, key := range []string{"vector_memory", "sql_memory", "documents", "bash", "search", "consolidation", "limits"} {
		if _, ok := tenantOut[key]; !ok {
			t.Errorf("tenant is missing %q — feature availability must reach every caller, or it cannot branch before calling", key)
		}
	}
}

// stubNamedTool is a minimal tools.Tool that only needs to answer Name().
type stubNamedTool struct{ name string }

func (s *stubNamedTool) Name() string                 { return s.name }
func (s *stubNamedTool) Description() string          { return "" }
func (s *stubNamedTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (s *stubNamedTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{}, nil
}
