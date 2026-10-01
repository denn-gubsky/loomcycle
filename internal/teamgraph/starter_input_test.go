package teamgraph

import (
	"encoding/json"
	"strings"
	"testing"
)

// okInputStarter is the smallest valid input-source starter: one run per item
// of the walk's input, at most 4.
func okInputStarter() Handler {
	return Handler{
		Kind:   HandlerStarter,
		Source: &StarterSource{Kind: SourceInput},
		Fanout: &StarterFanout{Agent: "researcher", Per: FanoutPerMessage, Max: 4},
		Sink:   &StarterSink{Channel: "research"},
	}
}

func TestValidate_InputStarterAtEntryAccepted(t *testing.T) {
	if err := Validate(starterDef(okInputStarter())); err != nil {
		t.Fatalf("an input-source starter at the entry must validate: %v", err)
	}
	full := okInputStarter()
	full.Binds = map[string]string{"chunk_id": "$.chunk_id"}
	full.Schema = json.RawMessage(`{"type":"object","required":["chunk_id"]}`)
	full.Prompt = &StarterPrompt{Input: "Item: {{starter.message}}"}
	if err := Validate(starterDef(full)); err != nil {
		t.Fatalf("binds + schema + prompt on an input starter must validate: %v", err)
	}
	once := okInputStarter()
	once.Fanout.Per, once.Fanout.Max = FanoutPerOnce, 0
	once.Sink = nil
	if err := Validate(starterDef(once)); err != nil {
		t.Fatalf("a per=once input starter without a sink must validate: %v", err)
	}
	dflt := okInputStarter()
	dflt.Fanout.Per = "" // defaults to message
	if err := Validate(starterDef(dflt)); err != nil {
		t.Fatalf("per omitted on an input starter must validate as per=message: %v", err)
	}
}

