// toolinject.go — the SERVER side of the {{tool:<Tool>.<op>}} system-prompt
// expander. The pure allowlist half lives in internal/memory (meminject); this
// file dispatches each allowlisted ref through the REAL tool and projects its
// JSON into compact prompt text.
//
// Dispatching the real tool rather than re-deriving its answer is deliberate: a
// second implementation of "what tools does this agent have" would drift from
// Context op=tools, and the drift would be invisible — the prompt would claim one
// inventory while the tool reported another. It follows the precedent already set
// by readUserRootMarkdown, which calls the Document tool at assembly time.
//
// Trust: the injected body is framed as tool-result DATA, not instructions
// (meminject.frameToolResult). Every input is server-sourced — the ref comes from
// the operator's prompt, validated against a closed allowlist at boot, and the
// inventory comes from the run's already-resolved tool list. The model never
// chooses what is dispatched.
package http

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

const (
	// toolInjectMaxTokens caps the TOTAL injected tool-result content across ALL
	// {{tool:...}} refs a prompt names (inventory + guide + capabilities). It is
	// separate from the memory budget so the two FAMILIES cannot starve each other
	// by prompt ORDER (see meminject.ExpandInput); WITHIN the tool family the refs
	// still share it, consumed in prompt order (meminject.takeBudget).
	//
	// The shared total is generous enough to carry all three at once because each
	// renderer already self-bounds (toolSummaryMaxDesc / toolGuideMaxHint per line;
	// capabilities is a fixed feature list), and the bundles place the small
	// capabilities block BEFORE the larger guide so a truncation, if any, falls on
	// the tail of the guide rather than dropping capabilities wholesale. It is a
	// backstop against an agent mounting hundreds of MCP tools, not a routine trim.
	toolInjectMaxTokens = 4096

	// toolSummaryMaxDesc caps each tool's one-line summary. The full schemas are
	// ALREADY in the provider request's tools array — this text exists only to make
	// the model attend to them, so restating whole descriptions would double the
	// token cost of every request to say nothing new.
	toolSummaryMaxDesc = 120

	// toolGuideMaxHint caps each per-tool usage hint in the guide. Hints are
	// hand-written and already short; this is a backstop against a long one, and
	// keeps the guide's per-line cost predictable for the shared budget above.
	toolGuideMaxHint = 200
)

// renderToolResults dispatches each allowlisted ref the prompt names and returns
// the rendered body per ref. Best-effort per ref: a dispatch that fails or returns
// an unexpected shape yields no entry, so the placeholder renders to nothing and
// the run proceeds — a prompt-assembly fault must not fail a run.
func (s *Server) renderToolResults(ctx context.Context, mi memInject, refs []meminject.ToolRef) map[meminject.ToolRef]string {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[meminject.ToolRef]string, len(refs))
	for _, ref := range refs {
		if body := s.renderToolResult(ctx, mi, ref); body != "" {
			out[ref] = body
		}
	}
	return out
}

// renderToolResult dispatches ONE ref. An unhandled ref returns "" rather than
// panicking, so widening the allowlist without wiring a renderer degrades to
// "renders nothing" instead of taking down every run;
// TestAllowedToolRefs_AllHaveRenderer pins the pairing so the gap is caught in CI.
func (s *Server) renderToolResult(ctx context.Context, mi memInject, ref meminject.ToolRef) string {
	switch ref {
	case meminject.ToolRef{Tool: "Context", Op: "tools"}:
		return s.renderContextTools(ctx, mi)
	case meminject.ToolRef{Tool: "Context", Op: "guide"}:
		return s.renderContextGuide(ctx, mi)
	case meminject.ToolRef{Tool: "Context", Op: "capabilities"}:
		return s.renderContextCapabilities(ctx, mi)
	default:
		return ""
	}
}

