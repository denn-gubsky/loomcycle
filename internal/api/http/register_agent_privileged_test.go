package http

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// A registered agent must not get a shell (Workspace) or file writes by
// naming them: they are stripped unless the operator let registered agents
// keep privileged tools. (Bash and Bashbox were removed in v1.172.0 and a
// registration naming one is refused: TestServer_RegisterAgent_NamingARemovedItemIsRefused.)
func TestRegisterAgent_StripsPrivilegedToolsUnlessAllowed(t *testing.T) {
	for _, tc := range []struct {
		label string
		allow bool
		want  []string
	}{
		{"not allowed", false, []string{"Read", "Memory"}},
		{"allowed", true, []string{"Read", "Write", "Edit", "Workspace", "Memory"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			srv, cfg, st := newRegisterAgentFixture(t)
			cfg.Env.MCPAllowPrivilegedTools = tc.allow
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := srv.RegisterAgent(ctx, connector.RegisterAgentRequest{
				Name:         "shell-dyn",
				SystemPrompt: "p",
				Tools:        []string{"Read", "Write", "Edit", "Workspace", "Memory"},
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
			// given, so it is asked only about the ones in question.
			for _, name := range []string{"Write", "Edit", "Workspace"} {
				if slices.Contains(def.Tools, name) != tc.allow {
					t.Errorf("stored tools = %v: %s present = %v, want %v", def.Tools, name, !tc.allow, tc.allow)
				}
			}
		})
	}
}
