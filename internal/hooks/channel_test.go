package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func channelHook(url string) *Hook {
	return &Hook{Owner: ChannelOwner("inbox"), Name: "screen", Phase: PhaseChannelPublish, CallbackURL: url}
}

func channelCall() ChannelHookCall {
	return ChannelHookCall{Channel: "inbox", Scope: "tenant", MessageID: "m1", Attempt: 1,
		PublishedAt: time.Unix(1700000000, 0).UTC(), Body: json.RawMessage(`{"text":"hi"}`)}
}

// A channel hook registers into a Set. The phase switch that validates a hook
// refused every phase it did not list, so a channel's chain could not be built.
func TestSet_RegistersAChannelHook(t *testing.T) {
	s := NewSet()
	if _, err := s.Register(channelHook("https://h.example")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := s.List(); len(got) != 1 || got[0].Phase != PhaseChannelPublish {
		t.Fatalf("set = %+v", got)
	}
}

// A channel hook runs on every message of its channel; a selector for a run's
// agent or tool would be a filter that can never apply.
func TestSet_AChannelHookHasNoAgentOrToolSelector(t *testing.T) {
	for name, h := range map[string]*Hook{
		"tools":  {Owner: "channel:c", Name: "n", Phase: PhaseChannelPublish, CallbackURL: "https://h.example", Tools: []string{"Read"}},
		"agents": {Owner: "channel:c", Name: "n", Phase: PhaseChannelPublish, CallbackURL: "https://h.example", Agents: []string{"a"}},
	} {
		if _, err := NewSet().Register(h); err == nil {
			t.Errorf("%s: a channel hook with a %s selector was accepted", name, name)
		}
	}
}

// A code body is told it decides on a channel message. The default of
// eventFor named every unlisted phase post_tool_use, so a channel hook's body
// was handed "post_tool_use".
func TestEventFor_ChannelPublish(t *testing.T) {
	if got := eventFor(PhaseChannelPublish); got != "channel_publish" {
		t.Fatalf("eventFor(channel_publish) = %q", got)
	}
	for p, want := range map[Phase]string{PhasePre: "pre_tool_use", PhasePost: "post_tool_use", PhasePostFailure: "post_tool_use_failure", PhaseAgentStop: "agent_stop"} {
		if got := eventFor(p); got != want {
			t.Errorf("eventFor(%s) = %q, want %q", p, got, want)
		}
	}
}

// A HookDef may answer channel_publish; like the run events it has no tool to
// match.
func TestDefValidate_ChannelPublishIsAnEventWithNoTools(t *testing.T) {
	d := Def{Event: PhaseChannelPublish, Body: DefBody{Kind: BodyKindCode, Code: "function hook(ev){return {}}"}}
	if err := d.Validate(); err != nil {
		t.Fatalf("channel_publish: %v", err)
	}
	d.Match = &DefMatch{Tools: []string{"Read"}}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "match.tools") {
		t.Fatalf("channel_publish with match.tools = %v; want refused", err)
	}
	if err := (Def{Event: "channel_pub", Body: d.Body}).Validate(); err == nil || !strings.Contains(err.Error(), "channel_publish") {
		t.Fatalf("an unknown event's error = %v; want it to list channel_publish", err)
	}
}

// channel_publish is a channel's event: an agent's, a tool's or a run's hooks
// cannot carry it, and a channel's hooks carry nothing else.
func TestChannelPublish_OnlyAChannelCarriesIt(t *testing.T) {
	onChannel := EventHooks{PhaseChannelPublish: {{Ref: "screen"}}}
	if err := onChannel.Validate(""); err == nil || !strings.Contains(err.Error(), "channel's hooks") {
		t.Errorf("an agent carrying channel_publish = %v; want refused", err)
	}
	if err := ValidateChannelHooks(onChannel); err != nil {
		t.Errorf("a channel carrying channel_publish: %v", err)
	}
	if err := ValidateChannelHooks(EventHooks{PhaseAgentStop: {{Ref: "screen"}}}); err == nil {
		t.Error("a channel carrying agent_stop was accepted")
	}
	if err := ValidateChannelHooks(EventHooks{PhaseChannelPublish: {{Ref: "bad@name@"}}}); err == nil {
		t.Error("a malformed reference was accepted")
	}
}

