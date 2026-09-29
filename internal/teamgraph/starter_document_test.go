package teamgraph

import (
	"strings"
	"testing"
)

// okDocStarter is the smallest valid document-source starter: one run per
// top-level section of /specs/acme, at most 20.
func okDocStarter() Handler {
	return Handler{
		Kind:   HandlerStarter,
		Source: &StarterSource{Kind: SourceDocument, Path: "/specs/acme"},
		Fanout: &StarterFanout{Agent: "reviewer", Per: FanoutPerChunk, Max: 20},
		Sink:   &StarterSink{Channel: "verdicts"},
	}
}

func TestValidate_DocumentStarterAccepted(t *testing.T) {
	if err := Validate(starterDef(okDocStarter())); err != nil {
		t.Fatalf("a per=chunk document starter must validate: %v", err)
	}
	full := okDocStarter()
	full.Source.Scope, full.Source.Select = "tenant", SelectChunks
	full.Binds = map[string]string{"title": "$.title"}
	if err := Validate(starterDef(full)); err != nil {
		t.Fatalf("scope=tenant + select=chunks + binds must validate: %v", err)
	}
	once := okDocStarter()
	once.Fanout.Per, once.Fanout.Max = FanoutPerOnce, 0
	if err := Validate(starterDef(once)); err != nil {
		t.Fatalf("a per=once document starter must validate: %v", err)
	}
}

// Each refusal is a field that would read as configured on a document source
// and do nothing — a document has no cursor and nothing to wait for — or a
// wave with no ceiling.
func TestValidate_DocumentStarterRefusals(t *testing.T) {
	mut := func(f func(h *Handler)) Handler {
		h := okDocStarter()
		f(&h)
		return h
	}
	cases := []struct {
		name string
		h    Handler
		want string
	}{
		{"ack", mut(func(h *Handler) { h.Ack = AckAfterResults }), "no cursor to acknowledge"},
		{"ack after_read", mut(func(h *Handler) { h.Ack = AckAfterRead }), "no cursor to acknowledge"},
		{"per=message", mut(func(h *Handler) { h.Fanout.Per = FanoutPerMessage }), "sections, not messages"},
		{"per omitted (defaults to message)", mut(func(h *Handler) { h.Fanout.Per = "" }), "sections, not messages"},
		{"per=chunk without max", mut(func(h *Handler) { h.Fanout.Max = 0 }), "per=chunk requires `max` >= 1"},
		{"per=once with max", mut(func(h *Handler) { h.Fanout.Per = FanoutPerOnce }), "`max` means nothing"},
		{"wait", mut(func(h *Handler) { h.Source.Wait = WaitAny }), "nothing to wait for"},
		{"n", mut(func(h *Handler) { h.Source.N = 2 }), "nothing to wait for"},
		{"wait_ms", mut(func(h *Handler) { h.Source.WaitMS = 100 }), "nothing to wait for"},
		{"batch", mut(func(h *Handler) { h.Source.Batch = 5 }), "remove source.batch"},
		{"channel too", mut(func(h *Handler) { h.Source.Channel = "pr-events" }), "`source.channel` means nothing"},
		{"no path", mut(func(h *Handler) { h.Source.Path = "" }), "requires `source.path`"},
		{"relative path", mut(func(h *Handler) { h.Source.Path = "specs/acme" }), "must be an absolute document path"},
		{"root path", mut(func(h *Handler) { h.Source.Path = "/" }), "must be an absolute document path"},
		{"variable path", mut(func(h *Handler) { h.Source.Path = "/specs/${var.team}" }), "must be a fixed path"},
		{"placeholder path", mut(func(h *Handler) { h.Source.Path = "/specs/{{run.user_id}}" }), "must be a fixed path"},
		{"agent scope", mut(func(h *Handler) { h.Source.Scope = "agent" }), "invalid scope"},
		{"unknown select", mut(func(h *Handler) { h.Source.Select = "all" }), "invalid select"},
		{"unknown kind", mut(func(h *Handler) { h.Source.Kind = "webhook" }), "invalid kind"},
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

// per=chunk has no meaning over a channel, and the document fields on a
// channel source would be ignored.
func TestValidate_ChannelStarterRefusesDocumentShapes(t *testing.T) {
	chunk := okStarter()
	chunk.Fanout.Per = FanoutPerChunk
	if err := Validate(starterDef(chunk)); err == nil || !strings.Contains(err.Error(), "needs a document source") {
		t.Errorf("per=chunk on a channel source: err = %v", err)
	}
	withPath := okStarter()
	withPath.Source.Path = "/specs/acme"
	if err := Validate(starterDef(withPath)); err == nil || !strings.Contains(err.Error(), "only a document source reads") {
		t.Errorf("path on a channel source: err = %v", err)
	}
	explicit := okStarter()
	explicit.Source.Kind = SourceChannel
	if err := Validate(starterDef(explicit)); err != nil {
		t.Errorf("kind=channel spelled out must validate: %v", err)
	}
}

// Adding the document fields must not move a single existing hash: every
// recorded content_sha256 of a channel-sourced team depends on it. The
// constant was computed from this definition BEFORE the fields existed.
func TestSign_ChannelStarterKeepsTheRecordedHash(t *testing.T) {
	d := Definition{
		Entry: "wave",
		States: []State{
			{ID: "wave", Handler: Handler{
				Kind:   HandlerStarter,
				Source: &StarterSource{Channel: "pr-events", Wait: WaitAtLeast, N: 2, WaitMS: 500, Batch: 4},
				Fanout: &StarterFanout{Agent: "reviewer", Per: FanoutPerMessage, Max: 8},
				Sink:   &StarterSink{Channel: "verdicts"},
				Ack:    AckAfterRead,
				Binds:  map[string]string{"pr": "$.pr"},
			}},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{{From: "wave", To: "done", On: OnSuccess}},
		Channels:    &TeamChannels{Publish: []string{"verdicts"}, Subscribe: []string{"pr-events"}},
	}
	const before = "sha256:9ccfbe5e0a3dc5c5bdb29e0724129d372e9a68e7f98f8855ad16d2821cd772f7"
	if got := Sign("t", d); got != before {
		t.Errorf("hash of a channel-sourced starter = %s, want the recorded %s — "+
			"a new StarterSource field is missing omitempty", got, before)
	}
}

// Which document a team reads is what it does, so it is content.
func TestSign_DocumentPathIsContent(t *testing.T) {
	a, b := starterDef(okDocStarter()), starterDef(okDocStarter())
	b.States[0].Handler.Source.Path = "/specs/other"
	if Sign("t", a) == Sign("t", b) {
		t.Errorf("changing the document path did not change the hash")
	}
}

// A document source subscribes to nothing, so it must not ask the preflight
// for a subscribe grant. Its sink is still a channel and still reported.
func TestChannelRefs_DocumentSourceNamesNoChannel(t *testing.T) {
	refs := ChannelRefs(starterDef(okDocStarter()))
	if len(refs) != 1 || refs[0].Field != "sink" || refs[0].Channel != "verdicts" {
		t.Errorf("ChannelRefs = %+v, want only the sink", refs)
	}
	// Even a stray channel name on a document source (refused by Validate) is
	// not reported as a subscription.
	h := okDocStarter()
	h.Source.Channel = "stray"
	for _, r := range ChannelRefs(starterDef(h)) {
		if r.Side == SideSubscribe {
			t.Errorf("a document source reported a subscription: %+v", r)
		}
	}
}
