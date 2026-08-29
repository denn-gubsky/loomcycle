package main

import "testing"

// The session stamp becomes a real observed time, and an unreadable one becomes
// nothing rather than a guess.
func TestTurn_ObservedAt(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1:56 pm on 8 May, 2023", "2023-05-08T00:00:00Z"},
		{"9:00 am on 1 June, 2023", "2023-06-01T00:00:00Z"},
		{"7:15 pm on 23 October, 2023", "2023-10-23T00:00:00Z"},
		{"sometime last spring", ""},
		{"", ""},
		{"on 31 Febtober, 2023", ""},
	} {
		if got := (Turn{DateTime: tc.in}).ObservedAt(); got != tc.want {
			t.Errorf("ObservedAt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A session row carries every turn in the session, once, under one date — and its
// key maps back to turn-level ground truth so recall stays scoreable.
func TestSessionRows_GroupsAndStaysScoreable(t *testing.T) {
	conv := Conversation{Turns: []Turn{
		{DiaID: "D1:1", Session: 1, DateTime: "1:00 pm on 3 May, 2023", Speaker: "A", Text: "hello"},
		{DiaID: "D1:2", Session: 1, DateTime: "1:00 pm on 3 May, 2023", Speaker: "B", Text: "hi there"},
		{DiaID: "D2:1", Session: 2, DateTime: "9:00 am on 10 May, 2023", Speaker: "A", Text: "later"},
	}}
	rows := SessionRows(conv)
	if len(rows) != 2 {
		t.Fatalf("got %d session rows, want 2", len(rows))
	}
	if rows[0].Key != "D1:s" || rows[1].Key != "D2:s" {
		t.Errorf("keys = %q/%q, want D1:s/D2:s", rows[0].Key, rows[1].Key)
	}
	if rows[0].ObservedAt != "2023-05-03T00:00:00Z" {
		t.Errorf("observed_at = %q, want the session's own date", rows[0].ObservedAt)
	}
	for _, want := range []string{"hello", "hi there"} {
		if !contains(rows[0].Body, want) {
			t.Errorf("session row lost %q: %s", want, rows[0].Body)
		}
	}
	if contains(rows[0].Body, "later") {
		t.Error("session row absorbed a turn from another session")
	}
	// The date appears ONCE — repeating it per turn would let it dominate the
	// embedding of a row whose value is its content.
	if n := count(rows[0].Body, "3 May, 2023"); n != 1 {
		t.Errorf("session date appears %d times, want exactly 1", n)
	}
	// And ground truth still maps.
	if SessionOfDiaID("D1:2") != "D1:s" {
		t.Errorf("SessionOfDiaID(D1:2) = %q, want D1:s — without this a session row "+
			"cannot be scored against turn-level evidence", SessionOfDiaID("D1:2"))
	}
	if SessionOfDiaID("nonsense") != "" {
		t.Error("an unparseable id must map to nothing, not to an invented session")
	}
}

func contains(hay, needle string) bool { return count(hay, needle) > 0 }

func count(hay, needle string) int {
	n, i := 0, 0
	for {
		j := indexFrom(hay, needle, i)
		if j < 0 {
			return n
		}
		n++
		i = j + 1
	}
}

func indexFrom(hay, needle string, from int) int {
	if from >= len(hay) {
		return -1
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// Scoring must credit a SESSION row for the expected TURNS it contains, or the
// CM-1 comparison measures nothing: the coarse arm would score zero everywhere.
func TestScore_SessionRowCreditsTheTurnsItContains(t *testing.T) {
	q := Query{Question: "when did X", Expected: []string{"D1:2", "D1:5", "D3:1"}}

	// Turn-level arm: three exact keys, three hits.
	turnRes := Score(q, []string{"D1:2", "D9:9", "D1:5", "D3:1"}, 10, 0)
	if turnRes.Hits != 3 {
		t.Errorf("turn arm hits = %d, want 3", turnRes.Hits)
	}

	// Session-level arm: D1:s covers TWO expected turns, D3:s covers one.
	sessRes := Score(q, []string{"D1:s", "D9:s", "D3:s"}, 10, 0)
	if sessRes.Hits != 3 {
		t.Errorf("session arm hits = %d, want 3 — a session row must credit every "+
			"expected turn it contains, or the coarse arm loses purely for being coarse",
			sessRes.Hits)
	}
	if sessRes.FirstHitRank != 1 {
		t.Errorf("session arm first hit at rank %d, want 1", sessRes.FirstHitRank)
	}
}

// The same expected turn is never credited twice, however it is reached.
func TestScore_NoDoubleCredit(t *testing.T) {
	q := Query{Question: "q", Expected: []string{"D1:2"}}
	res := Score(q, []string{"D1:s", "D1:2"}, 10, 0)
	if res.Hits != 1 {
		t.Errorf("hits = %d, want 1 — the session row and the turn row cover the SAME "+
			"expected turn, and counting it twice would report recall above 1.0", res.Hits)
	}
}
