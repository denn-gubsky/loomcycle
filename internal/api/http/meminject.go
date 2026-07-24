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
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// user_info composition budget knobs (RFC BL P1 PR4). See composeUserInfo.
const (
	// userRootReservationPct is the operator user-root document's first-claim
	// share of the user_info content budget; the `human` core block fills the
	// remainder, and either may borrow the other's unused headroom.
	userRootReservationPct = 60
	// userInfoStructuralHeadroom reserves bytes for the sub-section labels +
	// truncation markers so the assembled user_info section stays under the
	// meminject.Expand token budget (which frames + budgets only CONTENT), and
	// Expand never rune-truncates the line-truncated body back into a mid-line
	// cut. Deliberately generous; only the memory CONTENT competes for the rest.
	userInfoStructuralHeadroom = 160
	// userInfoTruncMarker flags a boundary-aware truncation (whole trailing
	// lines dropped, never mid-line). Structural, not counted against the budget.
	userInfoTruncMarker = "…(truncated)"
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

	forceRoots := agentDef.MemoryRoots == "force"
	suppressRoots := agentDef.MemoryRoots == "suppress"
	wantUserInfo := meminject.ReferencesVariant(agentDef.SystemPrompt, meminject.VariantUserInfo)

	// Fast path: no core blocks, no placeholder, no protocol, no forced
	// provisioning → return byte-identical with no store reads. Keeps every
	// non-memory agent exactly as before.
	if len(blocks) == 0 && !meminject.References(agentDef.SystemPrompt) && !agentDef.MemoryProtocol && !forceRoots {
		return agentDef, blocks
	}

	maxTokens := agentDef.MemoryInjectMaxTokens
	if maxTokens <= 0 {
		maxTokens = config.DefaultMemoryInjectMaxTokens
	}
	maxBytes := maxTokens * 4 // matches meminject.Expand's chars/4 estimator

	// force: pre-provision the user-root document even with no {{memory:user_info}}
	// reference, so the operator can fill it in before the first run that uses it.
	if forceRoots {
		s.ensureUserRootDoc(ctx, mi)
	}

	sections := make(map[meminject.Variant]string)
	if body := s.renderCoreBlocks(ctx, mi, blocks); body != "" {
		sections[meminject.VariantCoreBlocks] = body
	}
	// user_info rendering lazily PROVISIONS the user-root doc — gate it on an
	// actual reference so a prompt that never mentions user_info never creates it.
	if wantUserInfo {
		if body := s.renderUserInfo(ctx, mi, maxBytes, suppressRoots); body != "" {
			sections[meminject.VariantUserInfo] = body
		}
	}
	if body := s.renderSearchRequest(ctx, mi); body != "" {
		sections[meminject.VariantSearchRequest] = body
	}
	// tenant_info / ontology are accepted variants but resolve to empty in P1
	// (they need tenant scope + the entity tier — a later phase).

	out := agentDef
	out.SystemPrompt = meminject.Expand(agentDef.SystemPrompt, sections, maxTokens)

	// Prepend the runtime-authored memory protocol in a region ABOVE any
	// {{memory:...}} DATA blocks. It is trusted guidance (how to USE memory), so
	// it is not DATA-framed. Deterministic → prompt-cache stable.
	if agentDef.MemoryProtocol {
		idx := agentDef.MemoryIndexMaxBytes
		if idx <= 0 {
			idx = config.DefaultMemoryIndexMaxBytes
		}
		protocol := meminject.MemoryProtocol(idx)
		if out.SystemPrompt != "" {
			out.SystemPrompt = protocol + "\n\n" + out.SystemPrompt
		} else {
			out.SystemPrompt = protocol
		}
	}
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

// renderUserInfo composes the {{memory:user_info}} section: the operator-authored
// user-root DOCUMENT first, then the consolidated user-scope `human` core block,
// each in its own labeled sub-section (the whole thing is DATA-framed by
// meminject.Expand). The operator doc gets first claim on the content budget
// (userRootReservationPct); the block fills the remainder; either borrows the
// other's unused headroom; truncation is boundary-aware. Lazily provisions the
// user-root doc on this first reference unless suppressed. maxBytes is the total
// injection byte budget; the returned content is bounded by it (Expand re-caps
// the grand total).
func (s *Server) renderUserInfo(ctx context.Context, mi memInject, maxBytes int, suppress bool) string {
	if s.store == nil || mi.UserID == "" {
		return ""
	}
	if !suppress {
		s.ensureUserRootDoc(ctx, mi) // lazy-on-first-reference; serialized per principal
	}
	docMD := s.readUserRootMarkdown(ctx, mi)                                   // "" when unavailable
	humanVal, _ := s.readCoreBlock(ctx, mi.Tenant, "user", mi.UserID, "human") // "" on miss
	budget := maxBytes - userInfoStructuralHeadroom
	if budget < 0 {
		budget = 0
	}
	return composeUserInfo(docMD, humanVal, budget)
}

// composeUserInfo lays out the operator user-root document + the `human` block
// into two labeled sub-sections under a shared content budget (bytes). The
// operator doc reserves userRootReservationPct of the budget; the block gets the
// rest; unused headroom flows to the other side (operator first). Each body is
// truncated at a LINE boundary with a marker — never mid-line. The sum of the
// surviving BODY bytes is <= budget (labels + marker are fixed presentation
// overhead, matching Expand's own "frame is not counted" rule). Pure +
// deterministic for prompt-cache stability.
func composeUserInfo(doc, human string, budget int) string {
	doc = strings.TrimSpace(doc)
	human = strings.TrimSpace(human)
	if doc == "" && human == "" {
		return ""
	}
	if budget <= 0 {
		return joinUserInfo(doc, human)
	}
	docFloor := budget * userRootReservationPct / 100
	humanFloor := budget - docFloor
	docUse := min(len(doc), docFloor)
	humanUse := min(len(human), humanFloor)
	// Redistribute unused headroom: the operator doc borrows first (it has the
	// first claim), then the human block borrows whatever remains.
	slack := budget - docUse - humanUse
	if slack > 0 && len(doc) > docUse {
		take := min(slack, len(doc)-docUse)
		docUse += take
		slack -= take
	}
	if slack > 0 && len(human) > humanUse {
		humanUse += min(slack, len(human)-humanUse)
	}
	return joinUserInfo(truncateAtLineBoundary(doc, docUse), truncateAtLineBoundary(human, humanUse))
}

// joinUserInfo assembles the (possibly-truncated) doc + block bodies into their
// labeled sub-sections, skipping an empty side.
func joinUserInfo(doc, human string) string {
	var b strings.Builder
	if doc != "" {
		b.WriteString("## Operator-authored user profile\n")
		b.WriteString(doc)
	}
	if human != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## Learned about the user\n")
		b.WriteString(human)
	}
	return b.String()
}

