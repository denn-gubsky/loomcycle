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
	"log"
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
	// Tools is the run's ALREADY-RESOLVED tool list (allowedTools), for the
	// {{tool:Context.tools}} expander. Every call site resolves it before prompt
	// assembly, so this reuses that result rather than re-deriving it. Empty →
	// the placeholder renders to nothing.
	Tools []tools.Tool
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

	// inject_tool_guide implicitly appends the runtime-knowledge refs the agent did
	// not hand-place — mirroring core_blocks' implicit append. Done on a LOCAL copy
	// of the prompt so a flag-OFF agent stays byte-identical (AppendToolGuideRefs is
	// a no-op when the refs are already present, and is never called when off).
	promptSrc := agentDef.SystemPrompt
	if agentDef.InjectToolGuide {
		promptSrc = meminject.AppendToolGuideRefs(promptSrc)
	}

	forceRoots := agentDef.MemoryRoots == "force"
	suppressRoots := agentDef.MemoryRoots == "suppress"
	wantUserInfo := meminject.ReferencesVariant(promptSrc, meminject.VariantUserInfo)
	toolRefs := meminject.ReferencesToolRefs(promptSrc)
	docRefs := meminject.ReferencesDocRefs(promptSrc)
	// The WIDENED families are COLLECTED only for a def an operator wrote. The
	// expander's guard is what refuses them and says why; this is what stops a
	// def that may not use them from causing the store reads and the network
	// call regardless. Authorship rides on the def itself rather than on ctx
	// because ctx is not stamped yet on every assembly path — the sub-agent
	// path stamps it after this call — and a guard that reads an unstamped
	// value is a guard that fails open.
	var memRefs []meminject.MemoryRef
	var toolCalls []meminject.ToolCall
	if agentDef.OperatorAuthored {
		memRefs = meminject.ReferencesMemoryRefs(promptSrc, nil)
		toolCalls = meminject.ReferencesToolCalls(promptSrc, nil)
	}

	// Fast path: no core blocks, no placeholder of any family, no protocol, no
	// forced provisioning → return byte-identical with no store reads and no tool
	// dispatch. Keeps every non-memory agent exactly as before.
	//
	// The widened check is ReferencesWidened rather than the collected slices:
	// those are authorship-gated, so a non-operator def whose only placeholder
	// is a widened one would take the fast path and leave the placeholder in the
	// prompt as literal text. Refused and removed is the intended outcome.
	if len(blocks) == 0 && !meminject.References(promptSrc) && len(toolRefs) == 0 &&
		len(docRefs) == 0 && !meminject.ReferencesWidened(promptSrc) &&
		!agentDef.MemoryProtocol && !forceRoots {
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
	// tenant_info: gated on an actual reference for the same reason as user_info
	// and ontology — rendering it PROVISIONS the tenant's deployment-context
	// document, and a prompt that never mentions it must not create one.
	// suppressRoots covers it too: `memory_roots: suppress` means an agent wants no
	// root documents injected, and the tenant root is one.
	if !suppressRoots && meminject.ReferencesVariant(promptSrc, meminject.VariantTenantInfo) {
		if body := s.renderTenantInfo(ctx, mi); body != "" {
			sections[meminject.VariantTenantInfo] = body
		}
	}
	if body := s.renderSearchRequest(ctx, mi); body != "" {
		sections[meminject.VariantSearchRequest] = body
	}
	// ontology: gated on an actual reference, like user_info, because rendering it
	// PROVISIONS the tenant's ontology document. A prompt that never mentions the
	// ontology must not create one as a side effect of being assembled.
	if meminject.ReferencesVariant(promptSrc, meminject.VariantOntology) {
		if body := s.renderOntology(ctx, mi); body != "" {
			sections[meminject.VariantOntology] = body
		}
	}
	// consolidation_bands is pure config — no store read, no tenant scope, so
	// it is rendered unconditionally and costs nothing for a prompt that never
	// places the placeholder (Expand only substitutes what it finds, and this
	// variant has no implicit-append path). A Server built without config
	// (tests) still renders the defaults rather than panicking.
	mergeBand, relatedBand := float64(config.DefaultConsolidationMergeThreshold), float64(config.DefaultConsolidationRelatedThreshold)
	if s.cfg() != nil {
		mergeBand = s.cfg().Memory.Consolidation.EffectiveMergeThreshold()
		relatedBand = s.cfg().Memory.Consolidation.EffectiveRelatedThreshold()
	}
	sections[meminject.VariantConsolidationBands] = meminject.ConsolidationBands(mergeBand, relatedBand)

	out := agentDef
	expanded, refused := meminject.ExpandWithRefusals(promptSrc, meminject.ExpandInput{
		Sections:      sections,
		ToolResults:   s.renderToolResults(ctx, mi, toolRefs),
		Documents:     s.renderDocuments(ctx, mi, docRefs),
		MemoryRefs:    s.renderMemoryRefs(ctx, mi, memRefs),
		ToolCalls:     s.renderToolCalls(ctx, mi, toolCalls),
		MaxTokens:     maxTokens,
		ToolMaxTokens: toolInjectMaxTokens,
		// The widened families and their network bound. OperatorAuthored FALSE
		// is the safe direction: a legacy row that predates the column reads as
		// not-operator-authored and simply cannot use what this added, while
		// every family it already used keeps working.
		OperatorAuthored: agentDef.OperatorAuthored,
		HostAllowed:      s.staticHostAllowed,
	})
	out.SystemPrompt = expanded
	noteRefusedValues(mi.AgentName, refused)

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
		scopeID, ok := coreBlockScopeID(blk.Scope, mi)
		if !ok {
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
// renderDocuments resolves each {{document:REF}} to its markdown.
//
// Read UNDER THE RUN'S OWN AUTHORITY: the Document tool is dispatched on the
// same ctx every other renderer here uses, so the substrate applies the same
// scope fold the agent's own Document tool would. A ref naming a document this
// run cannot read renders NOTHING — the family adds reach, not authority.
//
// A miss of any kind (absent, unreadable, empty, malformed) renders to nothing
// rather than erroring. Prompt assembly runs at every run-entry, sub-agent
// spawn and resume, and a run must not fail because a document moved.
func (s *Server) renderDocuments(ctx context.Context, mi memInject, refs []meminject.DocRef) map[meminject.DocRef]string {
	if len(refs) == 0 || s.store == nil || s.sqlMem == nil {
		return nil
	}
	dctx := s.docToolCtx(ctx, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	out := make(map[meminject.DocRef]string, len(refs))
	for _, ref := range refs {
		if ref.IsWholeDocument() {
			// Renders an instruction, not content — no read, so it is not
			// resolved here at all.
			continue
		}
		if body := resolveDocRef(dctx, doc, ref); body != "" {
			out[ref] = body
		}
	}
	return out
}

// resolveDocRef reads one ref, in the three forms the family addresses:
//
//	/path            a document — inlined whole IF it fits, else its outline
//	/path#Section    one section, inlined complete
//	<chunk-id>       one chunk, inlined complete
//
// NOTHING IS EVER TRUNCATED. A precise ref yields a complete unit; an imprecise
// one that does not fit yields a REFERENCE (title, ref, section list) rather
// than a cut body. A visible truncation marker helps a human reading the
// resolved prompt, but the model still answers confidently from half a spec —
// and a half-spec is indistinguishable from a complete one to it. An outline is
// an accurate map instead of a confident half-answer.
func resolveDocRef(ctx context.Context, doc *builtin.Document, ref meminject.DocRef) string {
	if ref.IsID() {
		if body, ok := readChunkByID(ctx, doc, ref.Path); ok {
			return body
		}
		// Not a chunk id — fall through and try it as a document id.
	}
	md, _ := exportDocument(ctx, doc, ref)
	if md == "" {
		return ""
	}
	// A named section is inlined COMPLETE, whatever its size: the operator asked
	// for exactly this part, and cutting the thing they narrowed to would defeat
	// narrowing. The shared memory budget is still the backstop for the total.
	return sectionByHeading(md, ref.Heading)
}

// readChunkByID inlines one chunk. ok=false when the id names no chunk, so the
// caller can try the same token as a document id.
func readChunkByID(ctx context.Context, doc *builtin.Document, id string) (string, bool) {
	req, err := json.Marshal(map[string]any{"op": "get_chunk", "scope": "user", "id": id})
	if err != nil {
		return "", false
	}
	res, _ := doc.Execute(ctx, req)
	if res.IsError {
		return "", false
	}
	var payload struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		return "", false
	}
	body := strings.TrimSpace(payload.Body)
	if body == "" {
		return "", false
	}
	if payload.Title != "" {
		body = "## " + payload.Title + "\n\n" + body
	}
	return body, true
}

// exportDocument renders a document to markdown, addressed by path or id.
func exportDocument(ctx context.Context, doc *builtin.Document, ref meminject.DocRef) (md, title string) {
	arg := map[string]any{"op": "export_md", "scope": "user", "include_metadata": false}
	if ref.IsID() {
		arg["id"] = ref.Path
	} else {
		arg["path"] = ref.Path
	}
	req, err := json.Marshal(arg)
	if err != nil {
		return "", ""
	}
	res, _ := doc.Execute(ctx, req)
	if res.IsError {
		return "", ""
	}
	var payload struct {
		Markdown string `json:"markdown"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		return "", ""
	}
	return strings.TrimSpace(payload.Markdown), payload.Title
}

func headingsOf(md string) []string {
	var out []string
	for _, ln := range strings.Split(md, "\n") {
		if _, title, ok := atxHeading(ln); ok {
			out = append(out, title)
		}
	}
	return out
}

// sectionByHeading returns the markdown section under the ATX heading whose text
// matches name (case-insensitive), up to the next heading of the same or higher
// level. Returns "" when no heading matches — which renders nothing, the same as
// any other miss.
func sectionByHeading(md, name string) string {
	lines := strings.Split(md, "\n")
	start, level := -1, 0
	for i, ln := range lines {
		lv, title, ok := atxHeading(ln)
		if !ok {
			continue
		}
		if start == -1 && strings.EqualFold(title, name) {
			start, level = i, lv
			continue
		}
		if start != -1 && lv <= level {
			return strings.TrimSpace(strings.Join(lines[start:i], "\n"))
		}
	}
	if start == -1 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:], "\n"))
}

// atxHeading parses an ATX markdown heading line ("## Title"), returning its
// level and title.
func atxHeading(line string) (int, string, bool) {
	t := strings.TrimSpace(line)
	lv := 0
	for lv < len(t) && t[lv] == '#' {
		lv++
	}
	if lv == 0 || lv > 6 || lv >= len(t) || t[lv] != ' ' {
		return 0, "", false
	}
	return lv, strings.TrimSpace(t[lv:]), true
}

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
	entries, err := s.store.MemoryFullTextSearch(ctx, mi.Tenant, store.MemoryScopeUser, mi.UserID, store.MemorySearchFilter{}, mi.InitialInput, topK)
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

// coreBlockScopeID maps a block scope to its server-sourced scope_id, and reports
// whether the block is RESOLVABLE at all.
//
// The second return is the whole point. An empty scope_id means two different
// things depending on the scope, and collapsing them silently dropped every
// tenant-scope block: for agent/user it means the identity is missing (no agent
// name on the run, no user_id) and the block genuinely cannot be located; for
// TENANT it is the correct value, because the k/v plane carries tenant identity in
// the tenant_id COLUMN and leaves scope_id empty. SQL Memory is the opposite — it
// refuses an empty scope id, since that string becomes half a schema name and a
// database role — and that asymmetry is deliberate on both sides.
//
// So `tenant` was accepted by validCoreBlockScopes, mapped correctly here, and then
// discarded by a caller that read "" as "unresolvable". The deliverable looked
// unbuilt when it was one boolean away.
func coreBlockScopeID(scope string, mi memInject) (string, bool) {
	switch scope {
	case "agent":
		return mi.AgentName, mi.AgentName != ""
	case "user":
		return mi.UserID, mi.UserID != ""
	case "tenant":
		// Empty BY CONVENTION, and resolvable. Do not "fix" this to mi.Tenant:
		// that is the SQL-Memory convention and would key these rows somewhere
		// no reader looks.
		return "", true
	default:
		return "", false
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

// expandCallerSegments resolves the placeholder families and ${...} variables in
// a CALLER-supplied system segment and user prompt — today, a team state's
// per-node prompt and input template.
//
// WHY IT EXISTS. applyMemoryInjection expands the AgentDef's own system prompt
// and nothing else, so until this a {{document:...}} written into a team node's
// prompt was inert: it reached the model as literal text. The workflow bindings
// the team design is built on did not resolve at all.
//
// Both strings are expanded in ONE call each, through the same single-pass
// expander, with values supplied rather than pre-substituted — so a variable
// cannot introduce a placeholder and a placeholder body cannot introduce a
// variable. Refs are collected from BOTH strings and their bodies rendered once,
// so a document referenced by the system prompt and the input costs one read.
//
// Returns the inputs unchanged when there is nothing to do, which is every
// non-team run: no caller segment, no values, no refs.
func (s *Server) expandCallerSegments(ctx context.Context, mi memInject, values map[string]string, system, user string, operatorAuthored bool) (string, string) {
	if system == "" && user == "" {
		return system, user
	}
	combined := system + "\n" + user
	docRefs := meminject.ReferencesDocRefs(combined)
	toolRefs := meminject.ReferencesToolRefs(combined)
	// The WIDENED families, collected only for a team an operator wrote — the
	// same two-cost gate the agent-prompt path uses: collecting nothing means a
	// def that may not use them causes neither the store reads nor the fetch.
	var memRefs []meminject.MemoryRef
	var toolCalls []meminject.ToolCall
	if operatorAuthored {
		memRefs = meminject.ReferencesMemoryRefs(combined, values)
		toolCalls = meminject.ReferencesToolCalls(combined, values)
	}
	// ReferencesWidened is checked here for the same reason the agent-prompt
	// fast path checks it: a segment whose ONLY placeholder is a widened one has
	// no other reason to enter expansion, and skipping leaves it sitting in the
	// prompt as literal text. Refused and removed is the intended outcome — and
	// on this path EVERY widened reference is refused (see OperatorAuthored
	// below), so this is the only thing that makes the refusal visible at all.
	if len(values) == 0 && len(docRefs) == 0 && len(toolRefs) == 0 &&
		!meminject.References(combined) && !meminject.ReferencesWidened(combined) {
		return system, user
	}

	in := meminject.ExpandInput{
		Values:        values,
		Documents:     s.renderDocuments(ctx, mi, docRefs),
		ToolResults:   s.renderToolResults(ctx, mi, toolRefs),
		MaxTokens:     config.DefaultMemoryInjectMaxTokens,
		ToolMaxTokens: toolInjectMaxTokens,
		MemoryRefs:    s.renderMemoryRefs(ctx, mi, memRefs),
		ToolCalls:     s.renderToolCalls(ctx, mi, toolCalls),
		// The TEAM DEFINITION's authorship, supplied by the caller — NOT the
		// spawned agent's, and not read from ctx.
		//
		// A caller segment is a TEAM NODE's prompt, so the question the guard
		// asks is who wrote that template. Gating on the spawned agent's flag
		// would be wrong in the direction that matters: an operator-authored
		// agent invoked from a model-authored node would pass a guard whose
		// whole subject is the node. The two authors are independent, and the
		// flag travels with the prompt for exactly that reason.
		//
		// FALSE for every non-team caller — there is no template to author —
		// which is also the safe direction for a seam nobody has wired yet.
		OperatorAuthored: operatorAuthored,
		HostAllowed:      s.staticHostAllowed,
	}
	// Deliberately NO Sections: the {{memory:...}} variants render an agent's
	// own accumulated memory, which belongs to the AgentDef's prompt. A caller
	// segment naming one renders nothing rather than injecting a different
	// agent's material into a prompt the agent did not author.
	outSystem, sysRefused := meminject.ExpandWithRefusals(system, in)
	outUser, userRefused := meminject.ExpandWithRefusals(user, in)
	noteRefusedValues(mi.AgentName, append(sysRefused, userRefused...))
	return outSystem, outUser
}

// noteRefusedValues surfaces what prompt assembly REFUSED. A security event,
// not a formatting quirk, so it is logged rather than swallowed.
//
// Each entry names the reference, and never the resolved value — the value is
// the untrusted part. The ONE deliberate exception is a trust-rule-5d host: a
// refusal an operator cannot act on is a refusal they will disable, and the
// host has already survived the charset check and the URL grammar.
func noteRefusedValues(agent string, refused []string) {
	if len(refused) == 0 {
		return
	}
	log.Printf("prompt: agent %q: refused %v — a resolved value contained {{ or }} or left its argument's "+
		"charset, a widened family was named by a def no operator authored, or a network target was not on "+
		"the operator's static http_host_allowlist",
		agent, refused)
}

// renderMemoryRefs resolves the widened {{memory:key|search:…}} references.
//
// UNDER THE RUN'S OWN SCOPE, exactly like the other families: the same
// (tenant, user) the agent's own Memory tool would read. The family adds REACH
// — content lands in the prompt without a tool call — and no authority. What
// the authorship guard gates is who may use that reach, not how far it goes.
//
// A ref that resolves to nothing is simply absent from the map, and the
// expander renders it as nothing. Prompt assembly runs at every run entry,
// sub-agent spawn and resume, so a deleted key must never fail a run.
func (s *Server) renderMemoryRefs(ctx context.Context, mi memInject, refs []meminject.MemoryRef) map[meminject.MemoryRef]string {
	if len(refs) == 0 || s.store == nil || mi.UserID == "" {
		return nil
	}
	out := make(map[meminject.MemoryRef]string, len(refs))
	for _, ref := range refs {
		switch ref.Kind {
		case meminject.MemoryKindKey:
			entry, err := s.store.MemoryGet(ctx, mi.Tenant, store.MemoryScopeUser, mi.UserID, ref.Arg)
			if err != nil {
				continue
			}
			if body := strings.TrimSpace(renderMemoryValue(entry.Value)); body != "" {
				out[ref] = body
			}
		case meminject.MemoryKindSearch:
			const topK = 5
			entries, err := s.store.MemoryFullTextSearch(ctx, mi.Tenant, store.MemoryScopeUser, mi.UserID,
				store.MemorySearchFilter{}, ref.Arg, topK)
			if err != nil || len(entries) == 0 {
				continue
			}
			var b strings.Builder
			for _, e := range entries {
				// Core blocks already arrive via {{memory:core_blocks}}; a query
				// that matches one must not render it twice.
				if strings.HasPrefix(e.Key, meminject.CoreBlockKeyPrefix) {
					continue
				}
				fmt.Fprintf(&b, "- %s: %s\n", e.Key, renderMemoryValue(e.Value))
			}
			if body := strings.TrimSpace(b.String()); body != "" {
				out[ref] = body
			}
		}
	}
	return out
}
