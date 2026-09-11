package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The reach-through (RFC CV P1): a distilled fact is a sentence, and the turn it came
// from usually carries the specific the sentence dropped. These pin that following the
// pointer returns the RIGHT turn with its neighbours, and that every way it can fail
// says so rather than answering from the wrong place.

// seedConversation writes a chat of alternating turns and returns its session id.
func seedConversation(t *testing.T, s store.Store, turns ...string) string {
	t.Helper()
	bg := context.Background()
	id := seedChat(t, s, "t1", "agentA", "alice")
	run, err := s.CreateRun(bg, id, store.RunIdentity{AgentID: "a_run", UserID: "alice", TenantID: "t1"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for i, text := range turns {
		tj, _ := json.Marshal(text)
		if i%2 == 0 {
			// A user turn is a []PromptSegment, which is what userTurnText parses.
			seg := fmt.Sprintf(`[{"role":"user","content":[{"type":"text","text":%s}]}]`, string(tj))
			_ = s.AppendEvent(bg, run.ID, "user_input", []byte(seg))
			continue
		}
		_ = s.AppendEvent(bg, run.ID, "text", []byte(fmt.Sprintf(`{"text":%s}`, string(tj))))
		// `done` is what closes an assistant turn — without it the deltas accumulate
		// into one turn and the window would return a different shape than the
		// conversation rendering the span was quoted from.
		_ = s.AppendEvent(bg, run.ID, "done", []byte(`{}`))
	}
	if err := s.FinishRun(bg, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return id
}

func windowResult(t *testing.T, h *History, sessionID, quote string, contextN int) map[string]any {
	t.Helper()
	req := fmt.Sprintf(`{"op":"window","scope":"self","session_id":%q,"quote":%q,"context":%d}`,
		sessionID, quote, contextN)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("window: %s", res.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Text)
	}
	return out
}

// TestHistoryWindow_ReturnsTheTurnAFactCameFromAndItsNeighbours is the reach-through.
//
// The fact would have been distilled to "the release moved" — the date and the reason
// live in the turn and the turn beside it, which is the whole point.
func TestHistoryWindow_ReturnsTheTurnAFactCameFromAndItsNeighbours(t *testing.T) {
	h, s := historyFixture(t)
	id := seedConversation(t, s,
		"morning — anything on the calendar?",
		"standup at ten, then the release review.",
		"we moved the release to the 14th because Maria is out.",
		"noted, I will tell the team.",
		"thanks.",
	)

	got := windowResult(t, h, id, "we moved the release to the 14th because Maria is out.", 1)
	if got["matched"] != true {
		t.Fatalf("the span was not located: %v", got)
	}
	if n, _ := got["matched_turn"].(float64); int(n) != 2 {
		t.Errorf("matched turn %v, want 2", got["matched_turn"])
	}
	md, _ := got["markdown"].(string)
	// The turn itself...
	if !strings.Contains(md, "the 14th") {
		t.Errorf("the window does not contain the fact's own turn: %q", md)
	}
	// ...and its neighbours, which carry what a distilled sentence drops.
	if !strings.Contains(md, "release review") {
		t.Errorf("the preceding turn is missing, so a bare date has nothing to resolve against: %q", md)
	}
	if !strings.Contains(md, "tell the team") {
		t.Errorf("the following turn is missing: %q", md)
	}
	// And NOT the whole chat: a reach-through that returns 419 turns is not one.
	if strings.Contains(md, "morning") {
		t.Errorf("the window reached past its context bound: %q", md)
	}
}

// TestHistoryWindow_ASpanItCannotLocateIsReportedNotGuessed. Returning the start of
// the chat would look like a successful reach-through and answer from the wrong place —
// the failure mode that makes a retrieval surface untrustworthy rather than merely
// incomplete.
func TestHistoryWindow_ASpanItCannotLocateIsReportedNotGuessed(t *testing.T) {
	h, s := historyFixture(t)
	id := seedConversation(t, s, "hello", "hi there", "what is for lunch?")

	got := windowResult(t, h, id, "a sentence nobody in this chat ever said", 2)
	if got["matched"] != false {
		t.Fatalf("an unlocatable span reported a match: %v", got)
	}
	if turns, _ := got["turns"].([]any); len(turns) != 0 {
		t.Errorf("turns returned for an unmatched span: %v", turns)
	}
	if note, _ := got["note"].(string); note == "" {
		t.Error("an unmatched span must say why, or it is indistinguishable from an empty chat")
	}
}

// TestHistoryWindow_MatchesASpanThatDiffersOnlyCosmetically. A span crosses a model
// and two stores on its way to the sidecar and can pick up a collapsed newline or a
// changed case. Refusing those would report "no source" for facts whose source is
// right there.
func TestHistoryWindow_MatchesASpanThatDiffersOnlyCosmetically(t *testing.T) {
	h, s := historyFixture(t)
	id := seedConversation(t, s, "we ship on\n   Fridays, always", "understood")

	got := windowResult(t, h, id, "We ship on Fridays, always", 0)
	if got["matched"] != true {
		t.Fatalf("a span differing only in whitespace and case was not matched: %v", got)
	}
}

// TestHistoryWindow_RefusesWithoutAQuote. The span is the anchor; without it there is
// no window to compute, and defaulting to the start of the chat would be the guess the
// design refuses to make.
func TestHistoryWindow_RefusesWithoutAQuote(t *testing.T) {
	h, s := historyFixture(t)
	id := seedConversation(t, s, "hello", "hi")
	req := fmt.Sprintf(`{"op":"window","scope":"self","session_id":%q}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatal("window without a quote should refuse")
	}
}

// TestHistoryWindow_AnArchivedSourceSaysTheSpanSurvives. Retention snapshots the span
// onto the fact BEFORE removing the session, so the evidence outlives the transcript.
// What is lost is the surrounding context, and the message has to say which — "not
// found" alone reads as data loss.
func TestHistoryWindow_AnArchivedSourceSaysTheSpanSurvives(t *testing.T) {
	h, _ := historyFixture(t)
	req := `{"op":"window","scope":"self","session_id":"s_gone","quote":"anything"}`
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatal("a window into a session that is not there should refuse")
	}
	if !strings.Contains(res.Text, "span survives") {
		t.Errorf("the refusal must distinguish an archived source from data loss: %q", res.Text)
	}
}

// TestHistoryWindow_IsScopeGatedLikeEveryOtherTranscriptRead. The op reads a
// conversation, so it inherits History's own gate rather than introducing a second
// one — a fact id is a coordinate, never an authorisation.
func TestHistoryWindow_IsScopeGatedLikeEveryOtherTranscriptRead(t *testing.T) {
	h, s := historyFixture(t)
	id := seedConversation(t, s, "a secret", "noted")

	// A DIFFERENT agent, granted only its own chats.
	req := fmt.Sprintf(`{"op":"window","scope":"self","session_id":%q,"quote":"a secret"}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentB", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatalf("another agent's chat was readable through window: %s", res.Text)
	}
}

// TestHistoryWindow_SegmentsTurnsExACTLYAsTheConversationRenderingDoes.
//
// This is the invariant the whole reach-through rests on, and it is quiet enough to
// break unnoticed. A fact's source span was quoted out of the CONVERSATION rendering —
// that is the text the extractor reads — so if the window segmented turns even slightly
// differently, spans that are really there would stop being found. The failure would
// surface as "no source recorded", which reads as missing data rather than as a bug.
//
// So both consumers go through one segmentation, and this asserts they agree.
func TestHistoryWindow_SegmentsTurnsExactlyAsTheConversationRenderingDoes(t *testing.T) {
	h, s := historyFixture(t)
	id := seedConversation(t, s,
		"first question",
		"first answer",
		"second question",
		"second answer",
	)

	// The conversation rendering, which is what a fact's span is quoted from.
	req := fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"format":"conversation"}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(res.Text), &got)
	rendered, _ := got["markdown"].(string)

	// A window wide enough to cover the whole chat must reproduce it verbatim.
	win := windowResult(t, h, id, "first question", windowMaxContext)
	if md, _ := win["markdown"].(string); md != rendered {
		t.Errorf("the window and the conversation rendering disagree about turn boundaries.\n"+
			"window:\n%q\nrendering:\n%q", md, rendered)
	}
}