// truncateAtLineBoundary returns s cut to at most maxBytes bytes of body at a
// NEWLINE boundary — whole trailing lines are dropped, never a partial line —
// with userInfoTruncMarker appended when anything was dropped. A first line that
// alone exceeds the budget yields only the marker (never a mid-line fragment).
func truncateAtLineBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := strings.LastIndexByte(s[:maxBytes], '\n')
	if cut < 0 {
		return userInfoTruncMarker // first line already over budget → nothing fits
	}
	kept := strings.TrimRight(s[:cut], "\n")
	if kept == "" {
		return userInfoTruncMarker
	}
	return kept + "\n" + userInfoTruncMarker
}

// docToolCtx stamps the run's ALREADY-RESOLVED tenant + user (mi, server-sourced;
// never the wire) onto ctx as a RunIdentity, so the Document tool resolves its
// user scope to the SAME (tenant, user) the run uses. This reuses the resolved
// identity — it does NOT introduce a new tenant source — and is needed because
// WithRunIdentity is not stamped on ctx yet at prompt-assembly time.
func (s *Server) docToolCtx(ctx context.Context, mi memInject) context.Context {
	return tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: mi.UserID, TenantID: mi.Tenant})
}

// ensureUserRootDoc provisions the operator-authored user-root Document from the
// embedded template the first time it is referenced for a (tenant, user), via the
// SAME Document create path the Document tool uses (import_md). It closes two
// duplicate-creation races on the unsynchronized exists-check→create sequence,
// because createDocument always mints a FRESH doc id (the dirent upsert hides it),
// so two first-references that both miss the exists-check each import_md and leave
// a duplicate, orphaned, never-GC'd user-root Document:
//
//   - Same process — concurrent first-references for one (tenant,user) collapse
//     onto a single flight (userRootProvisionSF), so the exists-check + create run
//     exactly once and the rest share the result. The flight's key is evicted when
//     it returns, so nothing accumulates per principal (the former persistent memo
//     grew one entry per distinct principal forever). A repeat, NON-concurrent
//     reference is cheap and idempotent via the real exists-check below — one
//     indexed read, dwarfed by the per-run LLM call that gates this path.
//   - Cross replica — see provisionUserRootDoc: a PG advisory lock admits exactly
//     one replica to the exists-check + create; a replica that loses it skips this
//     pass (idempotent — the next reference finds the doc).
//
// Best-effort — SQL Memory / store absent, or any failure, degrades to "no doc
// rendered" (the run continues on the `human` block alone); nothing is cached, so
// a transient fault retries on the next reference.
func (s *Server) ensureUserRootDoc(ctx context.Context, mi memInject) {
	if s.store == nil || s.sqlMem == nil || mi.UserID == "" {
		return
	}
	// The NUL-joined cacheKey is a fine Go map key for the flight; it is NOT
	// reused for the PG advisory lock (Postgres text params reject NUL 0x00).
	cacheKey := userRootCacheKey(mi.Tenant, mi.UserID)
	_, _, _ = s.userRootProvisionSF.Do(cacheKey, func() (any, error) {
		s.provisionUserRootDoc(ctx, mi)
		return nil, nil
	})
}

