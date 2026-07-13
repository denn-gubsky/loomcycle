package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// History is the RFC BE built-in tool: browse / search / annotate PAST chats.
// A "chat" is a session (session → runs → events); History gives it a human
// handle (title / description / tags / pin / archive) and lets an agent or an
// operator list, search, and read prior transcripts across owner scopes.
//
// It supersedes the removed Context op=history: that op was agent-relationship
// scoped (self/any), had no listing/search/annotation, and — with `any` — read
// cross-tenant transcripts flat. History replaces it with proper owner-scope
// axes (self / user / tenant / global) resolved SERVER-SIDE from ctx and a
// tenant-safe visibility fold on every by-id read.
//
// Two non-negotiable isolation invariants:
//
//  1. The target owner is resolved from tools.RunIdentity(ctx) / AgentName(ctx),
//     NEVER from the wire (mirrors the Memory tool's scope_id rule). A
//     model-supplied owner id would let one tenant's agent read another's chats.
//  2. The requested scope must be present in the ctx HistoryPolicy (default-deny
//     when empty). `global` is stripped from the policy for non-admin principals
//     at policy-resolution time (server.go historyPolicyForAgent /
//     grantOperatorPolicies), so the tool simply trusts policy.Scopes — a tenant
//     caller can never resolve `global` even if the yaml lists it.
//
// Redaction: transcripts are persisted ALREADY-redacted by the recording emit
// path (internal/api/http/server.go makeRecordingEmit scrubs tool_call inputs
// and tool_result text at write time), so `get` returns scrubbed content without
// re-applying the transform — the same posture the old Context op=history had.
type History struct {
	// Store is the persistence backend. Nil disables the tool entirely (every
	// call returns an is_error result with a clear "not configured" message —
	// operators see one failure rather than a panic). Late-bound in main.go.
	Store store.Store
}

func (h *History) Name() string { return "History" }

func (h *History) Description() string {
	return "Browse, search, and annotate PAST chats (a chat = a conversation session; session -> runs -> events). " +
		"Ops: list (chats in a scope, filtered + paginated, pinned-first), search (title match within a scope), " +
		"get (one chat's metadata + full transcript; format:markdown renders it), rename (set title), " +
		"annotate (set description and/or tags), pin (float to the top), archive (reversible soft-hide). " +
		"scope selects whose chats: self = this agent's, user = this end-user's, tenant = this tenant's, " +
		"global = all tenants (admin only). The owner is resolved server-side from the run identity, never the wire; " +
		"cross-scope reads fold to an opaque not-found. Per-chat token/cost/run-count stats are included. " +
		"See Context op=help topic=history for the scope model and examples."
}

// historyInputSchema is a package const so the LoomCycle MCP server can source
// the wrapper's advertised inputSchema verbatim (via MCPWrapperInputSchema)
// rather than restating it — the same pattern as memoryInputSchema.
const historyInputSchema = `{
	"type": "object",
	"properties": {
		"op":              {"type": "string", "enum": ["list","get","search","rename","annotate","pin","archive"]},
		"scope":           {"type": "string", "enum": ["self","user","tenant","global"], "description": "Whose chats: self = this agent's; user = this end-user's; tenant = this tenant's; global = all tenants (admin only). Default self. The owner id is resolved server-side from the run identity, never the wire."},
		"session_id":      {"type": "string", "description": "get/rename/annotate/pin/archive: the chat (session) id to target."},
		"status":          {"type": "string", "description": "list/search: filter by derived chat status (running/completed/failed/cancelled)."},
		"from":            {"type": "string", "description": "list/search: RFC3339 lower bound on last activity."},
		"to":              {"type": "string", "description": "list/search: RFC3339 upper bound on last activity."},
		"tag":             {"type": "string", "description": "list/search: return only chats carrying this exact tag."},
		"title_contains":  {"type": "string", "description": "list: case-insensitive substring match on the title."},
		"query":           {"type": "string", "description": "search: case-insensitive title match (metadata MVP; full-text content search is not yet available)."},
		"pinned_only":     {"type": "boolean", "description": "list/search: restrict to pinned chats."},
		"include_archived":{"type": "boolean", "description": "list/search: include archived chats (excluded by default)."},
		"limit":           {"type": "integer", "description": "list/search: max chats per page (default 50, cap 500)."},
		"offset":          {"type": "integer", "description": "list/search: pagination offset."},
		"format":          {"type": "string", "description": "get: \"markdown\" renders the transcript as Markdown instead of a structured event array."},
		"title":           {"type": "string", "description": "rename: the new title."},
		"description":     {"type": "string", "description": "annotate: the new description."},
		"tags":            {"type": "array", "items": {"type": "string"}, "description": "annotate: the new tag set (replaces the existing set)."},
		"pinned":          {"type": "boolean", "description": "pin: true pins (default), false unpins."},
		"archived":        {"type": "boolean", "description": "archive: true archives (default), false unarchives."}
	},
	"required": ["op"]
}`

