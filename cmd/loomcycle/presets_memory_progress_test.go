package main

import (
	"context"
	"strings"
	"testing"
)

// The consolidator used to advance its watermark ONCE, at the end of a pass,
// and only when every chat on the page had succeeded. A pass stopped from
// outside part-way — the scheduler's per-target budget, a slow local extractor
// running a page past it — therefore kept nothing it had finished, and the next
// pass started from the same place and was stopped at the same point: on a live
// deployment the same transcript windows were re-sent to the extractor every
// five minutes for hours, and nothing was consolidated. These pin that progress
// is now recorded per chat, without loosening what the mark may move past.

// progressFixture is three chats, each with its own transcript and its own fact,
// over a toolset whose scan honours the watermark the way the store does.
func progressFixture() *fakeToolset {
	f := newFakeToolset()
	f.trackWatermark = true
	f.sessions = []map[string]any{
		scanRow("chat-1", "2026-07-01T10:00:00Z"),
		scanRow("chat-2", "2026-07-02T10:00:00Z"),
		scanRow("chat-3", "2026-07-03T10:00:00Z"),
	}
	f.transcripts = map[string]string{
		"chat-1": "user: I prefer Go for backend services.\nassistant: noted.",
		"chat-2": "user: I live in Berlin these days.\nassistant: noted.",
		"chat-3": "user: I drink green tea every morning.\nassistant: noted.",
	}
	f.factsBySession = map[string]string{
		"chat-1": `[{"text":"Denn prefers Go for backend services.","class":"preference"}]`,
		"chat-2": `[{"text":"Denn lives in Berlin.","class":"fact"}]`,
		"chat-3": `[{"text":"Denn drinks green tea every morning.","class":"preference"}]`,
	}
	return f
}

func historyReads(calls []recordedCall) []string {
	var out []string
	for _, c := range calls {
		if c.Tool == "History" {
			sid, _ := c.Input["session_id"].(string)
			out = append(out, sid)
		}
	}
	return out
}

// TestConsolidator_ACutPassKeepsTheChatsItFinished: extraction succeeds for
// chats 1 and 2, and the run is then cut while chat 3's extractor is running.
// The watermark must sit on chat 2, the next pass must start at chat 3, and
// nothing extracted on the first pass may be written again.
//
// Fails-before: the cut pass records no advance at all, so the second pass
// re-reads chats 1 and 2 and writes their facts a second time.
func TestConsolidator_ACutPassKeepsTheChatsItFinished(t *testing.T) {
	f := progressFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onSpawn = func(prompt string) bool {
		if strings.Contains(prompt, f.transcripts["chat-3"]) {
			cancel() // the pass's budget ends while chat 3 is being extracted
			return true
		}
		return false
	}

	// The cut run's own error is expected and not what this is about.
	_, _ = execConsolidator(ctx, t.TempDir(), f, memoryBundleConfig(t).Agents["memory/consolidator"])

	if got := strings.Join(historyReads(f.calls), ","); got != "chat-1,chat-2,chat-3" {
		t.Fatalf("first pass read %s, want chat-1,chat-2,chat-3 — the scenario must reach chat 3 before the cut; sequence %v", got, f.ops())
	}
	if f.watermarkSID != "chat-2" {
		t.Fatalf("after the cut the watermark is %q, want chat-2 — a pass stopped midway must keep the chats it finished; sequence %v", f.watermarkSID, f.ops())
	}
	firstPass := len(f.calls)

	f.onSpawn = nil
	res := runConsolidator(t, f)

	if got := strings.Join(historyReads(f.calls[firstPass:]), ","); got != "chat-3" {
		t.Errorf("second pass read %s, want only chat-3 — it must resume at the first chat the cut pass did not finish", got)
	}
	if f.watermarkSID != "chat-3" {
		t.Errorf("after the second pass the watermark is %q, want chat-3; report %q", f.watermarkSID, res.FinalText)
	}
	seen := map[string]int{}
	for _, w := range factWrites(f) {
		seen[w.Key]++
	}
	if len(seen) != 3 {
		t.Errorf("the two passes wrote %d distinct facts, want the 3 chats' 3; writes %v", len(seen), seen)
	}
	for key, n := range seen {
		if n != 1 {
			t.Errorf("fact %q was written %d times across the cut and the retry, want once — a finished chat must not be extracted again", key, n)
		}
	}
}

// TestConsolidator_AdvanceStopsAtTheFirstFailedChat: the per-chat advance must
// not skip a hole. Chat 2's write fails; chat 3 succeeds, but the watermark is a
// single position, so it may move past chat 1 and no further — moving to chat 3
// would put chat 2 behind it, never to be read again.
//
// Fails-before: no advance at all, chat 1 included.
func TestConsolidator_AdvanceStopsAtTheFirstFailedChat(t *testing.T) {
	f := progressFixture()
	f.failEntityKeys["memory/fact/denn-lives-berlin"] = true

	res := runConsolidator(t, f)

	if len(callsWithOp(f, "Document.upsert_chunk")) == 0 {
		t.Fatalf("no write was attempted, so the scenario proves nothing; sequence %v", f.ops())
	}
	if got := strings.Join(historyReads(f.calls), ","); got != "chat-1,chat-2,chat-3" {
		t.Errorf("the pass read %s, want every chat on the page", got)
	}
	var advanced []string
	for _, c := range callsWithOp(f, "Memory.cursor_advance") {
		sid, _ := c.Input["session_id"].(string)
		advanced = append(advanced, sid)
	}
	if got := strings.Join(advanced, ","); got != "chat-1" {
		t.Errorf("cursor_advance went to %q, want chat-1 only — the mark must stop before the chat whose write failed; sequence %v", got, f.ops())
	}
	for _, want := range []string{"watermark advanced to chat-1", "then held", "write failed"} {
		if !strings.Contains(res.FinalText, want) {
			t.Errorf("report = %q, want it to contain %q — it must say both what was kept and why it stopped", res.FinalText, want)
		}
	}
}
