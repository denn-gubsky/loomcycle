// meminject.go — RFC BL P1 core-memory-block + {{memory:...}} injection wiring.
//
// The pure closed-set expander lives in internal/memory (meminject). This file
// is the SERVER side: it resolves the run's effective core-block set (own +
// inherited), reads each block's value + the search_request retrieval from the
// store, and calls meminject.Expand to fold them into the agent's system
// prompt. It runs at every run-entry alongside resolveSkillBodiesForRun, so the
// injection is re-derived at fresh run / HTTP / continue / sub-agent / resume —
// and, because compaction only resets the MESSAGE list (never the system
// prompt), it survives a compaction rebuild without re-running.
//
// Trust: the injected content is framed as reference DATA, not instructions
// (meminject.frame). The tenant + scope are server-sourced (mi, never the
// wire); the model never chooses what memory is injected.
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memInject carries the run-entry identity the {{memory:...}} expander needs.
// Zero-value fields degrade gracefully (a variant with no resolvable data
// expands to nothing).
type memInject struct {
	Tenant       string
	UserID       string
	AgentName    string
	InitialInput string // the run's initial user text, for search_request
}

// applyMemoryInjection expands the agent's system prompt with its core memory
// blocks + the other {{memory:...}} variants, and returns (the possibly-rewritten
// agent def, the effective core-block set). The caller stamps the returned
// blocks onto the run ctx via tools.WithCoreBlocksPolicy so (a) the Memory tool
// can enforce read_only/limit_bytes and (b) an inherit_core_blocks sub-agent
// picks them up.
//
// ctx carries the PARENT run's core-block policy on the sub-agent path (empty
// for a top-level run), which is how inheritance flows.
func (s *Server) applyMemoryInjection(ctx context.Context, agentDef config.AgentDef, mi memInject) (config.AgentDef, []config.CoreBlock) {
	// The parent run's blocks ride on ctx (empty for a top-level run) — this is
	// the inheritance channel for an inherit_core_blocks sub-agent.
	parentInheritable := tools.CoreBlocksPolicy(ctx).Blocks
	blocks := effectiveCoreBlocks(agentDef.CoreBlocks, agentDef.InheritCoreBlocks, parentInheritable)

	// Fast path: no core blocks and no placeholder → return byte-identical, no
	// store reads. Keeps every non-memory agent exactly as before.
	if len(blocks) == 0 && !meminject.References(agentDef.SystemPrompt) {
		return agentDef, blocks
	}

	sections := make(map[meminject.Variant]string)
	if body := s.renderCoreBlocks(ctx, mi, blocks); body != "" {
		sections[meminject.VariantCoreBlocks] = body
	}
	if body := s.renderUserInfo(ctx, mi); body != "" {
		sections[meminject.VariantUserInfo] = body
	}
	if body := s.renderSearchRequest(ctx, mi); body != "" {
		sections[meminject.VariantSearchRequest] = body
	}
	// tenant_info / ontology are accepted variants but resolve to empty in P1
	// (they need tenant scope + the entity tier — a later phase). memory_protocol
	// has no variant yet; its protocol note lands with the consolidation tiers.

	maxTokens := agentDef.MemoryInjectMaxTokens
	if maxTokens <= 0 {
		maxTokens = config.DefaultMemoryInjectMaxTokens
	}

	out := agentDef
	out.SystemPrompt = meminject.Expand(agentDef.SystemPrompt, sections, maxTokens)
	return out, blocks
}