// renderContextTools dispatches Context op=tools against the run's resolved tool
// list and renders the result as a compact one-line-per-tool inventory.
//
// mi.Tools is the run's ALREADY-RESOLVED allowedTools — the same slice the
// dispatcher and WithAgentTools get — so the rendered inventory is what the agent
// can actually call, not the server's whole registry. It is passed as both the
// candidate set and the ctx allowlist, which makes op=tools' intersection a no-op
// on an already-filtered list.
func (s *Server) renderContextTools(ctx context.Context, mi memInject) string {
	if len(mi.Tools) == 0 {
		return ""
	}
	tctx := tools.WithAgentTools(s.docToolCtx(ctx, mi), toolNames(mi.Tools))
	ct := &builtin.Context{Tools: mi.Tools, Cfg: s.cfg()}
	req, _ := json.Marshal(map[string]any{"op": "tools"})
	res, err := ct.Execute(tctx, req)
	if err != nil || res.IsError {
		return ""
	}
	// A WHITELIST by shape, matching the posture publicFeatures uses for
	// /v1/config: a new field appearing in Context op=tools does not silently
	// start landing in every agent's system prompt — it has to be added here.
	var parsed struct {
		Tools []struct {
			Name            string `json:"name"`
			Description     string `json:"description"`
			SideEffectClass string `json:"side_effect_class"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		return ""
	}
	lines := make([]string, 0, len(parsed.Tools))
	for _, t := range parsed.Tools {
		if t.Name == "" {
			continue
		}
		lines = append(lines, formatToolLine(t.Name, t.Description, t.SideEffectClass))
	}
	if len(lines) == 0 {
		return ""
	}
	// Sorted so the body is a pure function of the tool SET: the system prompt is
	// re-derived at run-start/resume and must stay byte-stable for provider
	// prompt-caching, and registry iteration order is not stable.
	sort.Strings(lines)
	return toolInventoryPreamble + "\n\n" + strings.Join(lines, "\n")
}

// renderContextGuide dispatches Context op=guide against the run's resolved tool
// list and renders the per-tool call digest (op enum + required args + usage
// hint) as one compact line per tool. Where op=tools answers "what can I call",
// the guide answers "how do I call it" — the high-signal subset a model needs to
// stop guessing op names and required fields.
//
// Same drift-safe posture as renderContextTools: it dispatches the REAL op=guide
// (so the digest cannot diverge from what the tool reports) and projects the
// result through a shape-whitelist, so a new field in the guide output does not
// silently start landing in every prompt.
func (s *Server) renderContextGuide(ctx context.Context, mi memInject) string {
	if len(mi.Tools) == 0 {
		return ""
	}
	tctx := tools.WithAgentTools(s.docToolCtx(ctx, mi), toolNames(mi.Tools))
	ct := &builtin.Context{Tools: mi.Tools, Cfg: s.cfg()}
	req, _ := json.Marshal(map[string]any{"op": "guide"})
	res, err := ct.Execute(tctx, req)
	if err != nil || res.IsError {
		return ""
	}
	var parsed struct {
		Tools []struct {
			Name            string   `json:"name"`
			SideEffectClass string   `json:"side_effect_class"`
			Ops             []string `json:"ops"`
			Required        []string `json:"required"`
			Hint            string   `json:"hint"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		return ""
	}
	lines := make([]string, 0, len(parsed.Tools))
	for _, t := range parsed.Tools {
		if t.Name == "" {
			continue
		}
		lines = append(lines, formatGuideLine(t.Name, t.SideEffectClass, t.Ops, t.Required, t.Hint))
	}
	if len(lines) == 0 {
		return ""
	}
	// Sorted for the same byte-stability reason as renderContextTools: the prompt
	// is re-derived at run-start/resume and must cache-match.
	sort.Strings(lines)
	return toolGuidePreamble + "\n\n" + strings.Join(lines, "\n")
}

// formatGuideLine renders `- Name (class): ops a|b|c; requires x, y — hint`. The
// op enum and required list come straight from the tool's own schema; the hint is
// the hand-written one-liner (empty for tools without one).
func formatGuideLine(name, class string, ops, required []string, hint string) string {
	line := "- " + name
	if class != "" && class != "unknown" {
		line += " (" + class + ")"
	}
	line += ":"
	if len(ops) > 0 {
		line += " ops " + strings.Join(ops, "|")
	}
	if len(required) > 0 {
		if len(ops) > 0 {
			line += ";"
		}
		line += " requires " + strings.Join(required, ", ")
	}
	if h := firstSentence(hint, toolGuideMaxHint); h != "" {
		line += " — " + h
	}
	return line
}

// toolGuidePreamble frames the digest as a call reference. It deliberately does
// NOT restate the full schemas (already in the request's tools array) — it points
// the model at the two things it most often gets wrong: the op and the required
// fields.
//
// HOUSE RULE: model-visible text — no internal RFC citations.
const toolGuidePreamble = "How to call your tools — the op to pass and the required arguments for each. " +
	"Use this to pick the right op and fields; the full schema for each tool is already available to you."

// renderContextCapabilities dispatches Context op=capabilities and renders the
// deployment's supported features as two short lists (available / not available)
// plus the numeric limits. It tells an agent what it can rely on BEFORE it tries
// — so it does not, for instance, attempt a document write on a deployment with
// no SQL Memory and discover it only from the refusal.
//
// op=capabilities is already secrets-free and topology-free by construction
// (enforced by tests in internal/capabilities + the tool), so its whole output is
// safe to bake into a prompt — unlike op=self. This renderer builds the fuller
// Context tool (Store/SqlMem/Embedder) the probe needs, which the server holds.
func (s *Server) renderContextCapabilities(ctx context.Context, mi memInject) string {
	tctx := tools.WithAgentTools(s.docToolCtx(ctx, mi), toolNames(mi.Tools))
	ct := &builtin.Context{
		Tools:    mi.Tools,
		Cfg:      s.cfg(),
		Store:    s.store,
		SqlMem:   s.sqlMem,
		Embedder: s.embedder,
	}
	req, _ := json.Marshal(map[string]any{"op": "capabilities"})
	res, err := ct.Execute(tctx, req)
	if err != nil || res.IsError {
		return ""
	}
	// The capabilities output is a flat map of feature → {available: bool, …} plus
	// a numeric `limits` map. Parse loosely (map[string]json.RawMessage) rather than
	// pinning each feature key: features come and go, and this renderer's contract
	// is "the available ones, by name" — a new feature should appear automatically,
	// while a non-{available:…} key (storage, limits) is handled explicitly below.
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.Text), &top); err != nil {
		return ""
	}
	var available, unavailable []string
	for key, raw := range top {
		var feat struct {
			Available *bool `json:"available"`
		}
		if err := json.Unmarshal(raw, &feat); err != nil || feat.Available == nil {
			continue // not a feature entry (limits, storage) — handled separately
		}
		if *feat.Available {
			available = append(available, key)
		} else {
			unavailable = append(unavailable, key)
		}
	}
	sort.Strings(available)
	sort.Strings(unavailable)

	var b strings.Builder
	b.WriteString(toolCapabilitiesPreamble)
	if len(available) > 0 {
		b.WriteString("\nAvailable: " + strings.Join(available, ", "))
	}
	if len(unavailable) > 0 {
		b.WriteString("\nNot available: " + strings.Join(unavailable, ", "))
	}
	if lim := formatCapabilityLimits(top["limits"]); lim != "" {
		b.WriteString("\nLimits: " + lim)
	}
	if len(available) == 0 && len(unavailable) == 0 {
		return "" // nothing worth injecting
	}
	return b.String()
}

