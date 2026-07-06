package config

import (
	"strings"
	"testing"
)

// TestValidate_SearchProviders covers the RFC BB config surface: the enabled
// set is checked against the known drivers, SearXNG needs a base_url, and both
// the global search_priority and each agent's search_providers must reference
// only enabled providers.
func TestValidate_SearchProviders(t *testing.T) {
	base := func(sp map[string]SearchProviderConfig, prio []string, agents map[string]AgentDef) *Config {
		return &Config{
			Defaults:        Defaults{Provider: "anthropic", Model: "x"},
			Concurrency:     Concurrency{MaxConcurrentRuns: 1},
			SearchProviders: sp,
			SearchPriority:  prio,
			Agents:          agents,
		}
	}

	if err := validate(base(
		map[string]SearchProviderConfig{"brave": {}, "serper": {}, "searxng": {BaseURL: "http://sx:8080"}},
		[]string{"serper", "brave", "searxng"}, nil)); err != nil {
		t.Errorf("valid search config rejected: %v", err)
	}

	cases := []struct {
		name    string
		cfg     *Config
		wantSub string
	}{
		{"unknown provider",
			base(map[string]SearchProviderConfig{"bing": {}}, nil, nil), "unknown provider"},
		{"searxng missing base_url",
			base(map[string]SearchProviderConfig{"searxng": {}}, nil, nil), "base_url is required"},
		{"priority references unconfigured",
			base(map[string]SearchProviderConfig{"brave": {}}, []string{"serper"}, nil), "not an enabled"},
		{"agent references unconfigured",
			base(map[string]SearchProviderConfig{"brave": {}}, nil,
				map[string]AgentDef{"r": {Tier: "middle", Tools: []string{"WebSearch"}, SearchProviders: []string{"exa"}}}),
			"not an enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("got %v, want error containing %q", err, tc.wantSub)
			}
		})
	}

	// An agent that lists only enabled providers passes.
	ok := base(map[string]SearchProviderConfig{"brave": {}, "exa": {}}, []string{"brave"},
		map[string]AgentDef{"r": {Tier: "middle", Tools: []string{"WebSearch"}, SearchProviders: []string{"exa", "brave"}}})
	if err := validate(ok); err != nil {
		t.Errorf("agent with configured providers rejected: %v", err)
	}
}

// TestSearchHostKey maps each provider id to its env-derived operator key;
// keyless/unknown → "".
func TestSearchHostKey(t *testing.T) {
	c := &Config{Env: Env{BraveAPIKey: "b", SerperAPIKey: "s", ExaAPIKey: "e", TavilyAPIKey: "t"}}
	want := map[string]string{"brave": "b", "serper": "s", "exa": "e", "tavily": "t", "searxng": "", "nope": ""}
	for id, w := range want {
		if got := c.SearchHostKey(id); got != w {
			t.Errorf("SearchHostKey(%q) = %q, want %q", id, got, w)
		}
	}
}
