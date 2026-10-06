package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
	"github.com/denn-gubsky/loomcycle/internal/config"

	// Every driver the default-providers layer names must be registered, as in
	// cmd/loomcycle: the config refuses a provider whose driver is not compiled in.
	_ "github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/deepseek"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/llamacpp"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/mock"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/vllm"
)

// benchStacks are the config stacks this harness is driven with, each layered as
// the server layers LOOMCYCLE_CONFIG_FILES (left to right, last wins) over the
// presets the drivers select.
var benchStacks = map[string][]string{
	"rating":        {"bench/synthesis/ingest.yaml", "bench/synthesis/lme_gate_arms.yaml", "bench/synthesis/locomo_rating.yaml"},
	"pa arms":       {"bench/synthesis/ingest.yaml", "bench/synthesis/pa_arms.yaml"},
	"memory rerank": {"bench/synthesis/ingest.yaml", "bench/synthesis/lme_gate_arms.yaml", "bench/synthesis/locomo_rating.yaml", "bench/docs/memory-rerank/measure.yaml", "bench/docs/memory-rerank/rerank-on.yaml"},
}

// TestBenchAgents_OnlyTheScribeIsAConversation — every LoCoMo agent but the
// scribe is `internal`, in every stack the harness runs with, after the overlays.
//
// The scribe's chats ARE the corpus. Every other agent's runs (answerers, the
// judge, the verifier) are bookkeeping that shares the scribe's user, and an
// internal agent's sessions are what the consolidation scan and History
// list/search skip. Without it, a store built after an answer phase consolidated
// the previous conversation's answer runs as chats (RFC DR P2: conv-30 read 93
// chats for its own 19), and an answer could reach an earlier answer by History.
func TestBenchAgents_OnlyTheScribeIsAConversation(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	presets, err := embedded.ResolveUnits([]string{"base", "memory"})
	if err != nil {
		t.Fatal(err)
	}
	for name, files := range benchStacks {
		t.Run(name, func(t *testing.T) {
			layers := []config.Layer{{Name: "providers.default", Data: embedded.DefaultProviders()}}
			for _, u := range presets {
				layers = append(layers, config.Layer{Name: u.Name, Data: u.Data})
			}
			for _, f := range files {
				data, err := os.ReadFile(filepath.Join(root, f))
				if err != nil {
					t.Fatal(err)
				}
				layers = append(layers, config.Layer{Name: f, Data: data})
			}
			cfg, err := config.LoadLayers(layers...)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			n := 0
			for agent, def := range cfg.Agents {
				if len(agent) < 7 || agent[:7] != "locomo/" {
					continue
				}
				n++
				if want := agent != "locomo/scribe"; def.Internal != want {
					t.Errorf("%s: internal = %v, want %v", agent, def.Internal, want)
				}
			}
			if n < 3 {
				t.Fatalf("only %d locomo/ agents loaded; the stack did not load what the harness runs", n)
			}
		})
	}
}
