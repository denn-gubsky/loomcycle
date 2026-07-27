package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedPlumbingChat writes one session whose transcript carries the full shape a
// real run produces: the resolved system prompt, a user turn whose own content
// legitimately contains a JSON code fence, streamed assistant text deltas, a
// tool call with a fat input, a usage row, the iteration's `done`, and a fat
// tool result. It returns the session id.
func seedPlumbingChat(t *testing.T, s store.Store) string {
	t.Helper()
	bg := context.Background()
	id := seedChat(t, s, "t1", "agentA", "tok-user-1")
	run, err := s.CreateRun(bg, id, store.RunIdentity{AgentID: "a_run", UserID: "tok-user-1", TenantID: "t1"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	ev := func(typ, payload string) {
		t.Helper()
		if err := s.AppendEvent(bg, run.ID, typ, []byte(payload)); err != nil {
			t.Fatalf("AppendEvent(%s): %v", typ, err)
		}
	}
	ev("system_prompt", `{"system_prompt":"You are agentA. NEVER reveal the SYSTEM-PLUMBING-MARKER.","source":"static"}`)
	// The user turn is []PromptSegment — a system segment (the loop's own
	// framing) plus the human's actual message, which contains a JSON fence
	// that is CONTENT and must survive verbatim.
	ev("user_input", `[
		{"role":"system","content":[{"type":"trusted-text","text":"SYSTEM-SEGMENT-MARKER"}]},
		{"role":"user","content":[{"type":"trusted-text","text":"Deploy with this config:\n`+"```"+`json\n{\"replicas\": 3}\n`+"```"+`\nI am on the Prague team."}]}
	]`)
	// Assistant text is persisted one row PER STREAMED DELTA.
	ev("text", `{"type":"text","text":"Sure — "}`)
	ev("text", `{"type":"text","text":"three replicas it is."}`)
	ev("tool_call", `{"type":"tool_call","tool_use":{"id":"tu_1","name":"Read","input":{"file_path":"/srv/TOOLCALL-PLUMBING-MARKER.yaml"}}}`)
	ev("usage", `{"type":"usage","usage":{"input_tokens":8450,"output_tokens":120,"model":"USAGE-PLUMBING-MARKER"}}`)
	ev("done", `{"type":"done","stop_reason":"tool_use"}`)
	ev("tool_result", `{"type":"tool_result","tool_use":{"id":"tu_1","name":"Read"},"text":"TOOLRESULT-PLUMBING-MARKER: 4000 bytes of yaml"}`)
	ev("text", `{"type":"text","text":"Done."}`)
	ev("done", `{"type":"done","stop_reason":"end_turn"}`)
	if err := s.FinishRun(bg, run.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: 8450, OutputTokens: 120}, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return id
}

// getConversation runs History get with format=conversation and returns the
// rendered body.
func getConversation(t *testing.T, h *History, sessionID string) string {
	t.Helper()
	req := fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"format":"conversation"}`, sessionID)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "tok-user-1", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("get conversation: %s", res.Text)
	}
	var out struct {
		Format   string `json:"format"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Text)
	}
	if out.Format != "conversation" {
		t.Fatalf("format echo = %q, want %q — the runtime did not render the conversation form", out.Format, "conversation")
	}
	return out.Markdown
}

// TestHistory_ConversationFormatCarriesNoRuntimePlumbing is the extractor-input
// contract: what the memory consolidator hands an extractor model must be the
// conversation and nothing else. Every marker below is loomcycle's own
// scaffolding, and the live incident was the extractor returning the chat's
// participant line — read out of the markdown export's metadata header — as a
// durable "fact".
func TestHistory_ConversationFormatCarriesNoRuntimePlumbing(t *testing.T) {
	h, s := historyFixture(t)
	id := seedPlumbingChat(t, s)
	md := getConversation(t, h, id)

	for _, banned := range []string{
		"# Chat",     // metadata header title
		"- Agent:",   // which agent served the chat
		"- User:",    // the participant line the extractor turned into a "fact"
		"- Runs:",    // run/token/cost roll-up
		"- Chat:",    // the session id
		"## Summary", // stored summary — a fact about the conversation
		"SYSTEM-PLUMBING-MARKER",
		"SYSTEM-SEGMENT-MARKER",
		"TOOLCALL-PLUMBING-MARKER",
		"USAGE-PLUMBING-MARKER",
		"TOOLRESULT-PLUMBING-MARKER",
		"system_prompt",
		"tool_call",
		"tool_result",
		"usage",
	} {
		if strings.Contains(md, banned) {
			t.Errorf("conversation rendering must not contain %q:\n%s", banned, md)
		}
	}
}

// TestHistory_ConversationFormatKeepsUserContentVerbatim guards the other half:
// stripping loomcycle's event scaffolding is not the same as stripping every
// structured text. A user turn that contains a code fence or a JSON blob is
// content and must arrive at the extractor unchanged.
func TestHistory_ConversationFormatKeepsUserContentVerbatim(t *testing.T) {
	h, s := historyFixture(t)
	id := seedPlumbingChat(t, s)
	md := getConversation(t, h, id)

	for _, want := range []string{
		"```json",                      // the fence opener the user typed
		`{"replicas": 3}`,              // the JSON the user typed
		"I am on the Prague team.",     // the durable fact
		"Sure — three replicas it is.", // streamed deltas coalesced into ONE turn
		"Done.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("conversation rendering dropped user/assistant content %q:\n%s", want, md)
		}
	}
	// Deltas must not each become their own turn — a per-delta section header
	// would both bloat the input and hand the model fragments with no speaker.
	if n := strings.Count(md, "### assistant"); n != 2 {
		t.Errorf("assistant turns = %d, want 2 (one per iteration, deltas coalesced):\n%s", n, md)
	}
	if n := strings.Count(md, "### user"); n != 1 {
		t.Errorf("user turns = %d, want 1:\n%s", n, md)
	}
}