// effectiveCoreBlocks computes the core-block set that applies to a run: the
// agent's OWN declared blocks, plus — only when inherit is set — the parent
// run's user/tenant-scope blocks. Agent-scope parent blocks NEVER cross the
// spawn boundary (a sub-agent has its own agent identity + agent memory). The
// agent's own block wins on a (scope,label) collision. Pure + order-preserving
// so it is directly unit-testable.
func effectiveCoreBlocks(own []config.CoreBlock, inherit bool, parentInheritable []config.CoreBlock) []config.CoreBlock {
	out := make([]config.CoreBlock, 0, len(own)+len(parentInheritable))
	seen := make(map[string]bool, len(own)+len(parentInheritable))
	add := func(b config.CoreBlock) {
		k := b.Scope + "/" + b.Label
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, b)
	}
	for _, b := range own {
		add(b)
	}
	if inherit {
		for _, b := range parentInheritable {
			if b.Scope == "agent" {
				continue // agent-scope is private; never inherited
			}
			add(b)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// renderCoreBlocks reads each attached block's value and renders a labeled list.
// A block whose scope has no resolvable scope_id (tenant scope in P1, or a
// user/agent block with no id on the run) is skipped.
func (s *Server) renderCoreBlocks(ctx context.Context, mi memInject, blocks []config.CoreBlock) string {
	if s.store == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		scopeID := coreBlockScopeID(blk.Scope, mi)
		if scopeID == "" {
			continue
		}
		val, ok := s.readCoreBlock(ctx, mi.Tenant, blk.Scope, scopeID, blk.Label)
		if !ok || val == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", blk.Label, val)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderUserInfo renders the user-scope `human` core block (the user's durable
// profile). PR4 will prepend the user-root DOCUMENT here; for P3 it is the block.
func (s *Server) renderUserInfo(ctx context.Context, mi memInject) string {
	if s.store == nil || mi.UserID == "" {
		return ""
	}
	// PR4 seam: prepend the user-root document summary above the block.
	val, ok := s.readCoreBlock(ctx, mi.Tenant, "user", mi.UserID, "human")
	if !ok {
		return ""
	}
	return val
}

// renderSearchRequest runs an LLM-free retrieval against the run's initial user
// input over the user-scope memory, reusing RFC BL's full-text (lexical) leg
// directly on the store — it needs no embedder and takes the tenant explicitly
// (RunIdentity is not on ctx yet at prompt-assembly time). Blending the vector
// leg via RRF is a later-phase enrichment. Empty input / no store / no user →
// nothing; the token budget is applied by meminject.Expand.
func (s *Server) renderSearchRequest(ctx context.Context, mi memInject) string {
	if s.store == nil || mi.UserID == "" || strings.TrimSpace(mi.InitialInput) == "" {
		return ""
	}
	const topK = 5
	entries, err := s.store.MemoryFullTextSearch(ctx, mi.Tenant, store.MemoryScopeUser, mi.UserID, "", mi.InitialInput, topK)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		// Core blocks are already injected via core_blocks; skip them here so a
		// query that matches a block isn't rendered twice.
		if strings.HasPrefix(e.Key, meminject.CoreBlockKeyPrefix) {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", e.Key, renderMemoryValue(e.Value))
	}
	return strings.TrimRight(b.String(), "\n")
}

// readCoreBlock fetches core/<label> in (scope, scopeID) at the given tenant.
// Returns (value, true) on a hit, ("", false) on miss/error (best-effort: the
// run continues without the block rather than failing).
func (s *Server) readCoreBlock(ctx context.Context, tenant, scope, scopeID, label string) (string, bool) {
	entry, err := s.store.MemoryGet(ctx, tenant, store.MemoryScope(scope), scopeID, meminject.CoreBlockKeyPrefix+label)
	if err != nil {
		return "", false
	}
	return renderMemoryValue(entry.Value), true
}

// coreBlockScopeID maps a block scope to its server-sourced scope_id. tenant
// scope has no scope_id convention in P1 (the entity tier lands later) → "".
func coreBlockScopeID(scope string, mi memInject) string {
	switch scope {
	case "agent":
		return mi.AgentName
	case "user":
		return mi.UserID
	default: // tenant (P1) — not resolvable yet
		return ""
	}
}

// renderMemoryValue turns a stored JSON value into human-readable text: a JSON
// string is unquoted; any other JSON is rendered verbatim. Empty / null → "".
func renderMemoryValue(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return s
}

// firstUserText returns the concatenated text of the first user-role segment —
// the run's initial input for the search_request variant. "" when no user text.
func firstUserText(segs []loop.PromptSegment) string {
	for _, seg := range segs {
		if seg.Role != "user" {
			continue
		}
		var b strings.Builder
		for _, c := range seg.Content {
			if c.Text != "" {
				b.WriteString(c.Text)
				b.WriteString(" ")
			}
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			return t
		}
	}
	return ""
}
