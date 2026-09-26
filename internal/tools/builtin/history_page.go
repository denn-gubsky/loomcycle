package builtin

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// historyPage is the part of one chat a `get` returns, counted in conversation
// turns: [start, end) indexes the chat's turns, inside the [lo, hi) range the
// from/to bounds selected.
//
// Turns are the unit because they are what a reader means by "part of a chat",
// and because they are the one boundary every format shares: a turn owns the
// events from its first sequence number up to the next turn's, so the event and
// markdown formats page at exactly the places the conversation form does.
type historyPage struct {
	turns          []conversationTurn
	lo, hi         int
	start, end     int
	events         []store.Event // the events turns[start:end] span
	budget         int           // characters; 0 = unbounded (off-run)
	offsetTooLarge bool
}

// selectHistoryPage applies from/to, then offset/limit, then — inside a run —
// the size budget, and returns the page.
//
// Measured live: a local model read a whole chat into a 32K window (the default
// event array was 33K and 51K tokens on two runs) and overflowed. Inside a run a
// page therefore stops at the budget, at a turn boundary, and says where the
// next page starts. An off-run caller (an MCP client, the Web UI) has no window
// to protect and gets everything it asked for.
func selectHistoryPage(ctx context.Context, events []store.Event, in historyInput) (historyPage, error) {
	if in.Offset < 0 || in.Limit < 0 {
		return historyPage{}, fmt.Errorf("history: get: offset and limit must not be negative")
	}
	var from, to time.Time
	var err error
	if in.From != "" {
		if from, err = time.Parse(time.RFC3339, in.From); err != nil {
			return historyPage{}, fmt.Errorf("history: from must be RFC3339: %v", err)
		}
	}
	if in.To != "" {
		if to, err = time.Parse(time.RFC3339, in.To); err != nil {
			return historyPage{}, fmt.Errorf("history: to must be RFC3339: %v", err)
		}
	}

	p := historyPage{turns: conversationTurns(events)}
	if tools.RunID(ctx) != "" {
		p.budget = historyInRunBudget(ctx)
	}
	if len(p.turns) == 0 {
		// A chat with no user or assistant turn (a run that failed before its
		// first reply) has no boundary to page at: it is one page.
		p.events = events
		return p, nil
	}

	// Turns are in time order, so the ones inside [from, to] are contiguous.
	p.lo, p.hi = len(p.turns), len(p.turns)
	for i, t := range p.turns {
		if !from.IsZero() && t.At.Before(from) {
			continue
		}
		if !to.IsZero() && t.At.After(to) {
			break
		}
		if p.lo == len(p.turns) {
			p.lo = i
		}
		p.hi = i + 1
	}
	if p.lo == len(p.turns) {
		p.lo, p.hi = 0, 0 // nothing in the range
	}

	p.start = p.lo + in.Offset
	if p.start > p.hi {
		p.start = p.hi
		p.offsetTooLarge = p.hi > p.lo
	}
	p.end = p.hi
	if in.Limit > 0 && p.start+in.Limit < p.end {
		p.end = p.start + in.Limit
	}
	if p.budget > 0 {
		p.end = p.fitBudget(events, in.Format)
	}
	p.events = p.eventsBetween(events, p.start, p.end)
	return p, nil
}

// historyInRunBudget is how many characters of transcript one in-run `get` may
// return: a fifth of the run's context window at about 4 characters a token,
// so a page leaves room for the prompt, the task, and the answer. Without a
// known window it falls back to the fixed cap the markdown export used.
func historyInRunBudget(ctx context.Context) int {
	if n := tools.MaxContextTokens(ctx); n > 0 {
		return n * 4 / 5
	}
	return historyMarkdownInRunCap
}

// fitBudget returns the last turn (exclusive) that fits the budget, counting
// from start. At least one turn is always returned, so a page can never be
// empty while turns remain; a single turn larger than the budget is cut by the
// caller and marked truncated.
func (p historyPage) fitBudget(events []store.Event, format string) int {
	used := 0
	for i := p.start; i < p.end; i++ {
		n := p.turnSize(events, i, format)
		if i > p.start && used+n > p.budget {
			return i
		}
		used += n
	}
	return p.end
}

// turnSize estimates one turn's rendered size in the requested format. The
// conversation form is exact; the event forms sum their payloads plus a fixed
// per-event overhead, which is what dominates them.
func (p historyPage) turnSize(events []store.Event, i int, format string) int {
	if format == conversationFormat {
		t := p.turns[i]
		return len("### ") + len(t.Speaker) + len("\n\n") + len(t.Text) + len("\n\n")
	}
	n := 0
	for _, ev := range p.eventsBetween(events, i, i+1) {
		n += len(ev.Payload) + 96
	}
	return n
}

// eventsBetween returns the events that turns[i:j] span: from turn i's first
// sequence number up to turn j's. The first page also carries the events before
// the first turn (the run start, the prompt snapshot), and the last page the
// events after the last turn began.
func (p historyPage) eventsBetween(events []store.Event, i, j int) []store.Event {
	if i >= j {
		return nil
	}
	lo := int64(math.MinInt64)
	if i > 0 {
		lo = p.turns[i].Seq
	}
	hi := int64(math.MaxInt64)
	if j < len(p.turns) {
		hi = p.turns[j].Seq
	}
	var out []store.Event
	for _, ev := range events {
		if ev.Seq >= lo && ev.Seq < hi {
			out = append(out, ev)
		}
	}
	return out
}

// fields reports where this page sits, so a caller can ask for the next one.
// Offsets count turns inside the from/to selection.
func (p historyPage) fields() map[string]any {
	out := map[string]any{
		"turns_total":    p.hi - p.lo,
		"offset":         p.start - p.lo,
		"turns_returned": p.end - p.start,
		"has_more":       p.end < p.hi,
	}
	if p.end < p.hi {
		out["next_offset"] = p.end - p.lo
	}
	return out
}
