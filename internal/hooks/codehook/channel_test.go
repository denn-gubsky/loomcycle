package codehook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

func channelHook(code string) *hooks.Hook {
	return &hooks.Hook{ID: "hook_c", Owner: hooks.ChannelOwner("inbox"), Name: "screen", Phase: hooks.PhaseChannelPublish, Code: code, Timeout: time.Second}
}

func message(id, body string) hooks.ChannelHookCall {
	return hooks.ChannelHookCall{Phase: hooks.PhaseChannelPublish, Channel: "inbox", MessageID: id, Attempt: 1, Body: json.RawMessage(body)}
}

// A channel hook's body reads the message and returns a channel decision. The
// decision decodes strictly, so before it had an updated_body field a body
// that rewrote a message failed as a misspelt decision.
func TestCodeHook_AChannelDecisionDecodes(t *testing.T) {
	h := channelHook(`function hook(ev) {
		if (ev.event !== "channel_publish") return {decision: "drop", reason: "wrong event " + ev.event};
		if (ev.body.text.indexOf("spam") >= 0) return {decision: "drop", reason: "spam in " + ev.message_id};
		return {decision: "release", updated_body: {text: ev.body.text.replace("secret", "[redacted]")}};
	}`)
	r := New(nil)
	got, err := r.Run(context.Background(), h, "channel_publish", message("m1", `{"text":"buy spam"}`))
	if err != nil || got.Decision != "drop" || got.Reason != "spam in m1" {
		t.Fatalf("drop: got %+v, %v", got, err)
	}
	got, err = r.Run(context.Background(), h, "channel_publish", message("m2", `{"text":"the secret"}`))
	if err != nil || got.Decision != "release" || string(got.UpdatedBody) != `{"text":"the [redacted]"}` {
		t.Fatalf("rewrite: got %+v (%s), %v", got, got.UpdatedBody, err)
	}
}

// Two messages draw different random sequences, and one message the same one
// on every call. A channel hook has no run or tool call to seed from, so
// without the message id every message drew the same numbers.
func TestCodeHook_EachMessageHasItsOwnSeed(t *testing.T) {
	h := channelHook(`function hook(ev) { return {decision: "release", updated_body: {r: Math.random()}}; }`)
	r := New(nil)
	draw := func(id string) string {
		t.Helper()
		got, err := r.Run(context.Background(), h, "channel_publish", message(id, `{}`))
		if err != nil {
			t.Fatal(err)
		}
		return string(got.UpdatedBody)
	}
	a1, b, a2 := draw("m1"), draw("m2"), draw("m1")
	if a1 == b {
		t.Errorf("two messages drew the same number: %s", a1)
	}
	if a1 != a2 {
		t.Errorf("one message drew %s, then %s", a1, a2)
	}
}
