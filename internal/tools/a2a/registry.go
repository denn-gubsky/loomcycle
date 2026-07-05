package a2a

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// peerLister enumerates the A2A peers known at boot so RegisterTools can
// synthesise their tools. It is the narrow store surface the registry
// needs (the active-A2AAgentDef name list), declared here so main.go
// passes store.Store and tests pass a small fake.
type peerLister interface {
	A2AAgentDefListNames(ctx context.Context) ([]store.A2AAgentDefNameSummary, error)
}

// RegisterTools builds the synthetic outbound A2A tools an agent can
// invoke, one per (peer, expected_skill) pair, mirroring how static MCP
// servers register one `mcp__<server>__<tool>` tool per discovered
// remote tool at boot (see cmd/loomcycle/main.go's mcpPool init loop).
// The returned tools are appended to the global tool set and then
// filtered per-agent by `tools` exactly like MCP tools — an
// agent reaches an A2A peer only by listing `a2a__<peer>__<skill>` (or a
// `a2a__<peer>__*` glob) in its allowlist.
//
// Peer + skill identities come ONLY from operator-registered sources
// (yaml cfg.A2AAgents + active A2AAgentDef substrate rows), never from
// model text — the trust boundary that lets the model pick WHICH blessed
// peer/skill to call without being able to reach an arbitrary host.
//
// resolve is the per-call DefResolver each tool re-runs at Execute time
// (so a substrate fork of an already-registered peer is picked up
// without re-registering); newPeer is the client factory (nil ⇒ the
// production SDK factory); logf emits one triage line per skipped peer
// (no credentials/secrets — CLAUDE.md rule 4). Boot-time enumeration
// failures are logged and skipped, never fatal: a transient store error
// must not block loomcycle start.
func RegisterTools(ctx context.Context, cfg *config.Config, st peerLister, resolve DefResolver, newPeer peerClientFactory, logf func(string, ...any)) []tools.Tool {
	if cfg == nil {
		return nil
	}

	// Peer name set: yaml entries plus active-substrate names. A yaml
	// entry and a substrate def can share a name (yaml takes precedence
	// in the resolver, lookup.A2AAgent); dedup so we register one set of
	// tools per name.
	seen := make(map[string]bool)
	var peers []string
	for name := range cfg.A2AAgents {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		peers = append(peers, name)
	}
	if st != nil {
		names, err := st.A2AAgentDefListNames(ctx)
		if err != nil {
			trace(logf, "a2a tools: listing substrate peer defs failed (%v) — registering yaml peers only", err)
		} else {
			for _, n := range names {
				// Only names with an ACTIVE version are callable; a name
				// that only has draft/retired versions has no resolvable
				// def, so it would produce tools that always error.
				if n.Name == "" || n.ActiveDefID == "" || seen[n.Name] {
					continue
				}
				seen[n.Name] = true
				peers = append(peers, n.Name)
			}
		}
	}

	var out []tools.Tool
	for _, peer := range peers {
		def, ok := resolve(ctx, peer)
		if !ok {
			trace(logf, "a2a tools: peer %q could not be resolved at boot — skipping", peer)
			continue
		}
		if len(def.ExpectedSkills) == 0 {
			// A peer with no declared expected_skills has no skill to
			// target; the synthetic tool fronts exactly one (peer, skill)
			// pair. Skip with a triage line rather than guessing a skill.
			trace(logf, "a2a tools: peer %q declares no expected_skills — no synthetic tool registered (add expected_skills to make it callable)", peer)
			continue
		}
		for _, skill := range def.ExpectedSkills {
			if skill.ID == "" {
				continue
			}
			desc := fmt.Sprintf("Call the %q skill on remote A2A peer %q.", skill.ID, peer)
			out = append(out, NewTool(peer, skill.ID, desc, resolve, newPeer, logf))
		}
	}
	return out
}

// trace is a nil-safe logf invocation, matching the helper in the
// server-side card package.
func trace(logf func(string, ...any), format string, args ...any) {
	if logf != nil {
		logf(format, args...)
	}
}