// Each refusal is a field that would read as configured on an input source and
// do nothing — it reads no store, waits for nothing and has no cursor — or a
// wave shape that has no meaning for it. Each names the field.
func TestValidate_InputStarterRefusalsNameTheField(t *testing.T) {
	mut := func(f func(h *Handler)) Handler {
		h := okInputStarter()
		f(&h)
		return h
	}
	cases := []struct {
		name string
		h    Handler
		want string
	}{
		{"channel", mut(func(h *Handler) { h.Source.Channel = "pr-events" }), "`source.channel`"},
		{"path", mut(func(h *Handler) { h.Source.Path = "/specs/acme" }), "source.path/scope/select"},
		{"scope", mut(func(h *Handler) { h.Source.Scope = "user" }), "source.path/scope/select"},
		{"select", mut(func(h *Handler) { h.Source.Select = SelectChunks }), "source.path/scope/select"},
		{"wait", mut(func(h *Handler) { h.Source.Wait = WaitAny }), "source.wait/n/wait_ms"},
		{"n", mut(func(h *Handler) { h.Source.N = 2 }), "source.wait/n/wait_ms"},
		{"wait_ms", mut(func(h *Handler) { h.Source.WaitMS = 100 }), "source.wait/n/wait_ms"},
		{"batch", mut(func(h *Handler) { h.Source.Batch = 5 }), "source.batch"},
		{"ack", mut(func(h *Handler) { h.Ack = AckAfterResults }), "`ack`"},
		{"ack after_read", mut(func(h *Handler) { h.Ack = AckAfterRead }), "`ack`"},
		{"per=chunk", mut(func(h *Handler) { h.Fanout.Per = FanoutPerChunk }), "needs a document source"},
		{"per=message without max", mut(func(h *Handler) { h.Fanout.Max = 0 }), "requires `max` >= 1"},
		{"per=once with max", mut(func(h *Handler) { h.Fanout.Per = FanoutPerOnce }), "`max` means nothing"},
		{"binds with per=once", mut(func(h *Handler) {
			h.Fanout.Per, h.Fanout.Max = FanoutPerOnce, 0
			h.Binds = map[string]string{"x": "$.x"}
		}), "`binds` with per=once"},
		{"invalid schema", mut(func(h *Handler) { h.Schema = json.RawMessage(`{"type":`) }), "`schema` is not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(starterDef(c.h))
			if err == nil {
				t.Fatalf("accepted; want a refusal mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refused with %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// The walk's input is what the walk was started with, so only the entry can
// read it. A later state reading it would give "input" a second meaning.
func TestValidate_InputStarterNotAtEntryRefusedNamingTheState(t *testing.T) {
	d := Definition{
		Entry: "form",
		States: []State{
			{ID: "form", Handler: Handler{Kind: HandlerInput}},
			{ID: "wave", Handler: okInputStarter()},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{
			{From: "form", To: "wave", On: OnSuccess},
			{From: "wave", To: "done", On: OnSuccess},
		},
	}
	err := Validate(d)
	if err == nil {
		t.Fatal("a non-entry input-source starter was accepted")
	}
	if !strings.Contains(err.Error(), `state "wave"`) || !strings.Contains(err.Error(), "must be the definition's `entry`") {
		t.Errorf("err = %q, want it to name the state and say it must be the entry", err)
	}
}

// The form schema lives on the input starter (no input state can precede the
// entry), and only there among starters: on a channel or document starter it
// would read as a contract the runtime never consults.
func TestValidate_SchemaOnlyOnAnInputSourcedStarter(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	in := okInputStarter()
	in.Schema = schema
	if err := Validate(starterDef(in)); err != nil {
		t.Fatalf("schema on an input-source starter: %v", err)
	}
	ch := okStarter()
	ch.Schema = schema
	if err := Validate(starterDef(ch)); err == nil || !strings.Contains(err.Error(), "sets `schema`") {
		t.Errorf("schema on a channel starter: err = %v, want refused", err)
	}
	doc := okDocStarter()
	doc.Schema = schema
	if err := Validate(starterDef(doc)); err == nil || !strings.Contains(err.Error(), "sets `schema`") {
		t.Errorf("schema on a document starter: err = %v, want refused", err)
	}
	ag := Handler{Kind: HandlerAgent, Agent: "a", Schema: schema}
	if err := Validate(starterDef(ag)); err == nil || !strings.Contains(err.Error(), "sets `schema`") {
		t.Errorf("schema on an agent state: err = %v, want refused", err)
	}
}

func TestValidate_UnknownSourceKindListsInput(t *testing.T) {
	h := okInputStarter()
	h.Source.Kind = "webhook"
	err := Validate(starterDef(h))
	if err == nil || !strings.Contains(err.Error(), "want channel|document|input") {
		t.Errorf("err = %v, want the invalid-kind refusal to list input", err)
	}
}

// Adding the input kind must not move a document-sourced team's hash. The
// constant was computed from this definition BEFORE the kind existed.
func TestSign_DocumentStarterKeepsTheRecordedHash(t *testing.T) {
	d := Definition{
		Entry: "wave",
		States: []State{
			{ID: "wave", Handler: Handler{
				Kind:   HandlerStarter,
				Source: &StarterSource{Kind: SourceDocument, Path: "/specs/acme", Scope: "tenant", Select: SelectChunks},
				Fanout: &StarterFanout{Agent: "reviewer", Per: FanoutPerChunk, Max: 20},
				Sink:   &StarterSink{Channel: "verdicts"},
				Binds:  map[string]string{"title": "$.title"},
			}},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "wave", To: "done", On: OnSuccess}},
		Channels:    &TeamChannels{Publish: []string{"verdicts"}},
	}
	const before = "sha256:c35f881ed90e7cdfa11e8ebf8670cd22269bf0e7156643c2302203d10425a312"
	if got := Sign("t", d); got != before {
		t.Errorf("hash of a document-sourced starter = %s, want the recorded %s", got, before)
	}
}

// The source kind is what a team reads, so it is content.
func TestSign_InputSourceKindIsContent(t *testing.T) {
	in := starterDef(okInputStarter())
	ch := starterDef(okInputStarter())
	ch.States[0].Handler.Source = &StarterSource{Channel: "x"}
	if Sign("t", in) == Sign("t", ch) {
		t.Errorf("an input source hashed like a channel source")
	}
}

// An input source subscribes to nothing; only its sink is a channel reference.
func TestChannelRefs_InputSourceNamesNoChannel(t *testing.T) {
	refs := ChannelRefs(starterDef(okInputStarter()))
	if len(refs) != 1 || refs[0].Field != "sink" || refs[0].Channel != "research" {
		t.Errorf("ChannelRefs = %+v, want only the sink", refs)
	}
	// Even a stray channel name (refused by Validate) is not a subscription.
	h := okInputStarter()
	h.Source.Channel = "stray"
	for _, r := range ChannelRefs(starterDef(h)) {
		if r.Side == SideSubscribe {
			t.Errorf("an input source reported a subscription: %+v", r)
		}
	}
}