func (h *History) InputSchema() json.RawMessage { return json.RawMessage(historyInputSchema) }

type historyInput struct {
	Op              string `json:"op"`
	Scope           string `json:"scope"`
	SessionID       string `json:"session_id"`
	Status          string `json:"status"`
	From            string `json:"from"`
	To              string `json:"to"`
	Tag             string `json:"tag"`
	TitleContains   string `json:"title_contains"`
	Query           string `json:"query"`
	PinnedOnly      bool   `json:"pinned_only"`
	IncludeArchived bool   `json:"include_archived"`
	Limit           int    `json:"limit"`
	Offset          int    `json:"offset"`
	Format          string `json:"format"`
	// Mutation fields — pointers so "absent" (nil) maps directly to
	// SessionMetaPatch's "leave unchanged" and an explicit empty string /
	// empty slice is a legitimate "clear it" value.
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Tags        *[]string `json:"tags"`
	Pinned      *bool     `json:"pinned"`
	Archived    *bool     `json:"archived"`
}

func (h *History) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if h.Store == nil {
		return errResult("History tool: not configured (no Store backend)"), nil
	}
	var in historyInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid input JSON: " + err.Error()), nil
	}
	if in.Op == "" {
		return errResult("missing required field: op"), nil
	}
	scope, err := h.authorizedScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}

	switch in.Op {
	case "list":
		return h.list(ctx, scope, in, false)
	case "search":
		return h.list(ctx, scope, in, true)
	case "get":
		return h.get(ctx, scope, in)
	case "rename":
		return h.rename(ctx, scope, in)
	case "annotate":
		return h.annotate(ctx, scope, in)
	case "pin":
		return h.pin(ctx, scope, in)
	case "archive":
		return h.archive(ctx, scope, in)
	default:
		return errResult(fmt.Sprintf("unknown op %q (want one of: list, get, search, rename, annotate, pin, archive)", in.Op)), nil
	}
}

// authorizedScope canonicalizes the requested scope (default self) and enforces
// the ctx HistoryPolicy gate (default-deny). Because policy resolution strips
// `global` for non-admin principals, membership in policy.Scopes is the whole
// authorization check — the tool needs no separate admin test.
func (h *History) authorizedScope(ctx context.Context, requested string) (string, error) {
	if requested == "" {
		requested = "self"
	}
	switch requested {
	case "self", "user", "tenant", "global":
	default:
		return "", fmt.Errorf("history: unknown scope %q (want one of: self, user, tenant, global)", requested)
	}
	pol := tools.HistoryPolicy(ctx)
	if !containsScope(pol.Scopes, requested) {
		if len(pol.Scopes) == 0 {
			return "", fmt.Errorf("history: no history_scope policy (default-deny); grant history_scope: [%s] on the agent to allow it", requested)
		}
		return "", fmt.Errorf("history: scope %q not permitted (allowed: %s)", requested, strings.Join(pol.Scopes, ", "))
	}
	return requested, nil
}