// formatCapabilityLimits renders the numeric limits map as sorted `key=value`
// pairs. Values are numbers (JSON), rendered verbatim so an int stays an int.
func formatCapabilityLimits(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var lim map[string]json.Number
	if err := json.Unmarshal(raw, &lim); err != nil || len(lim) == 0 {
		return ""
	}
	keys := make([]string, 0, len(lim))
	for k := range lim {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+lim[k].String())
	}
	return strings.Join(parts, ", ")
}

// toolCapabilitiesPreamble frames the capability lists as deployment facts.
//
// HOUSE RULE: model-visible text — no internal RFC citations.
const toolCapabilitiesPreamble = "What this deployment supports right now — rely on the available features and do not attempt the unavailable ones."

// toolInventoryPreamble tells the model what the list IS and what to do with it.
// Without it the framed body is just names; the observed failure mode is not
// ignorance of the names but reluctance to act on them.
//
// HOUSE RULE: model-visible text — no internal RFC citations.
const toolInventoryPreamble = "These are the tools you can call right now. " +
	"Call one directly when a task needs it; do not ask whether a tool exists, and do not say you lack a capability listed here."

// formatToolLine renders `- Name (class) — summary`. The side-effect class is
// included because it is the cheapest signal separating a read from a mutation,
// which is what a model needs to decide whether to confirm before acting.
func formatToolLine(name, desc, class string) string {
	line := "- " + name
	if class != "" && class != "unknown" {
		line += " (" + class + ")"
	}
	if summary := firstSentence(desc, toolSummaryMaxDesc); summary != "" {
		line += " — " + summary
	}
	return line
}

// firstSentence reduces a tool description to its opening claim: the first
// non-empty LINE, cut at the first sentence end, then hard-capped at maxBytes on
// a rune boundary. Deterministic — same description, same bytes.
func firstSentence(desc string, maxBytes int) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	if i := strings.IndexByte(desc, '\n'); i >= 0 {
		desc = strings.TrimSpace(desc[:i])
	}
	// Cut at the first ". " rather than any '.', so "op=self." and "v1.2" survive.
	if i := strings.Index(desc, ". "); i >= 0 {
		desc = desc[:i+1]
	}
	if len(desc) <= maxBytes {
		return desc
	}
	cut := maxBytes
	for cut > 0 && !utf8Start(desc[cut]) {
		cut--
	}
	// Back off to a word boundary so the summary does not end mid-word ("body)
	// t…"). Only when a boundary is reasonably close — otherwise a description
	// with no spaces in range would collapse to almost nothing.
	if sp := strings.LastIndexByte(desc[:cut], ' '); sp > cut/2 {
		cut = sp
	}
	return strings.TrimSpace(desc[:cut]) + "…"
}

// utf8Start reports whether b starts a UTF-8 sequence, so a hard cap never splits
// a multi-byte rune (a split rune renders as U+FFFD).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
