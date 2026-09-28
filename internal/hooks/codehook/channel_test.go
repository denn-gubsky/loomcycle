package codehook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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

// fakeSession is an AskSession over memory that counts what the runner does.
type fakeSession struct {
	recorded      []hooks.AskRecord
	began, waited int
	resumed       int
	anchor        int64
}

func (s *fakeSession) Recorded() []hooks.AskRecord { return s.recorded }
func (s *fakeSession) Record(r hooks.AskRecord) error {
	s.recorded = append(s.recorded, r)
	return nil
}
func (s *fakeSession) Begin(ctx context.Context) (context.Context, error) {
	s.began++
	return tools.WithRunID(ctx, "run_hook"), nil
}
func (s *fakeSession) Wait() func() { s.waited++; return func() { s.resumed++ } }
func (s *fakeSession) Anchor(now int64) int64 {
	if s.anchor == 0 {
		s.anchor = now
	}
	return s.anchor
}

// An answer a person already gave is replayed from the session, not asked
// again — the body decides on it as if it had just been answered. Without the
// session the answers lived only in the invocation's memory, so a restart
// asked the same question twice.
func TestCodeHook_AskedAnswerReplaysFromTheJournal(t *testing.T) {
	h := channelHook(`function hook(ev) {
		var a = Interruption.ask({question: "deliver " + ev.message_id + "?", options: ["yes", "no"]});
		return a === "yes" ? {decision: "release"} : {decision: "drop", reason: "a person said no"};
	}`)
	fi := &fakeInterruption{answer: func(int, string) tools.Result { return answered("no") }}
	sess := &fakeSession{recorded: []hooks.AskRecord{{
		Input: json.RawMessage(`{"op":"ask","options":["yes","no"],"question":"deliver m1?"}`),
		Text:  `{"interrupt_id":"int_old","answer":"yes"}`,
	}}}
	got, err := New(fi).Run(hooks.WithAskSession(context.Background(), sess), h, "channel_publish", message("m1", `{}`))
	if err != nil || got.Decision != "release" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(fi.inputs) != 0 || sess.began != 0 {
		t.Fatalf("asked again (%d calls, %d runs opened)", len(fi.inputs), sess.began)
	}
}

// A new ask goes through the session: under the run it opens, with the slot
// given up while a person decides, and the answer kept before the body runs
// again.
func TestCodeHook_ANewAskGoesThroughTheSession(t *testing.T) {
	h := channelHook(`function hook(ev) {
		var a = Interruption.ask({question: "ok?", options: ["yes", "no"]});
		return a === "yes" ? {decision: "release"} : {decision: "drop"};
	}`)
	fi := &fakeInterruption{answer: func(int, string) tools.Result { return answered("yes") }}
	sess := &fakeSession{}
	got, err := New(fi).Run(hooks.WithAskSession(context.Background(), sess), h, "channel_publish", message("m1", `{}`))
	if err != nil || got.Decision != "release" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if sess.began != 1 || sess.waited != 1 || sess.resumed != 1 || len(sess.recorded) != 1 {
		t.Fatalf("session: began %d, waited %d, resumed %d, recorded %d", sess.began, sess.waited, sess.resumed, len(sess.recorded))
	}
}