// filterForScope builds the owner-constrained SessionFilter from ctx identity.
// The owner id NEVER comes from the wire — only the scope selector does.
func (h *History) filterForScope(ctx context.Context, scope string, in historyInput) (store.SessionFilter, error) {
	ident := tools.RunIdentity(ctx)
	f := store.SessionFilter{
		Tag:             in.Tag,
		TitleContains:   in.TitleContains,
		IncludePinned:   in.PinnedOnly,
		IncludeArchived: in.IncludeArchived,
	}
	if in.Status != "" {
		f.Status = store.RunStatus(in.Status)
	}
	if in.From != "" {
		t, err := time.Parse(time.RFC3339, in.From)
		if err != nil {
			return store.SessionFilter{}, fmt.Errorf("history: from must be RFC3339: %v", err)
		}
		f.From = t
	}
	if in.To != "" {
		t, err := time.Parse(time.RFC3339, in.To)
		if err != nil {
			return store.SessionFilter{}, fmt.Errorf("history: to must be RFC3339: %v", err)
		}
		f.To = t
	}
	switch scope {
	case "self":
		f.AgentName = tools.AgentName(ctx)
		f.TenantID = ident.TenantID
	case "user":
		f.UserID = ident.UserID
		f.TenantID = ident.TenantID
	case "tenant":
		f.TenantID = ident.TenantID
	case "global":
		// No tenant filter — spans every tenant. Reachable only under an admin
		// principal (policy resolution dropped `global` otherwise).
	}
	return f, nil
}

func (h *History) list(ctx context.Context, scope string, in historyInput, isSearch bool) (tools.Result, error) {
	f, err := h.filterForScope(ctx, scope, in)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if isSearch {
		if strings.TrimSpace(in.Query) == "" {
			return errResult("history: search requires a non-empty query"), nil
		}
		// MVP metadata search: case-insensitive title match. Description/tags
		// full-text search is deferred (an FTS index, additive later).
		f.TitleContains = in.Query
	}
	rows, total, err := h.Store.ListSessions(ctx, f, in.Limit, in.Offset)
	if err != nil {
		return errResult("history: list: " + err.Error()), nil
	}
	return okJSON(map[string]any{
		"scope":  scope,
		"chats":  rows,
		"total":  total,
		"limit":  in.Limit,
		"offset": in.Offset,
	})
}

