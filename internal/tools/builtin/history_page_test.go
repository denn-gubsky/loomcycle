package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// pagedChat builds a transcript of n exchanges (a user turn, then an assistant
// reply), one minute apart, each reply `size` characters long. It returns the
// events and the start time.
func pagedChat(n, size int) ([]store.Event, time.Time) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	var evs []store.Event
	seq := int64(0)
	add := func(typ string, at time.Time, payload string) {
		seq++
		evs = append(evs, store.Event{Seq: seq, Type: typ, Timestamp: at, Payload: []byte(payload)})
	}
	add("system_prompt", t0, `{"system_prompt":"sp"}`)
	for i := 0; i < n; i++ {
		at := t0.Add(time.Duration(i) * time.Minute)
		add("user_input", at, fmt.Sprintf(`[{"role":"user","content":[{"type":"text","text":"question %d"}]}]`, i))
		reply, _ := json.Marshal(map[string]any{"type": "text", "text": fmt.Sprintf("answer %d ", i) + strings.Repeat("x", size)})
		add("text", at.Add(10*time.Second), string(reply))
		add("done", at.Add(11*time.Second), `{"type":"done","stop_reason":"end_turn"}`)
	}
	return evs, t0
}

func speakers(p historyPage) []string {
	var out []string
	for _, t := range p.turns[p.start:p.end] {
		out = append(out, t.Speaker+":"+strings.Fields(t.Text)[0]+strings.Fields(t.Text)[1])
	}
	return out
}

// Offset and limit count conversation TURNS, and the page says where the next
// one starts. Without them a chat could only be read whole.
func TestHistoryPage_OffsetAndLimitCountTurns(t *testing.T) {
	evs, _ := pagedChat(3, 10) // 6 turns
	p, err := selectHistoryPage(context.Background(), evs, historyInput{Offset: 2, Limit: 2, Format: conversationFormat})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(speakers(p), " "); got != "user:question1 assistant:answer1" {
		t.Errorf("page = %q, want the second exchange", got)
	}
	f := p.fields()
	if f["turns_total"] != 6 || f["offset"] != 2 || f["turns_returned"] != 2 || f["has_more"] != true || f["next_offset"] != 4 {
		t.Errorf("fields = %v", f)
	}
	// The event form of the same page carries exactly that exchange's events.
	for _, ev := range p.events {
		if ev.Timestamp.Before(time.Date(2026, 9, 1, 10, 1, 0, 0, time.UTC)) || !ev.Timestamp.Before(time.Date(2026, 9, 1, 10, 2, 0, 0, time.UTC)) {
			t.Errorf("page carries an event from outside its turns: %s at %s", ev.Type, ev.Timestamp)
		}
	}
	if len(p.events) != 3 {
		t.Errorf("page events = %d, want 3 (user_input, text, done)", len(p.events))
	}
}

// from/to keep only the turns said inside the range, and offsets count within it.
func TestHistoryPage_FromToSelectTurnsByTime(t *testing.T) {
	evs, t0 := pagedChat(5, 10)
	in := historyInput{
		From:   t0.Add(2 * time.Minute).Format(time.RFC3339),
		To:     t0.Add(3*time.Minute + 30*time.Second).Format(time.RFC3339),
		Format: conversationFormat,
	}
	p, err := selectHistoryPage(context.Background(), evs, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(speakers(p), " "); got != "user:question2 assistant:answer2 user:question3 assistant:answer3" {
		t.Errorf("selected %q, want exchanges 2 and 3", got)
	}
	if f := p.fields(); f["turns_total"] != 4 || f["offset"] != 0 || f["has_more"] != false {
		t.Errorf("fields = %v", f)
	}
	if _, err := selectHistoryPage(context.Background(), evs, historyInput{From: "yesterday"}); err == nil {
		t.Error("a non-RFC3339 from was accepted")
	}
}

// Inside a run a page stops at the budget, at a turn boundary, and consecutive
// pages cover the chat exactly once. Off-run the whole chat comes back.
func TestHistoryPage_InRunPagesFitTheWindowAndCoverTheChat(t *testing.T) {
	evs, _ := pagedChat(10, 3000) // 20 turns, 10 of them ~3K characters
	window := 10000               // tokens → a 8,000-character budget
	ctx := tools.WithMaxContextTokens(tools.WithRunID(context.Background(), "r_reader"), window)

	var seen []string
	offset := 0
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging did not terminate")
		}
		p, err := selectHistoryPage(ctx, evs, historyInput{Offset: offset, Format: conversationFormat})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(renderConversationTurns(p.turns[p.start:p.end])); n > p.budget {
			t.Errorf("page at offset %d is %d characters, over the %d budget", offset, n, p.budget)
		}
		if p.end == p.start {
			t.Fatalf("empty page at offset %d while turns remain", offset)
		}
		seen = append(seen, speakers(p)...)
		f := p.fields()
		if f["has_more"] != true {
			break
		}
		offset = f["next_offset"].(int)
	}
	if len(seen) != 20 || seen[0] != "user:question0" || seen[19] != "assistant:answer9" {
		t.Errorf("pages covered %d turns (%v … %v), want all 20 once", len(seen), seen[0], seen[len(seen)-1])
	}

	off, _ := selectHistoryPage(context.Background(), evs, historyInput{Format: conversationFormat})
	if off.end-off.start != 20 {
		t.Errorf("off-run get returned %d turns, want the whole chat", off.end-off.start)
	}
}

// Through the tool: a paged get reports its position, and an offset past the
// end says so rather than returning an empty page that reads as an empty chat.
func TestHistory_GetPagesThroughTheTool(t *testing.T) {
	h, s := historyFixture(t)
	id := seedPlumbingChat(t, s) // 3 turns: user, assistant, assistant
	get := func(req string) map[string]any {
		t.Helper()
		res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "tok-user-1", "t1"), json.RawMessage(req))
		if res.IsError {
			t.Fatalf("get: %s", res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	out := get(fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"format":"conversation","offset":1,"limit":1}`, id))
	md := out["markdown"].(string)
	if !strings.Contains(md, "three replicas it is.") || strings.Contains(md, "Prague") || strings.Contains(md, "Done.") {
		t.Errorf("page 2 of 3 = %q", md)
	}
	if out["turns_total"] != float64(3) || out["next_offset"] != float64(2) || out["has_more"] != true {
		t.Errorf("paging fields = %v", out)
	}

	past := get(fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"format":"conversation","offset":9}`, id))
	if past["markdown"] != "" || !strings.Contains(fmt.Sprint(past["note"]), "past the last turn") {
		t.Errorf("offset past the end: %v", past)
	}

	// The event form pages at the same turn boundaries.
	ev := get(fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"offset":2}`, id))
	tr := ev["transcript"].([]any)
	if len(tr) != 2 || !strings.Contains(fmt.Sprint(tr[0]), "Done.") {
		t.Errorf("event page for the last turn = %v", tr)
	}
}
