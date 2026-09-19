package builtin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestConversationTurns_CarriesTheTurnsOwnInstant.
//
// ⚠️ WITHOUT THIS THE TRACE INDEX CANNOT ANSWER "WHEN". The index stored
// time.Now() — the moment of INDEXING — because nothing carried the turn's own
// time, so a backfill run months after a conversation stamped every turn in it
// with the same afternoon. That matters more than it sounds: a distilled fact is
// tenseless and the TURN is what carries the date, which is why attaching turns
// moved the temporal slice at all. A turn without its instant is the half of the
// evidence the temporal questions need, missing.
func TestConversationTurns_CarriesTheTurnsOwnInstant(t *testing.T) {
	t0 := time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Minute)
	mk := func(seq int64, ts time.Time, typ string, payload string) store.Event {
		return store.Event{Seq: seq, Timestamp: ts, Type: typ, Payload: []byte(payload)}
	}
	turns := conversationTurns([]store.Event{
		mk(1, t0, "user_input", `[{"role":"user","content":[{"type":"text","text":"I went to the support group."}]}]`),
		mk(2, t1, "text", `{"type":"text","text":"How was it?"}`),
		mk(3, t1, "done", `{"type":"done"}`),
	})
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d: %+v", len(turns), turns)
	}
	if !turns[0].At.Equal(t0) {
		t.Errorf("user turn At = %v, want the transcript event's own time %v", turns[0].At, t0)
	}
	if !turns[1].At.Equal(t1) {
		t.Errorf("assistant turn At = %v, want %v", turns[1].At, t1)
	}
}

// TestConversationTurns_AssistantTurnIsStampedWhenItBEGAN.
//
// An assistant turn accumulates across streamed deltas. Taking the LAST delta's
// instant would date a long reply by when it finished, which is not when it was
// said and is not how the user turn beside it is stamped.
func TestConversationTurns_AssistantTurnIsStampedWhenItBegan(t *testing.T) {
	first := time.Date(2023, 5, 8, 14, 0, 0, 0, time.UTC)
	last := first.Add(90 * time.Second)
	mk := func(ts time.Time, typ, payload string) store.Event {
		return store.Event{Seq: 1, Timestamp: ts, Type: typ, Payload: []byte(payload)}
	}
	turns := conversationTurns([]store.Event{
		mk(first, "text", `{"type":"text","text":"Really "}`),
		mk(last, "text", `{"type":"text","text":"good."}`),
		mk(last, "done", `{"type":"done"}`),
	})
	if len(turns) != 1 {
		t.Fatalf("want 1 assistant turn, got %d", len(turns))
	}
	if !turns[0].At.Equal(first) {
		t.Errorf("At = %v, want the FIRST delta's time %v — a reply is dated when it "+
			"began, matching the user turn beside it", turns[0].At, first)
	}
}

// TestTraceIndex_StoresTheTurnInstantNotTheIndexingTime.
//
// Structural guard over the three write paths. The live USER path may legitimately
// use now() — it indexes a turn as it arrives — but the backfill and the assistant
// path both REPLAY turns said earlier and must take the turn's own stamp.
func TestTraceIndex_StoresTheTurnInstantNotTheIndexingTime(t *testing.T) {
	for _, f := range []string{"../../api/http/trace_backfill.go", "../../api/http/trace_index.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		if !strings.Contains(src, "turn.At") {
			t.Errorf("%s never reads turn.At — it is stamping replayed turns with the "+
				"time they were INDEXED, so a trace search cannot answer \"when\"", f)
		}
	}
}

// TestTraceTurnValue_RoundTripsTheInstant. The field is what a reader sees; if it
// does not survive marshalling the rest is academic.
func TestTraceTurnValue_RoundTripsTheInstant(t *testing.T) {
	raw := `{"at":"2023-05-08T13:56:00Z","text":"[1:56 pm on 8 May, 2023] Caroline: hi","speaker":"user","session_id":"s_1"}`
	var row struct {
		At   string `json:"at"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatal(err)
	}
	if row.At != "2023-05-08T13:56:00Z" {
		t.Errorf("at = %q", row.At)
	}
	if got := TraceTurnText(json.RawMessage(raw)); !strings.Contains(got, "Caroline") {
		t.Errorf("the shared parser lost the text: %q", got)
	}
}
