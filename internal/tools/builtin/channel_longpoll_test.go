package builtin

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

// F22: the dedup guard logs a wait_ms truncation only the first time per channel.
func TestChannel_ShouldWarnTruncationDedup(t *testing.T) {
	c := &Channel{}
	if !c.shouldWarnTruncation("findings") {
		t.Fatal("first truncation on a channel should warn")
	}
	if c.shouldWarnTruncation("findings") {
		t.Fatal("second truncation on the same channel should be deduped")
	}
	if !c.shouldWarnTruncation("alerts") {
		t.Fatal("a different channel should warn independently")
	}
}

// F22 end-to-end: a subscribe whose wait_ms exceeds the operator cap is
// truncated and surfaces a one-time WARNING per channel (not on every
// re-subscribe); a within-cap wait_ms does not warn.
func TestChannelTool_SubscribeWaitMSTruncationWarnsOnce(t *testing.T) {
	tool, ctx, cleanup := channelFixture(t)
	defer cleanup()
	tool.LongPollCapMS = 20 // tiny cap so wait_ms=5000 truncates fast

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	over := json.RawMessage(`{"op":"subscribe","channel":"findings","wait_ms":5000,"max_messages":10}`)
	for i := 0; i < 2; i++ {
		if res, _ := tool.Execute(ctx, over); res.IsError {
			t.Fatalf("over-cap subscribe %d: %s", i, res.Text)
		}
	}
	if n := strings.Count(buf.String(), "truncated to the operator cap"); n != 1 {
		t.Fatalf("want exactly 1 truncation warning across 2 over-cap subscribes, got %d:\n%s", n, buf.String())
	}

	// A within-cap wait_ms must NOT warn (different channel to avoid the dedup).
	buf.Reset()
	within := json.RawMessage(`{"op":"subscribe","channel":"alerts","wait_ms":10,"max_messages":10}`)
	if res, _ := tool.Execute(ctx, within); res.IsError {
		t.Fatalf("within-cap subscribe: %s", res.Text)
	}
	if strings.Contains(buf.String(), "truncated to the operator cap") {
		t.Errorf("within-cap wait_ms should not warn:\n%s", buf.String())
	}
}
