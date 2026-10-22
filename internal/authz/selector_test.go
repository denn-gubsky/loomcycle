package authz

import (
	"strings"
	"testing"
)

func TestParseSelector_ShapesPerType(t *testing.T) {
	good := []string{
		"agent:chat/*", "agent:chat/**", "agent:*", "agent:ccq-score", "agent:a/b/c",
		"volume:shared-ro", "tool:Bash", "tool:*",
		"memory:tenant", "memory:*", "memory:user:support-*", "memory:global",
		"document:tenant:/kb/**", "document:user", "document:*:/x",
		"def:agent/chat/*", "def:acl/*", "def:*",
		"history:self", "history:*", "run:own", "gateway:chat", "crypto:tenant",
		"principal:user:support-*", "principal:group:*", "runtime:limits", "transport:bridge:telegram",
		"git:github.com/org/*", "mcp:jobs/get*", "a2a:peer/skill",
	}
	for _, sel := range good {
		if _, err := parseSelector(sel); err != nil {
			t.Errorf("parseSelector(%q): %v", sel, err)
		}
	}
	bad := map[string]string{
		"":                                  "want <type>:<name>",
		"agent":                             "want <type>:<name>",
		"nosuch:x":                          "not a catalog type",
		"agent:":                            "a name is required",
		"agent:a//b":                        "empty segment",
		"agent:../x":                        "climbs",
		"memory:nosuch":                     "scope",
		"history:nosuch":                    "not one of",
		"history:self/x":                    "not one of",
		"def:nosuchfamily/x":                "scope",
		"run:tenant/extra":                  "not one of",
		"agent:a b":                         "control or space",
		"agent:" + strings.Repeat("x", 300): "longer than",
	}
	for sel, want := range bad {
		_, err := parseSelector(sel)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseSelector(%q) = %v, want error containing %q", sel, err, want)
		}
	}
}

func TestSelector_Matches(t *testing.T) {
	cases := []struct {
		sel, name string
		want      bool
	}{
		{"agent:chat/*", "chat/support", true},
		{"agent:chat/*", "chat/a/b", false},
		{"agent:chat/**", "chat/a/b", true},
		{"agent:chat/**", "chat", true},
		{"agent:*", "anything/at/all", true},
		{"agent:ccq-*", "ccq-score", true},
		{"agent:ccq-*", "ccq/score", false},
		{"agent:exact", "exact", true},
		{"agent:exact", "exact2", false},
		{"memory:tenant", "tenant", true},
		{"memory:tenant", "tenant:anything", true},
		{"memory:user", "tenant", false},
		{"memory:*", "global", true},
		{"memory:user:support-*", "user:support-1", true},
		{"memory:user:support-*", "user:ops-1", false},
		{"document:tenant:/kb/**", "tenant:/kb/policies/x", true},
		{"document:tenant:/kb/**", "tenant:kb", true},
		{"document:tenant:/kb/**", "tenant:/other", false},
		{"document:tenant:/kb/**", "user:/kb/x", false},
		{"def:agent/chat/*", "agent/chat/x", true},
		{"def:agent/chat/*", "skill/chat/x", false},
		{"def:*", "acl/anything", true},
		{"history:self", "self", true},
		{"history:self", "user", false},
		{"history:*", "global", true},
		{"run:own", "own", true},
		{"git:github.com/org/*", "github.com/org/repo", true},
		{"git:github.com/org/*", "gitea.local/org/repo", false},
	}
	for _, c := range cases {
		s, err := parseSelector(c.sel)
		if err != nil {
			t.Fatalf("parseSelector(%q): %v", c.sel, err)
		}
		spec := catalogByName[s.typ]
		if got := s.matches(spec, c.name); got != c.want {
			t.Errorf("%q matches %q = %v, want %v", c.sel, c.name, got, c.want)
		}
	}
}

func TestParseSelector_LiteralIsDetected(t *testing.T) {
	s, _ := parseSelector("tool:Bash")
	if s.literal != "Bash" {
		t.Errorf("tool:Bash literal = %q", s.literal)
	}
	s, _ = parseSelector("tool:Ba*")
	if s.literal != "" {
		t.Errorf("tool:Ba* literal = %q, want none", s.literal)
	}
	s, _ = parseSelector("document:tenant:/kb/x")
	if s.literal != "kb/x" {
		t.Errorf("scoped literal = %q", s.literal)
	}
}