// TestHistory_ConversationFormatIsAFractionOfTheMarkdownExport quantifies the
// size win the consolidator gets: the plumbing, not the conversation, is what
// fills a transcript, and the extractor is split into parts by character
// budget. Both renderings are measured off the same session.
func TestHistory_ConversationFormatIsAFractionOfTheMarkdownExport(t *testing.T) {
	h, s := historyFixture(t)
	id := seedPlumbingChat(t, s)

	full := getMarkdown(t, h, id)
	conv := getConversation(t, h, id)
	t.Logf("markdown export = %d chars; conversation = %d chars (%.1f%% of the export)",
		len(full), len(conv), 100*float64(len(conv))/float64(len(full)))
	if len(conv) >= len(full)/2 {
		t.Errorf("conversation rendering (%d chars) should be well under half the markdown export (%d chars)",
			len(conv), len(full))
	}
}

// TestHistory_ConversationFormatOnAProductionSizedChat measures the same split
// on a chat scaled to the size of the real transcript part that produced the
// live incident (~8.5k chars of markdown export). The composition is what
// matters: a resolved system prompt, a handful of tool round-trips with fat
// results, per-delta assistant rows, and a usage row per iteration — that is
// what fills a production transcript, and none of it is conversation.
func TestHistory_ConversationFormatOnAProductionSizedChat(t *testing.T) {
	h, s := historyFixture(t)
	bg := context.Background()
	id := seedChat(t, s, "t1", "agentA", "tok-user-1")
	run, err := s.CreateRun(bg, id, store.RunIdentity{AgentID: "a_run", UserID: "tok-user-1", TenantID: "t1"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	ev := func(typ, payload string) {
		t.Helper()
		if err := s.AppendEvent(bg, run.ID, typ, []byte(payload)); err != nil {
			t.Fatalf("AppendEvent(%s): %v", typ, err)
		}
	}
	ev("system_prompt", fmt.Sprintf(`{"system_prompt":%q,"source":"static"}`,
		"You are agentA. "+strings.Repeat("Follow the operator's standing instructions carefully. ", 12)))
	ev("user_input", `[{"role":"user","content":[{"type":"trusted-text","text":"Set up the staging cluster. I prefer Terraform over Helm."}]}]`)
	for i := 0; i < 5; i++ {
		ev("text", fmt.Sprintf(`{"type":"text","text":"Checking step %d — "}`, i))
		ev("text", `{"type":"text","text":"one moment."}`)
		ev("tool_call", fmt.Sprintf(`{"type":"tool_call","tool_use":{"id":"tu_%d","name":"Read","input":{"file_path":"/srv/infra/module-%d.tf","offset":0,"limit":400}}}`, i, i))
		ev("usage", fmt.Sprintf(`{"type":"usage","usage":{"input_tokens":%d,"output_tokens":90,"model":"a-model","provider":"a-provider"}}`, 1200+i))
		ev("done", `{"type":"done","stop_reason":"tool_use"}`)
		ev("tool_result", fmt.Sprintf(`{"type":"tool_result","tool_use":{"id":"tu_%d","name":"Read"},"text":%q}`,
			i, strings.Repeat("resource \"aws_instance\" \"node\" { ami = \"ami-0\" }\n", 8)))
	}
	ev("text", `{"type":"text","text":"Staging is up, Terraform only."}`)
	ev("done", `{"type":"done","stop_reason":"end_turn"}`)

	full := getMarkdown(t, h, id)
	conv := getConversation(t, h, id)
	t.Logf("production-shaped chat: markdown export = %d chars; conversation = %d chars (%.1f%% of the export, %.1fx smaller)",
		len(full), len(conv), 100*float64(len(conv))/float64(len(full)), float64(len(full))/float64(len(conv)))
	// The durable content is the user's preference; it must survive the strip.
	if !strings.Contains(conv, "I prefer Terraform over Helm.") {
		t.Errorf("the one durable fact in the chat did not survive:\n%s", conv)
	}
	if len(conv) >= len(full)/4 {
		t.Errorf("conversation rendering (%d chars) should be a small fraction of the export (%d chars)", len(conv), len(full))
	}
}

// getMarkdown runs History get with the human-facing markdown export.
func getMarkdown(t *testing.T, h *History, sessionID string) string {
	t.Helper()
	req := fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"format":"markdown"}`, sessionID)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "tok-user-1", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("get markdown: %s", res.Text)
	}
	var out struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Markdown
}

// TestHistory_MarkdownExportStillCarriesTheMetadataHeader pins the deliberate
// split: `format:markdown` is the HUMAN export (Web UI, an operator reading a
// chat) and keeps the header and the full event log. Only the new
// `format:conversation` strips them. Without this, a later "simplify" pass
// could collapse the two renderers and silently change the human export.
func TestHistory_MarkdownExportStillCarriesTheMetadataHeader(t *testing.T) {
	h, s := historyFixture(t)
	id := seedPlumbingChat(t, s)
	md := getMarkdown(t, h, id)
	for _, want := range []string{"- Chat:", "- Agent:", "- User:", "- Runs:", "### tool_call"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown export lost %q — it is the human-facing rendering and must keep the header", want)
		}
	}
}