// A channel's references resolve in the channel's tenant, then in the shared
// one; a reference to a HookDef of another event, or to none, fails the whole
// chain.
func TestResolveChannel_ResolvesInTheChannelsTenant(t *testing.T) {
	defs := map[string]Def{
		"acme|screen": {Event: PhaseChannelPublish, Body: DefBody{Kind: BodyKindHTTP, URL: "https://acme.example"}, FailMode: FailClosed},
		"|redact":     {Event: PhaseChannelPublish, Body: DefBody{Kind: BodyKindCode, Code: "function hook(ev){}"}},
		"|gate":       {Event: PhasePre, Body: DefBody{Kind: BodyKindHTTP, URL: "https://h.example"}},
		"other|x":     {Event: PhaseChannelPublish, Body: DefBody{Kind: BodyKindHTTP, URL: "https://other.example"}},
	}
	src := Source{Owner: ChannelOwner("inbox"), Tenant: "acme"}
	set := NewSet()
	if err := ResolveChannel(context.Background(), src, EventHooks{PhaseChannelPublish: {{Ref: "screen"}, {Ref: "redact"}}}, fakeLookup(defs), set); err != nil {
		t.Fatalf("ResolveChannel: %v", err)
	}
	got := set.List()
	if len(got) != 2 || got[0].Name != "screen" || got[0].Tenant != "acme" || got[0].FailMode != FailClosed ||
		got[1].Name != "redact" || got[1].Tenant != "" || !got[1].IsCode() || got[0].Owner != "channel:inbox" {
		t.Fatalf("chain = %+v", got)
	}
	for ref, want := range map[string]string{"gate": "answers pre", "x": "not found", "nothere": "not found"} {
		err := ResolveChannel(context.Background(), src, EventHooks{PhaseChannelPublish: {{Ref: ref}}}, fakeLookup(defs), NewSet())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", ref, err, want)
		}
	}
}

// Each decision a webhook can return reaches the worker as it was meant; an
// empty response releases the message unchanged.
func TestInvokeChannel_WebhookDecisions(t *testing.T) {
	for _, tc := range []struct {
		resp, decision, kind, body, reason string
	}{
		{``, "release", "release", "", ""},
		{`{"decision":"release"}`, "release", "release", "", ""},
		{`{"updated_body":{"text":"[redacted]"}}`, "release", "rewrite_body", `{"text":"[redacted]"}`, ""},
		{`{"decision":"release","updated_body":null}`, "release", "release", "", ""},
		{`{"decision":"drop","reason":" spam "}`, "drop", "drop", "", "spam"},
		{`{"decision":"hold","reason":"needs a look"}`, "hold", "hold", "", "needs a look"},
	} {
		f := newFakeHook(t, tc.resp)
		got, err := NewDispatcher(nil, nil).InvokeChannel(context.Background(), channelHook(f.srv.URL), channelCall())
		if err != nil {
			t.Errorf("%s: %v", tc.resp, err)
			continue
		}
		if got.Decision != tc.decision || got.Kind() != tc.kind || string(got.UpdatedBody) != tc.body || got.Reason != tc.reason {
			t.Errorf("%s: got %+v (kind %s)", tc.resp, got, got.Kind())
		}
	}
}

