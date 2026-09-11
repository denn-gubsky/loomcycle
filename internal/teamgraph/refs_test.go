package teamgraph

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestChannelRefs_CoversEveryChannelBearingField is the guard on the whole
// preflight: every check downstream walks ChannelRefs, so a channel-bearing
// field added to Handler without a line in ChannelRefs silently stops being
// checked — the definition would validate, save, and fail at run time, which
// is the exact failure this phase exists to remove.
//
// It reads Handler's own FIELDS rather than a hand-written list, so adding one
// reds this test instead of quietly widening the hole.
func TestChannelRefs_CoversEveryChannelBearingField(t *testing.T) {
	// The fields of Handler (and its nested starter structs) that name a
	// channel. Derived from the struct, so a new one appears here on its own.
	var bearing []string
	ht := reflect.TypeOf(Handler{})
	for i := 0; i < ht.NumField(); i++ {
		f := ht.Field(i)
		switch f.Type.Kind() {
		case reflect.String:
			if strings.Contains(strings.ToLower(f.Name), "channel") {
				bearing = append(bearing, f.Name)
			}
		case reflect.Ptr:
			// A starter sub-struct carrying a Channel field is a channel site.
			st := f.Type.Elem()
			if st.Kind() != reflect.Struct {
				continue
			}
			if _, ok := st.FieldByName("Channel"); ok {
				bearing = append(bearing, f.Name)
			}
		}
	}
	if len(bearing) == 0 {
		t.Fatal("found no channel-bearing fields on Handler — the derivation broke, not the code")
	}

	// One state per bearing field, each naming a distinct channel, so a missed
	// field shows up as a missing name rather than as a count that happens to
	// match.
	def := Definition{Entry: "s0", States: []State{
		{ID: "src", Handler: Handler{Kind: HandlerStarter,
			Source: &StarterSource{Channel: "ch-source"},
			Sink:   &StarterSink{Channel: "ch-sink"}}},
		{ID: "pub", Handler: Handler{Kind: HandlerChannel, Channel: "ch-channel"}},
	}}
	got := map[string]bool{}
	for _, r := range ChannelRefs(def) {
		got[r.Channel] = true
	}
	for name, want := range map[string]string{
		"Source": "ch-source", "Sink": "ch-sink", "Channel": "ch-channel",
	} {
		if !got[want] {
			t.Errorf("Handler.%s names a channel that ChannelRefs does not report (%q) — "+
				"add it to ChannelRefs or the preflight silently stops checking it", name, want)
		}
	}
	for _, f := range bearing {
		if _, covered := map[string]bool{"Source": true, "Sink": true, "Channel": true}[f]; !covered {
			t.Errorf("Handler.%s is a NEW channel-bearing field with no case in ChannelRefs "+
				"(and no coverage in this test) — every channel a definition names must be enumerated", f)
		}
	}
}

// TestChannelRefs_CarriesSideStateAndField: a refusal is only actionable if it
// says which node, which field, and which grant.
func TestChannelRefs_CarriesSideStateAndField(t *testing.T) {
	def := Definition{States: []State{
		{ID: "wave", Handler: Handler{Kind: HandlerStarter,
			Source: &StarterSource{Channel: "in"}, Sink: &StarterSink{Channel: "out"}}},
		{ID: "notify", Handler: Handler{Kind: HandlerChannel, Channel: "out"}},
	}}
	want := []ChannelRef{
		{"in", SideSubscribe, "wave", "source"},
		{"out", SidePublish, "wave", "sink"},
		{"out", SidePublish, "notify", "channel"},
	}
	got := ChannelRefs(def)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChannelRefs =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestChannelRefs_KeepsDuplicates: one channel used as a source in one state
// and a sink in another needs BOTH grants; collapsing by name would hide one.
func TestChannelRefs_KeepsDuplicates(t *testing.T) {
	def := Definition{States: []State{
		{ID: "a", Handler: Handler{Kind: HandlerStarter, Source: &StarterSource{Channel: "loop"}}},
		{ID: "b", Handler: Handler{Kind: HandlerChannel, Channel: "loop"}},
	}}
	refs := ChannelRefs(def)
	if len(refs) != 2 || refs[0].Side == refs[1].Side {
		t.Fatalf("both uses of one channel must survive with their own side: %+v", refs)
	}
}

func TestAgentRefs_CoversEveryAgentBearingField(t *testing.T) {
	def := Definition{States: []State{
		{ID: "one", Handler: Handler{Kind: HandlerAgent, Agent: "a1", Consolidator: "c1"}},
		{ID: "par", Handler: Handler{Kind: HandlerParallel, Agents: []string{"a2", "a3"}}},
		{ID: "wave", Handler: Handler{Kind: HandlerStarter,
			Fanout: &StarterFanout{Agent: "a4", Agents: []string{"a5"}}}},
	}}
	got := map[string]string{}
	for _, r := range AgentRefs(def) {
		got[r.Agent] = r.Field
	}
	for agent, field := range map[string]string{
		"a1": "agent", "c1": "consolidator", "a2": "agents", "a3": "agents",
		"a4": "fanout.agent", "a5": "fanout.agents",
	} {
		if got[agent] != field {
			t.Errorf("AgentRefs missed %q (want field %q, got %q)", agent, field, got[agent])
		}
	}
}

// TestGrantList_NilChannelsGrantsNothing pins the case that strands workflows:
// a definition with a Starter and NO channels block parses, validates, and then
// refuses its own source the first time it runs.
func TestGrantList_NilChannelsGrantsNothing(t *testing.T) {
	var def Definition
	if got := def.GrantList(SidePublish); got != nil {
		t.Errorf("GrantList on a nil Channels block = %v, want nil", got)
	}
	if got := def.GrantList(SideSubscribe); got != nil {
		t.Errorf("GrantList on a nil Channels block = %v, want nil", got)
	}
	def.Channels = &TeamChannels{Publish: []string{"p"}, Subscribe: []string{"s"}}
	if got := def.GrantList(SidePublish); len(got) != 1 || got[0] != "p" {
		t.Errorf("GrantList(publish) = %v", got)
	}
	if got := def.GrantList(SideSubscribe); len(got) != 1 || got[0] != "s" {
		t.Errorf("GrantList(subscribe) = %v", got)
	}
}

// TestChannelRefs_SurvivesJSONRoundTrip: the sweep reads a STORED definition,
// so the refs must come out of parsed JSON, not only out of a Go literal.
func TestChannelRefs_SurvivesJSONRoundTrip(t *testing.T) {
	raw := `{"entry":"wave","states":[
	  {"state":"wave","handler":{"kind":"starter","source":{"channel":"in"},
	    "fanout":{"agent":"r","per":"message","max":2},"sink":{"channel":"out"}}},
	  {"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"wave","to":"done","on":"success"}]}`
	var def Definition
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := len(ChannelRefs(def)); got != 2 {
		t.Errorf("ChannelRefs over parsed JSON = %d refs, want 2", got)
	}
	if got := len(AgentRefs(def)); got != 1 {
		t.Errorf("AgentRefs over parsed JSON = %d refs, want 1", got)
	}
}
