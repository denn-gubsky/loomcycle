package http

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// Bashbox is registered on every server, so a registered agent must not get
// a shell (and, where the operator allowlisted host fallback commands, real
// host processes) by naming it. It is stripped with Bash, Write and Edit
// unless the operator let registered agents keep privileged tools.
func TestRegisterAgent_StripsBashboxUnlessPrivilegedToolsAreAllowed(t *testing.T) {
	for _, tc := range []struct {
		label string
		allow bool
		want  []string
	}{
		{"not allowed", false, []string{"Read", "Memory"}},
		{"allowed", true, []string{"Read", "Bashbox", "Bash", "Write", "Edit", "Memory"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			srv, cfg, st := newRegisterAgentFixture(t)
			cfg.Env.MCPAllowPrivilegedTools = tc.allow
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
				Name:         "shell-dyn",
				SystemPrompt: "p",
				Tools:        []string{"Read", "Bashbox", "Bash", "Write", "Edit", "Memory"},
			})
			if err != nil {
				t.Fatalf("RegisterAgent: %v", err)
			}
			if !slices.Equal(got.Tools, tc.want) {
				t.Errorf("returned tools = %v, want %v", got.Tools, tc.want)
			}
			def, ok := lookup.Agent(ctx, st, cfg, "", "shell-dyn")
			if !ok {
				t.Fatal("the registered agent does not resolve")
			}
			// The resolved definition also carries the tools every agent is
			// given, so it is asked only about the four in question.
			for _, name := range []string{"Bashbox", "Bash", "Write", "Edit"} {
				if slices.Contains(def.Tools, name) != tc.allow {
					t.Errorf("stored tools = %v: %s present = %v, want %v", def.Tools, name, !tc.allow, tc.allow)
				}
			}
		})
	}
}