// The payload is the message and where it was published, stamped with the
// hook it goes to.
func TestInvokeChannel_PayloadCarriesTheMessage(t *testing.T) {
	f := newFakeHook(t, ``)
	if _, err := NewDispatcher(nil, nil).InvokeChannel(context.Background(), channelHook(f.srv.URL), channelCall()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"phase":"channel_publish"`, `"owner":"channel:inbox"`, `"hook_name":"screen"`,
		`"channel":"inbox"`, `"message_id":"m1"`, `"attempt":1`, `"body":{"text":"hi"}`} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("payload lacks %s: %s", want, f.bodies[0])
		}
	}
}

// A response that does not say one thing is a failed hook, not a release: a
// mistake in a gate must not let the message through.
func TestInvokeChannel_AnAmbiguousAnswerFails(t *testing.T) {
	for _, resp := range []string{
		`{"decision":"allow"}`,
		`{"decision":"drop","updated_body":{"x":1}}`,
		`{"decision":"hold","updated_body":"x"}`,
		`not json`,
	} {
		f := newFakeHook(t, resp)
		if got, err := NewDispatcher(nil, nil).InvokeChannel(context.Background(), channelHook(f.srv.URL), channelCall()); err == nil {
			t.Errorf("%s: accepted as %+v", resp, got)
		}
	}
	down := newFakeHook(t, `oops`)
	down.status = 500
	_, err := NewDispatcher(nil, nil).InvokeChannel(context.Background(), channelHook(down.srv.URL), channelCall())
	if err == nil || DecisionReason(err) != "the hook returned status 500" {
		t.Errorf("a 500: err = %v, reason %q", err, DecisionReason(err))
	}
}

// Only a channel_publish hook decides on a channel message.
func TestInvokeChannel_RefusesAnotherPhase(t *testing.T) {
	h := &Hook{Owner: "agent:a", Name: "gate", Phase: PhasePre, CallbackURL: "https://h.example"}
	if _, err := NewDispatcher(nil, nil).InvokeChannel(context.Background(), h, channelCall()); err == nil {
		t.Fatal("a pre hook was run on a channel message")
	}
}

// A code body decides the same way, and is told it decides on a
// channel_publish; a field of another phase is refused.
func TestInvokeChannel_ACodeHookDecides(t *testing.T) {
	run := &fakeCodeRunner{decide: func(context.Context) (CodeDecision, error) {
		return CodeDecision{Decision: "release", UpdatedBody: json.RawMessage(`{"text":"clean"}`)}, nil
	}}
	d := NewDispatcher(nil, nil)
	d.SetCodeRunner(run)
	h := &Hook{Owner: ChannelOwner("inbox"), Name: "redact", Phase: PhaseChannelPublish, Code: "function hook(ev){}", Timeout: time.Second}
	got, err := d.InvokeChannel(context.Background(), h, channelCall())
	if err != nil || got.Kind() != "rewrite_body" || string(got.UpdatedBody) != `{"text":"clean"}` {
		t.Fatalf("got %+v, %v", got, err)
	}
	if len(run.events) != 1 || run.events[0] != "channel_publish" {
		t.Fatalf("the body was told %v", run.events)
	}
	run.decide = func(context.Context) (CodeDecision, error) {
		return CodeDecision{AdditionalContext: "x"}, nil
	}
	if _, err := d.InvokeChannel(context.Background(), h, channelCall()); err == nil {
		t.Fatal("a channel hook returning additional_context was accepted")
	}
	run.decide = func(context.Context) (CodeDecision, error) { return CodeDecision{}, errors.New("boom") }
	if _, err := d.InvokeChannel(context.Background(), h, channelCall()); err == nil {
		t.Fatal("a failed body was read as a decision")
	}
}

// updated_body is a channel hook's field; any other hook returning it made a
// mistake, which is reported rather than ignored.
func TestCodeDecision_UpdatedBodyOnlyOnAChannelHook(t *testing.T) {
	d := CodeDecision{UpdatedBody: json.RawMessage(`{}`)}
	if _, err := d.preResult(&Hook{}); err == nil {
		t.Error("pre accepted updated_body")
	}
	if _, err := d.postResult(); err == nil {
		t.Error("post accepted updated_body")
	}
	if _, err := d.lifecycleResult(); err == nil {
		t.Error("a lifecycle hook accepted updated_body")
	}
}
