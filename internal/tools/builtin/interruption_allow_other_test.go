package builtin

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// capturePublisher records the payloads published to the system channel.
type capturePublisher struct {
	mu       sync.Mutex
	payloads []json.RawMessage
}

func (c *capturePublisher) Publish(ctx context.Context, channel, tenantID string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, deliverAt time.Time, publishedByUserID string, maxMessages int, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, payload)
	return store.ChannelMessage{}, nil
}

func (c *capturePublisher) PublishNow(ctx context.Context, channel, tenantID string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, publishedByUserID string, maxMessages int, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	return c.Publish(ctx, channel, tenantID, scope, scopeID, payload, time.Time{}, publishedByUserID, maxMessages, defaultTTLSeconds)
}

// askAndCapture runs one ask with the given input and returns what it
// stored, emitted and published, then cancels the pending row.
func askAndCapture(t *testing.T, input string) (store.InterruptRow, *providers.InterruptionEventInfo, map[string]any) {
	t.Helper()
	tool, ctx, _, cleanup := interruptionFixture(t)
	defer cleanup()
	pub := &capturePublisher{}
	tool.SystemPublisher = pub
	evCh := make(chan providers.Event, 1)
	ctx = tools.WithEventEmitter(ctx, func(e providers.Event) {
		if e.Type == providers.EventInterruptionPending {
			select {
			case evCh <- e:
			default:
			}
		}
	})
	resCh := make(chan tools.Result, 1)
	go func() {
		r, _ := tool.Execute(ctx, json.RawMessage(input))
		resCh <- r
	}()
	row := waitPending(t, tool, ctx)[0]
	var ev providers.Event
	select {
	case ev = <-evCh:
	case <-time.After(5 * time.Second):
		t.Fatal("EventInterruptionPending not emitted")
	}
	_ = tool.Store.InterruptFinish(ctx, row.InterruptID, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)
	tool.Bus.Notify("intr:" + row.InterruptID)
	<-resCh

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.payloads) != 1 {
		t.Fatalf("system publishes = %d, want 1", len(pub.payloads))
	}
	var payload map[string]any
	if err := json.Unmarshal(pub.payloads[0], &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return row, ev.Interruption, payload
}

// TestInterruption_AllowOtherReachesTheRowTheEventAndTheSystemPayload pins
// RFC HG §3.1: allow_other on ask is stored on the row (which every resolve
// path checks), carried on the SSE event and the _system/interrupts/pending
// payload (which clients render from).
func TestInterruption_AllowOtherReachesTheRowTheEventAndTheSystemPayload(t *testing.T) {
	row, ev, payload := askAndCapture(t, `{"op":"ask","question":"Which?","options":["A","B"],"allow_other":true,"timeout_ms":5000}`)
	if !row.AllowOther {
		t.Error("row AllowOther = false, want true")
	}
	if ev == nil || !ev.AllowOther {
		t.Errorf("event = %+v, want AllowOther", ev)
	}
	if payload["allow_other"] != true {
		t.Errorf("system payload allow_other = %v, want true", payload["allow_other"])
	}
}

// TestInterruption_AllowOtherWithoutOptionsIsNotClaimed pins that a free-text
// question never claims allow_other: it already takes any answer.
func TestInterruption_AllowOtherWithoutOptionsIsNotClaimed(t *testing.T) {
	row, ev, payload := askAndCapture(t, `{"op":"ask","question":"Why?","allow_other":true,"timeout_ms":5000}`)
	if row.AllowOther || ev.AllowOther || payload["allow_other"] != false {
		t.Errorf("free-text question claims allow_other: row=%v event=%v payload=%v", row.AllowOther, ev.AllowOther, payload["allow_other"])
	}
}

// TestInterruption_ResultMarksAnAnswerOutsideTheOptionsOther pins RFC HG §3.3:
// the resolved tool result carries other: true exactly when the answer is not
// one of the question's options.
func TestInterruption_ResultMarksAnAnswerOutsideTheOptionsOther(t *testing.T) {
	it := &Interruption{}
	opts := json.RawMessage(`["Yes","No"]`)
	cases := []struct {
		name      string
		row       store.InterruptRow
		wantOther bool
	}{
		{"an option", store.InterruptRow{Options: opts, AllowOther: true, Answer: "Yes"}, false},
		{"outside the options", store.InterruptRow{Options: opts, AllowOther: true, Answer: "Wait a week"}, true},
		{"free text", store.InterruptRow{Answer: "Wait a week"}, false},
	}
	for _, c := range cases {
		c.row.InterruptID, c.row.Status = "intr_x", store.InterruptStatusResolved
		res := it.toolResultForStatus(c.row)
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("%s: result not JSON: %v", c.name, err)
		}
		_, has := out["other"]
		if has != c.wantOther || (has && out["other"] != true) {
			t.Errorf("%s: other = %v (present %v), want present=%v", c.name, out["other"], has, c.wantOther)
		}
		if out["answer"] != c.row.Answer {
			t.Errorf("%s: answer = %v", c.name, out["answer"])
		}
	}
}
