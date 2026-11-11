package config

import (
	"strings"
	"testing"
)

// TestValidate_ReportsTheFirstBrokenBlockInCheckOrder pins which error validate
// returns when several config blocks are wrong at once: the first in the order
// validate checks them. Callers show only that one error, so the order is what
// an operator fixing a config sees, and a reshuffle of the checks would change
// it silently. Each step drops the block just reported and expects the next.
// One bad entry per block keeps the answer independent of map order.
func TestValidate_ReportsTheFirstBrokenBlockInCheckOrder(t *testing.T) {
	bad := []struct {
		yaml, want string
	}{
		{"concurrency:\n  max_concurrent_runs: -1\n", "concurrency.max_concurrent_runs must be >= 1"},
		{"context_plugins:\n  - name: nope\n", "context_plugins[0]: unknown plugin"},
		{"skills:\n  \"bad..name\":\n    body: x\n", "skills: inline skill"},
		{"provider_priority: [nope]\n", "provider_priority[0]: unknown provider"},
		{"search_providers:\n  nope: {}\n", "search_providers: unknown provider"},
		{"providers:\n  mine:\n    driver: \"\"\n", "providers.mine: driver is required"},
		{"models:\n  m1:\n    model: a\n    model_pattern: b*\n", "models.m1: exactly one of model or model_pattern"},
		{"tiers:\n  bogus: []\n", "tiers.bogus: unknown tier"},
		{"user_tiers:\n  gold: {}\n", "user_tiers: a \"default\" entry is required"},
		{"memory_backends:\n  mb:\n    kind: remote\n", "memory_backends.mb: kind=remote requires config.base_url"},
		{"change_subscriptions:\n  s:\n    callback_url: \"\"\n", "change_subscriptions.s: callback_url is required"},
		{"document_sources:\n  d:\n    config:\n      base_url: \"\"\n", "document_sources.d: config.base_url is required"},
		{"agents:\n  \"bad..name\":\n    model: m\n", "agents: "},
		{"channels:\n  c1:\n    scope: bogus\n", "channels.c1: unknown scope"},
		{"mcp_servers:\n  s:\n    transport: carrier-pigeon\n", "mcp_servers.s: unknown transport"},
		{"scheduled_runs:\n  nightly:\n    agent: ghost\n", "scheduled_runs.nightly: agent \"ghost\" not declared"},
		{"memory:\n  consolidation:\n    merge_threshold: 2\n", "memory.consolidation.merge_threshold must be > 0 and <= 1"},
	}
	load := func(blocks []string) error {
		_, err := LoadLayers(Layer{Name: "pin", Data: []byte(strings.Join(blocks, ""))})
		return err
	}
	for i, b := range bad {
		var rest []string
		for _, r := range bad[i:] {
			rest = append(rest, r.yaml)
		}
		err := load(rest)
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Fatalf("with block %d and every later broken block: err = %v, want it to contain %q", i, err, b.want)
		}
		if err := load([]string{b.yaml}); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Fatalf("block %d alone: err = %v, want it to contain %q (the block does not break what it claims)", i, err, b.want)
		}
	}
	if err := load(nil); err != nil {
		t.Fatalf("no broken block: err = %v, want nil", err)
	}
}

// TestValidate_ReportsAnAgentsFirstBrokenFieldInCheckOrder is the same pin
// inside one agent: its routing fields are checked before its memory fields,
// and those before its tool grants.
func TestValidate_ReportsAnAgentsFirstBrokenFieldInCheckOrder(t *testing.T) {
	bad := []struct {
		yaml, want string
	}{
		{"    effort: extreme\n", "invalid effort"},
		{"    memory_scopes: [nowhere]\n", "memory_scopes[0]: unknown scope"},
		{"    history_scope: [siblings]\n", "history_scope[0]: unknown scope"},
	}
	for i, b := range bad {
		src := "agents:\n  a1:\n    model: m\n"
		for _, r := range bad[i:] {
			src += r.yaml
		}
		_, err := LoadLayers(Layer{Name: "pin", Data: []byte(src)})
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Fatalf("with field %d and every later broken field: err = %v, want it to contain %q", i, err, b.want)
		}
	}
}
