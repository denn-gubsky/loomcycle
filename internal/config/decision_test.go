package config

import (
	"reflect"
	"strings"
	"testing"
)

// decisionCfg is a config with an Ollama and a non-Ollama provider, aliases of
// each tagging, and body.
func decisionCfg(body string) string {
	return `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  ollama-local: { driver: ollama, base_url: "http://ollama.test:11434" }
  openai:       { driver: openai, api_key_env: OPENAI_API_KEY }
models:
  decide:      { provider: ollama-local, model: nimble, kind: decision }
  decide-deep: { provider: ollama-local, model: clef, kind: decision }
  plain:       { provider: ollama-local, model: nimble }
  talker:      { provider: ollama-local, model: qwen3.6:latest, kind: chat }
  newest:      { provider: ollama-local, model_pattern: "nimble*", kind: decision }
  elsewhere:   { provider: openai, model: gpt-5.4-mini, kind: decision }
  homeless:    { model: nimble, kind: decision }
` + body
}

func entryNames(c *Config) []string {
	var out []string
	for _, e := range c.DecisionEntries() {
		out = append(out, e.Name)
	}
	return out
}

// TestDecisionConfig_LoadsFromYaml — every field of the block round-trips, and
// the entries are the listed models with the default among them.
func TestDecisionConfig_LoadsFromYaml(t *testing.T) {
	cfg, err := Load(writeCfg(t, decisionCfg(`
decision:
  default: decide
  models: [decide-deep, nimble-lit]
  provider: ollama-local
  timeout_ms: 20000
  max_concurrent: 2
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := DecisionConfig{Default: "decide", Models: []string{"decide-deep", "nimble-lit"},
		Provider: "ollama-local", TimeoutMs: 20000, MaxConcurrent: 2}
	if !reflect.DeepEqual(cfg.Decision, want) {
		t.Errorf("decision = %+v, want %+v", cfg.Decision, want)
	}
	// An alias carries its own provider; a plain name takes the block's.
	wantEntries := []DecisionEntry{{Name: "decide-deep"}, {Name: "nimble-lit", Provider: "ollama-local"}, {Name: "decide"}}
	if got := cfg.DecisionEntries(); !reflect.DeepEqual(got, wantEntries) {
		t.Errorf("entries = %+v, want %+v", got, wantEntries)
	}
	if got := kindWarnings(cfg); len(got) != 0 {
		t.Errorf("warnings = %q, want none: every alias is tagged and a plain name is never checked", got)
	}
}

// TestDecisionConfig_AbsentMeansOff — no block, no decision models, whatever
// aliases are tagged.
func TestDecisionConfig_AbsentMeansOff(t *testing.T) {
	cfg, err := Load(writeCfg(t, decisionCfg("")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Decision.Configured() || len(cfg.DecisionEntries()) != 0 {
		t.Errorf("no decision block, yet configured: %+v entries %v", cfg.Decision, entryNames(cfg))
	}
}

// TestDecisionConfig_ModelsOmittedMeansTheTaggedAliases — with no list, a caller
// may name every alias tagged kind: decision, and no other. (The tagged aliases
// that could not be built are left out of this fixture; see the refusals.)
func TestDecisionConfig_ModelsOmittedMeansTheTaggedAliases(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  ollama-local: { driver: ollama, base_url: "http://ollama.test:11434" }
models:
  decide:      { provider: ollama-local, model: nimble, kind: decision }
  decide-deep: { provider: ollama-local, model: clef, kind: decision }
  plain:       { provider: ollama-local, model: nimble }
  talker:      { provider: ollama-local, model: qwen3.6:latest, kind: chat }
decision:
  default: decide-deep
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := entryNames(cfg); !reflect.DeepEqual(got, []string{"decide", "decide-deep"}) {
		t.Errorf("entries = %v, want the two tagged aliases and neither the untagged nor the chat one", got)
	}
}

// TestDecisionConfig_AnUntaggedAliasLoadsAndWarns — an alias written before the
// tag existed still loads in the block, named in an advisory with the tag to add.
func TestDecisionConfig_AnUntaggedAliasLoadsAndWarns(t *testing.T) {
	cfg, err := Load(writeCfg(t, decisionCfg("decision:\n  default: plain\n  models: [plain]\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := kindWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "decision.default") || !strings.Contains(got[0], "models.plain") ||
		!strings.Contains(got[0], "`kind: decision`") {
		t.Errorf("warnings = %q, want one naming decision.default, models.plain and kind: decision", got)
	}
}

// TestDecisionConfig_RejectsABlockThatCouldNotBeBuilt — each refusal names the
// entry and what is wrong with it, at load.
func TestDecisionConfig_RejectsABlockThatCouldNotBeBuilt(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       []string
	}{
		{"no default", "models: [decide]", []string{"decision.default is required"}},
		{"a chat alias", "default: talker", []string{"decision.default", "models.talker is kind: chat", "kind: decision is needed"}},
		{"a chat alias in the list", "default: decide\n  models: [talker]", []string{"decision.models", "models.talker is kind: chat"}},
		{"a pattern alias", "default: newest", []string{`"newest" is a model_pattern alias`}},
		{"a plain name with no provider", "default: nimble", []string{"decision.default", `"nimble" is not a models: alias`, "decision.provider"}},
		{"an undeclared block provider", "default: nimble\n  provider: nowhere", []string{"decision.provider", `"nowhere" is not declared`}},
		{"a provider with no decision driver", "default: elsewhere", []string{"decision.default", `"elsewhere"`, `provider "openai"`, `"openai" driver serves no decision models`, "ollama"}},
		{"a plain name on such a provider", "default: gpt-x\n  provider: openai", []string{`"openai" driver serves no decision models`}},
		{"an alias with no provider", "default: homeless", []string{"decision.default", "models.homeless names no provider"}},
		{"a negative timeout", "default: decide\n  timeout_ms: -1", []string{"decision.timeout_ms"}},
		{"a negative bound", "default: decide\n  max_concurrent: -1", []string{"decision.max_concurrent"}},
		{"an empty entry", "default: decide\n  models: [\"\"]", []string{"decision.models", "empty"}},
		{"an unknown key", "default: decide\n  model: nimble", []string{`decision: unknown key "model"`, "default"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The list is written out (empty unless the case sets one), so the only
			// entries are the ones the case names.
			body := c.body
			if !strings.Contains(body, "models:") {
				body += "\n  models: []"
			}
			_, err := Load(writeCfg(t, decisionCfg("decision:\n  "+body+"\n")))
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

// TestDecisionConfig_AnUnbuildableTaggedAliasFailsLoadWhenNoListIsWritten — with
// no list every tagged alias is allowed, so every one must be buildable, and the
// refusal says why an entry nobody wrote is being checked.
func TestDecisionConfig_AnUnbuildableTaggedAliasFailsLoadWhenNoListIsWritten(t *testing.T) {
	_, err := Load(writeCfg(t, decisionCfg("decision:\n  default: decide\n")))
	if err == nil || !strings.Contains(err.Error(), "no models listed") || !strings.Contains(err.Error(), "kind: decision") {
		t.Errorf("err = %v, want a refusal explaining that every tagged alias is allowed", err)
	}
}

// TestDecisionConfig_AnUndeclaredAliasProviderFailsLoad — the provider an alias
// names must be one this deployment declares: a built-in id passes the alias's
// own check without being declared, and the block could not be built on it.
func TestDecisionConfig_AnUndeclaredAliasProviderFailsLoad(t *testing.T) {
	_, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
models:
  decide: { provider: ollama-local, model: nimble, kind: decision }
decision:
  default: decide
`))
	if err == nil || !strings.Contains(err.Error(), `provider "ollama-local", which is not declared in providers`) {
		t.Errorf("err = %v, want a refusal naming the undeclared provider", err)
	}
}

// TestDecisionConfig_MergesAcrossLayers — the block layers like any other: a
// later layer changes the default and replaces the list, and keeps what it does
// not restate.
func TestDecisionConfig_MergesAcrossLayers(t *testing.T) {
	base := []byte(decisionCfg(`
decision:
  default: decide
  models: [decide]
  timeout_ms: 20000
`))
	overlay := []byte(`
decision:
  default: decide-deep
  models: [decide, decide-deep]
`)
	cfg, err := LoadLayers(Layer{Name: "base", Data: base}, Layer{Name: "operator", Data: overlay})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	want := DecisionConfig{Default: "decide-deep", Models: []string{"decide", "decide-deep"}, TimeoutMs: 20000}
	if !reflect.DeepEqual(cfg.Decision, want) {
		t.Errorf("decision = %+v, want %+v", cfg.Decision, want)
	}
	// The unknown-key check holds on the layered path too.
	_, err = LoadLayers(Layer{Name: "base", Data: base}, Layer{Name: "operator", Data: []byte("decision:\n  modles: [decide]\n")})
	if err == nil || !strings.Contains(err.Error(), `unknown key "modles"`) {
		t.Errorf("layered unknown key: err = %v, want a refusal", err)
	}
}