// provisionUserRootDoc runs the exists-check + import_md create under the
// cross-replica advisory lock when clustered. Split out of ensureUserRootDoc so
// the singleflight closure stays a thin wrapper. Never returns an error —
// provisioning is best-effort (see ensureUserRootDoc).
func (s *Server) provisionUserRootDoc(ctx context.Context, mi memInject) {
	// Cross-replica dedup: in cluster mode hold the PG advisory lock so only one
	// replica runs the exists-check + create for this principal. A lost lock means
	// a peer is provisioning — skip (idempotent; the next reference finds the doc).
	// Nil (single-replica) → the in-process flight is the only guard.
	if s.sessionLockPG != nil {
		// NUL-free lock id: pg text params (hashtextextended) reject 0x00, so we do
		// NOT reuse the NUL-joined userRootCacheKey here. A '/' join can only alias
		// across principals whose (tenant,user) concatenate identically — harmless,
		// since a lost lock only skips this pass and the next reference retries
		// against the principal's own, isolated scope.
		release, ok := s.sessionLockPG.TryLock(ctx, "memory:userroot:"+mi.Tenant+"/"+mi.UserID)
		if !ok {
			return
		}
		defer release()
	}

	dctx := s.docToolCtx(ctx, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}

	// Exists-check: a get_document that succeeds means it was provisioned by an
	// earlier run (this process or another replica) — nothing to do.
	probe, _ := json.Marshal(map[string]any{"op": "get_document", "scope": "user", "path": meminject.UserRootPath})
	if res, _ := doc.Execute(dctx, probe); !res.IsError {
		return
	}

	create, _ := json.Marshal(map[string]any{
		"op": "import_md", "scope": "user", "path": meminject.UserRootPath,
		"markdown": meminject.UserRootTemplate(),
	})
	_, _ = doc.Execute(dctx, create) // best-effort; a failure retries next reference
}

// readUserRootMarkdown exports the user-root Document to clean Markdown for
// injection. Best-effort: no store / no SQL Memory / no such document → "".
func (s *Server) readUserRootMarkdown(ctx context.Context, mi memInject) string {
	if s.store == nil || s.sqlMem == nil || mi.UserID == "" {
		return ""
	}
	dctx := s.docToolCtx(ctx, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	req, _ := json.Marshal(map[string]any{
		"op": "export_md", "scope": "user", "path": meminject.UserRootPath, "include_metadata": false,
	})
	res, _ := doc.Execute(dctx, req)
	if res.IsError {
		return ""
	}
	var out struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Markdown)
}

// userRootCacheKey is the NUL-joined memoization key for a provisioned user-root
// document. Scope is always "user" in P1, so it is folded into the constant.
func userRootCacheKey(tenant, userID string) string {
	return "user\x00" + tenant + "\x00" + userID
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
