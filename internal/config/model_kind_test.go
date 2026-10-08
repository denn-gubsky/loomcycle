package config

import (
	"strings"
	"testing"
)

// kindCfg is a config whose models: map holds one alias of each tagging, plus
// body. The providers are the registered Ollama embedder ids, so the embedder
// block validates without a providers: block.
func kindCfg(body string) string {
	return `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  plain:     { provider: ollama, model: qwen3.6:latest }
  talker:    { provider: ollama, model: qwen3.6:latest, kind: chat }
  decider:   { provider: ollama, model: nimble, kind: decision }
  embedding: { provider: ollama, model: bge-m3:latest, kind: embedder }
` + body
}

// TestModelKind_UnknownValueFailsLoad — a typo'd kind must not load as "chat".
func TestModelKind_UnknownValueFailsLoad(t *testing.T) {
	_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  decider: { provider: ollama, model: nimble, kind: decison }
`))
	if err == nil || !strings.Contains(err.Error(), `models.decider: kind "decison"`) {
		t.Fatalf("err = %v, want a refusal naming models.decider and the bad kind", err)
	}
}

// TestModelKind_ATaggedAliasInTheWrongPlaceFailsLoad — every place a models:
// alias may be written refuses one tagged for another kind, and the error names
// the alias, its kind and the place.
func TestModelKind_ATaggedAliasInTheWrongPlaceFailsLoad(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       []string
	}{
		{"decision alias as an agent's model", `
agents:
  a: { model: decider, max_tokens: 100 }
`, []string{`agent "a": model`, "models.decider is kind: decision", "kind: chat is needed"}},
		{"embedder alias as an agent's model", `
agents:
  a: { model: embedding, max_tokens: 100 }
`, []string{`agent "a": model`, "models.embedding is kind: embedder", "kind: chat is needed"}},
		{"decision alias as a tier candidate", `
tiers:
  low: [decider]
`, []string{"tiers.low[0]", "models.decider is kind: decision", "kind: chat is needed"}},
		{"decision alias as a user tier's candidate", `
user_tiers:
  default:
    tiers:
      low: [decider]
`, []string{"user_tiers.default.tiers.low[0]", "models.decider is kind: decision"}},
		{"decision alias as an agent's tier candidate", `
agents:
  a:
    tier: low
    max_tokens: 100
    models:
      low: [decider]
`, []string{`agent "a": models.low[0]`, "models.decider is kind: decision"}},
		{"chat alias as the embedder", `
memory:
  embedder: { model: talker }
`, []string{"memory.embedder.model", "models.talker is kind: chat", "kind: embedder is needed"}},
		{"decision alias as the embedder", `
memory:
  embedder: { model: decider }
`, []string{"memory.embedder.model", "models.decider is kind: decision", "kind: embedder is needed"}},
		{"decision alias as a listwise reranker", `
memory:
  reranker: { model: decider }
`, []string{"memory.reranker.model", "models.decider is kind: decision", "kind: chat is needed"}},
		{"chat alias as a decision reranker", `
memory:
  reranker: { kind: decision, model: talker }
`, []string{"memory.reranker.model", "models.talker is kind: chat", "kind: decision is needed"}},
		{"embedder alias as the unit generator", `
memory:
  unit_generator: { model: embedding }
`, []string{"memory.unit_generator.model", "models.embedding is kind: embedder", "kind: chat is needed"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, kindCfg(c.body)))
			if err == nil {
				t.Fatal("the config loaded; want a refusal")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q, want it to contain %q", err, w)
				}
			}
		})
	}
}

// kindWarnings are the advisories about an untagged alias.
func kindWarnings(c *Config) []string {
	var out []string
	for _, w := range c.Warnings {
		if strings.Contains(w, "declares no kind") {
			out = append(out, w)
		}
	}
	return out
}

// TestModelKind_AnUntaggedAliasInASpecialPlaceLoadsAndWarns — aliases written
// before the tag existed keep loading where an embedder or decision model is
// needed, and the operator is told which alias to tag and with what.
func TestModelKind_AnUntaggedAliasInASpecialPlaceLoadsAndWarns(t *testing.T) {
	for _, c := range []struct{ name, body, place, tag string }{
		{"embedder", "memory:\n  embedder: { model: plain }\n", "memory.embedder.model", "kind: embedder"},
		{"decision reranker", "memory:\n  reranker: { kind: decision, model: plain }\n", "memory.reranker.model", "kind: decision"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Load(writeCfg(t, kindCfg(c.body)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := kindWarnings(cfg)
			if len(got) != 1 || !strings.Contains(got[0], c.place) || !strings.Contains(got[0], "models.plain") ||
				!strings.Contains(got[0], "`"+c.tag+"`") {
				t.Errorf("warnings = %q, want one naming %s, models.plain and %s", got, c.place, c.tag)
			}
		})
	}
}

// TestModelKind_NoWarningWhereNothingIsAmiss — an untagged alias is a chat
// model, so it says nothing in a chat place; a correctly tagged alias says
// nothing anywhere; and a literal model name is never looked at.
func TestModelKind_NoWarningWhereNothingIsAmiss(t *testing.T) {
	cfg, err := Load(writeCfg(t, kindCfg(`
tiers:
  low: [plain, talker]
agents:
  a: { model: plain, max_tokens: 100 }
  b: { model: talker, max_tokens: 100 }
  c: { provider: ollama, model: nimble, max_tokens: 100 }
memory:
  embedder: { model: embedding }
  reranker: { kind: decision, model: decider }
  unit_generator: { model: plain }
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := kindWarnings(cfg); len(got) != 0 {
		t.Errorf("warnings = %q, want none", got)
	}
	// The listwise reranker is the one service block that takes a chat model.
	cfg, err = Load(writeCfg(t, kindCfg("memory:\n  reranker: { model: plain }\n")))
	if err != nil {
		t.Fatalf("Load (listwise reranker): %v", err)
	}
	if got := kindWarnings(cfg); len(got) != 0 {
		t.Errorf("listwise reranker on an untagged alias: warnings = %q, want none", got)
	}
}

// TestResolveAgentDefModel_RefusesANonChatAlias — a def authored at run time is
// never seen by config load, so the pin's kind is refused where it resolves.
func TestResolveAgentDefModel_RefusesANonChatAlias(t *testing.T) {
	c := &Config{Models: map[string]ModelRef{
		"decider": {Provider: "ollama", Model: "nimble", Kind: ModelKindDecision},
		"plain":   {Provider: "ollama", Model: "qwen3.6:latest"},
	}}
	_, _, _, err := c.ResolveAgentDefModel("dyn", AgentDef{Model: "decider"})
	if err == nil || !strings.Contains(err.Error(), `agent "dyn": model: models.decider is kind: decision`) {
		t.Errorf("err = %v, want a refusal naming the agent, the alias and its kind", err)
	}
	if p, m, _, err := c.ResolveAgentDefModel("dyn", AgentDef{Model: "plain"}); err != nil || p != "ollama" || m != "qwen3.6:latest" {
		t.Errorf("untagged alias = %s/%s, %v; want ollama/qwen3.6:latest", p, m, err)
	}
}
