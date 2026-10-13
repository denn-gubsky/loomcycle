package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The decision block and the memory reranker each had a cap of their own on
// calls in flight (max_concurrent, default 4). Both are gone: every model call
// is counted in a lane of the provider it goes to. A config that still sets one
// must FAIL to load and say where the cap lives now — loading it and ignoring
// the key would leave an operator who set it to protect a model host with no
// cap at all, and no sign of it.

const (
	wantDecisionRemoved = "decision.max_concurrent was removed: cap decision calls per provider with providers.<id>.lanes.decision.max_concurrent"
	wantRerankerRemoved = "memory.reranker.max_concurrent was removed: a listwise rerank is a chat call, capped by providers.<id>.lanes.chat.max_concurrent (or max_concurrent); a decision rerank by providers.<id>.lanes.decision.max_concurrent"
)

const oldDecisionBlock = `
decision:
  default: decide
  max_concurrent: 2
`

const oldRerankerBlock = `
memory:
  reranker:
    provider: ollama-local
    model: qwen3.6:latest
    max_concurrent: 2
`

// TestLoad_ARemovedConcurrencyKeyFailsNamingItsReplacement — one file, each key.
// The value does not matter: the key being written at all is the fault, so 0
// (the old "use the default") fails like any other.
func TestLoad_ARemovedConcurrencyKeyFailsNamingItsReplacement(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"decision", oldDecisionBlock, wantDecisionRemoved},
		{"decision, zero", "\ndecision:\n  default: decide\n  max_concurrent: 0\n", wantDecisionRemoved},
		{"reranker", oldRerankerBlock, wantRerankerRemoved},
		{"reranker, zero", "\nmemory:\n  reranker:\n    provider: ollama-local\n    model: m\n    max_concurrent: 0\n", wantRerankerRemoved},
		{"reranker, decision kind", "\nmemory:\n  reranker:\n    kind: decision\n    model: decide\n    max_concurrent: 2\n", wantRerankerRemoved},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, decisionCfg(c.body)))
			if err == nil {
				t.Fatal("the config loaded: the removed key was accepted or silently dropped")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not name the replacement:\n got: %v\nwant: %s", err, c.want)
			}
		})
	}
}

// TestLoadLayers_ARemovedConcurrencyKeyFailsInAnyLayer — a layered config
// (presets → CONFIG_DIR → CONFIG_FILES → --config) is merged before it is
// decoded, so the old key fails wherever it sits: in a base layer the
// operator's file does not mention, in the operator's file over a clean base,
// and in a file read from disk as well as an in-memory layer. A later layer
// cannot hide it — there is no value to override it with that makes it valid.
func TestLoadLayers_ARemovedConcurrencyKeyFailsInAnyLayer(t *testing.T) {
	base := decisionCfg("")
	cleanDecision := "\ndecision:\n  default: decide\n  models: [decide]\n  timeout_ms: 20000\n"
	cleanReranker := "\nmemory:\n  reranker:\n    provider: ollama-local\n    model: qwen3.6:latest\n    timeout_ms: 20000\n"
	file := func(body string) Layer {
		p := filepath.Join(t.TempDir(), "layer.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Layer{Name: p}
	}
	mem := func(name, body string) Layer { return Layer{Name: name, Data: []byte(body)} }
	for _, c := range []struct {
		name   string
		layers []Layer
		want   string
	}{
		{"decision: in the base layer", []Layer{mem("preset", base+oldDecisionBlock), file(cleanDecision)}, wantDecisionRemoved},
		{"decision: in the last file", []Layer{mem("preset", base+cleanDecision), file(oldDecisionBlock)}, wantDecisionRemoved},
		{"decision: in a middle file", []Layer{mem("preset", base), file(oldDecisionBlock), file(cleanDecision)}, wantDecisionRemoved},
		{"reranker: in the base layer", []Layer{mem("preset", base+oldRerankerBlock), file(cleanReranker)}, wantRerankerRemoved},
		{"reranker: in the last file", []Layer{mem("preset", base+cleanReranker), file(oldRerankerBlock)}, wantRerankerRemoved},
		{"reranker: in a middle file", []Layer{mem("preset", base), file(oldRerankerBlock), file(cleanReranker)}, wantRerankerRemoved},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadLayers(c.layers...)
			if err == nil {
				t.Fatal("the layered config loaded: the removed key was accepted or silently dropped")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not name the replacement:\n got: %v\nwant: %s", err, c.want)
			}
		})
	}
	// Control: the same layers without the key load, so the failures above are
	// the key and nothing else about these configs.
	if _, err := LoadLayers(mem("preset", base+cleanDecision), file(cleanReranker)); err != nil {
		t.Fatalf("control: the clean layered config does not load: %v", err)
	}
}

// TestLoad_TheReplacementLoads — what the two messages point to is a config
// that loads: the caps, per kind, on the provider.
func TestLoad_TheReplacementLoads(t *testing.T) {
	cfg, err := Load(writeCfg(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  ollama-local:
    driver: ollama
    base_url: "http://ollama.test:11434"
    lanes:
      chat:     { max_concurrent: 2 }
      decision: { max_concurrent: 2 }
models:
  decide: { provider: ollama-local, model: nimble, kind: decision }
decision:
  default: decide
memory:
  reranker:
    provider: ollama-local
    model: qwen3.6:latest
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	lanes := cfg.Providers["ollama-local"].EffectiveLanes()
	if lanes[ModelKindChat].MaxConcurrent != 2 || lanes[ModelKindDecision].MaxConcurrent != 2 {
		t.Errorf("lanes = %+v, want chat and decision capped at 2", lanes)
	}
}