func (h *History) get(ctx context.Context, scope string, in historyInput) (tools.Result, error) {
	sess, err := h.loadSessionInScope(ctx, scope, "get", in.SessionID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	runs, err := h.Store.RunsForSession(ctx, sess.ID)
	if err != nil {
		return errResult("history: runs: " + err.Error()), nil
	}
	events, err := h.Store.GetTranscript(ctx, sess.ID)
	if err != nil {
		return errResult("history: transcript: " + err.Error()), nil
	}
	chat := sessionMeta(sess, runs)

	if in.Format == "markdown" {
		return okJSON(map[string]any{
			"scope":    scope,
			"chat":     chat,
			"markdown": renderTranscriptMarkdown(sess, chat, events),
		})
	}
	return okJSON(map[string]any{
		"scope":      scope,
		"chat":       chat,
		"transcript": transcriptEvents(events),
	})
}

func (h *History) rename(ctx context.Context, scope string, in historyInput) (tools.Result, error) {
	if in.Title == nil {
		return errResult("history: rename requires title"), nil
	}
	return h.applyMeta(ctx, scope, in.SessionID, "rename", store.SessionMetaPatch{Title: in.Title})
}

func (h *History) annotate(ctx context.Context, scope string, in historyInput) (tools.Result, error) {
	if in.Description == nil && in.Tags == nil {
		return errResult("history: annotate requires description and/or tags"), nil
	}
	return h.applyMeta(ctx, scope, in.SessionID, "annotate", store.SessionMetaPatch{
		Description: in.Description,
		Tags:        in.Tags,
	})
}

func (h *History) pin(ctx context.Context, scope string, in historyInput) (tools.Result, error) {
	pinned := true // op=pin defaults to pinning; pass pinned:false to unpin.
	if in.Pinned != nil {
		pinned = *in.Pinned
	}
	return h.applyMeta(ctx, scope, in.SessionID, "pin", store.SessionMetaPatch{Pinned: &pinned})
}

func (h *History) archive(ctx context.Context, scope string, in historyInput) (tools.Result, error) {
	archived := true // op=archive defaults to archiving; pass archived:false to restore.
	if in.Archived != nil {
		archived = *in.Archived
	}
	return h.applyMeta(ctx, scope, in.SessionID, "archive", store.SessionMetaPatch{Archived: &archived})
}

// applyMeta enforces the scope fold, writes the patch, then returns the updated
// chat metadata. The fold runs BEFORE the write so a cross-scope session_id is
// never mutated (and is reported as an opaque not-found).
func (h *History) applyMeta(ctx context.Context, scope, sessionID, op string, patch store.SessionMetaPatch) (tools.Result, error) {
	sess, err := h.loadSessionInScope(ctx, scope, op, sessionID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := h.Store.SetSessionMeta(ctx, sess.ID, patch); err != nil {
		return errResult("history: " + op + ": " + err.Error()), nil
	}
	updated, err := h.Store.GetSession(ctx, sess.ID)
	if err != nil {
		return errResult("history: reload: " + err.Error()), nil
	}
	runs, _ := h.Store.RunsForSession(ctx, sess.ID)
	return okJSON(map[string]any{
		"scope": scope,
		"chat":  sessionMeta(updated, runs),
	})
}

// loadSessionInScope fetches a session and enforces the resolved scope's owner
// constraint. A missing session AND a session outside the caller's scope both
// return the SAME opaque not-found message — the fold must never become a
// cross-tenant existence oracle (session_ids are not secret).
func (h *History) loadSessionInScope(ctx context.Context, scope, op, sessionID string) (store.Session, error) {
	if sessionID == "" {
		return store.Session{}, fmt.Errorf("history: %s requires session_id", op)
	}
	notFound := fmt.Errorf("history: chat %q not found", sessionID)
	sess, err := h.Store.GetSession(ctx, sessionID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return store.Session{}, notFound
		}
		return store.Session{}, fmt.Errorf("history: %s: %v", op, err)
	}
	ident := tools.RunIdentity(ctx)
	visible := false
	switch scope {
	case "global":
		visible = true // admin-only per policy resolution.
	case "tenant":
		visible = sess.TenantID == ident.TenantID
	case "user":
		visible = sess.TenantID == ident.TenantID && sess.UserID == ident.UserID
	case "self":
		visible = sess.TenantID == ident.TenantID && sess.Agent == tools.AgentName(ctx)
	}
	if !visible {
		return store.Session{}, notFound
	}
	return sess, nil
}

// containsScope reports membership in the granted scope list.
func containsScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// chatMeta is the metadata view returned by get / rename / annotate / pin /
// archive — the session's human handle plus the RFC AV token/cost/run-count
// aggregates summed from its runs.
type chatMeta struct {
	SessionID    string    `json:"session_id"`
	TenantID     string    `json:"tenant_id,omitempty"`
	Agent        string    `json:"agent,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Title        string    `json:"title,omitempty"`
	Description  string    `json:"description,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
	Archived     bool      `json:"archived,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	RunCount     int       `json:"run_count"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	Cost         float64   `json:"cost"`
}

// sessionMeta rolls a session row + its runs into the get/mutation view. The
// aggregate definition (SUM tokens/cost, COUNT runs) matches ListSessions so a
// get and a list of the same chat agree.
func sessionMeta(sess store.Session, runs []store.Run) chatMeta {
	m := chatMeta{
		SessionID:   sess.ID,
		TenantID:    sess.TenantID,
		Agent:       sess.Agent,
		UserID:      sess.UserID,
		CreatedAt:   sess.CreatedAt,
		Title:       sess.Title,
		Description: sess.Description,
		Tags:        sess.Tags,
		Pinned:      sess.Pinned,
		Archived:    !sess.ArchivedAt.IsZero(),
		Summary:     sess.Summary,
		RunCount:    len(runs),
	}
	for _, r := range runs {
		m.InputTokens += int64(r.InputTokens)
		m.OutputTokens += int64(r.OutputTokens)
		if r.Cost != nil {
			m.Cost += *r.Cost
		}
	}
	return m
}

// transcriptEvent is the structured per-event shape returned by get (mirrors
// the shape the removed Context op=history returned so consumers migrate 1:1).
type transcriptEvent struct {
	Seq       int64           `json:"seq"`
	RunID     string          `json:"run_id"`
	Timestamp string          `json:"ts"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func transcriptEvents(events []store.Event) []transcriptEvent {
	out := make([]transcriptEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, transcriptEvent{
			Seq:       ev.Seq,
			RunID:     ev.RunID,
			Timestamp: ev.Timestamp.UTC().Format(time.RFC3339Nano),
			Type:      ev.Type,
			Payload:   json.RawMessage(ev.Payload),
		})
	}
	return out
}

// renderTranscriptMarkdown produces a human-facing Markdown export of a chat:
// a metadata header followed by one section per event. Content is pulled from
// the persisted (already-redacted) event payload; a payload with no `text`
// field falls back to its compact JSON so nothing is silently lost.
func renderTranscriptMarkdown(sess store.Session, meta chatMeta, events []store.Event) string {
	var b strings.Builder
	title := sess.Title
	if title == "" {
		title = "Chat " + sess.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "- Chat: `%s`\n", sess.ID)
	if sess.Agent != "" {
		fmt.Fprintf(&b, "- Agent: %s\n", sess.Agent)
	}
	if sess.UserID != "" {
		fmt.Fprintf(&b, "- User: %s\n", sess.UserID)
	}
	fmt.Fprintf(&b, "- Created: %s\n", sess.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Runs: %d · Tokens in/out: %d/%d · Cost: %.4f\n", meta.RunCount, meta.InputTokens, meta.OutputTokens, meta.Cost)
	if sess.Description != "" {
		fmt.Fprintf(&b, "- Description: %s\n", sess.Description)
	}
	if len(sess.Tags) > 0 {
		fmt.Fprintf(&b, "- Tags: %s\n", strings.Join(sess.Tags, ", "))
	}
	if sess.Summary != "" {
		fmt.Fprintf(&b, "\n## Summary\n\n%s\n", sess.Summary)
	}
	b.WriteString("\n## Transcript\n")
	for _, ev := range events {
		fmt.Fprintf(&b, "\n### %s · %s\n\n", ev.Type, ev.Timestamp.UTC().Format(time.RFC3339))
		if text := eventText(ev.Payload); text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		} else if len(ev.Payload) > 0 {
			b.WriteString("```json\n")
			b.Write(compactJSON(ev.Payload))
			b.WriteString("\n```\n")
		}
	}
	return b.String()
}

// eventText extracts a human-readable `text` field from an event payload, if any
// (text / thinking events carry it). Returns "" when the payload has no plain
// text body — the caller then falls back to the raw JSON.
func eventText(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var m struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.Text)
}

// compactJSON re-marshals a payload compactly. encoding/json emits map keys in
// sorted order, so a markdown export is stable across reads. A non-JSON payload
// is returned unchanged.
func compactJSON(payload []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(payload, &v); err != nil {
		return payload
	}
	b, err := json.Marshal(v)
	if err != nil {
		return payload
	}
	return b
}
