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
	// toolInjectMaxTokens caps the TOTAL injected tool-result content. Separate
	// from the memory budget so the two families cannot starve each other by
	// prompt ORDER (see meminject.ExpandInput).
	//
	// Sized for the inventory it exists to carry: a broad agent holds ~20 builtins
	// plus its MCP tools and toolSummaryMaxDesc bounds each line, so ~60 tools
	// still fit. It is a backstop against an agent mounting hundreds of MCP tools,
	// not a routine trim.
	toolInjectMaxTokens = 1024

	// toolSummaryMaxDesc caps each tool's one-line summary. The full schemas are
	// ALREADY in the provider request's tools array — this text exists only to make
	// the model attend to them, so restating whole descriptions would double the
	// token cost of every request to say nothing new.
	toolSummaryMaxDesc = 120
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
	ct := &builtin.Context{Tools: mi.Tools, Cfg: s.cfg}
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
	return strings.TrimSpace(desc[:cut]) + "…"
}

// utf8Start reports whether b starts a UTF-8 sequence, so a hard cap never splits
// a multi-byte rune (a split rune renders as U+FFFD).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
