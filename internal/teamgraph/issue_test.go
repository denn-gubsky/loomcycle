package teamgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// where is the part of an Issue an editor uses to place it.
type where struct{ Kind, Path, State, Field string }

func wheres(is []*Issue) []where {
	out := make([]where, len(is))
	for i, x := range is {
		out[i] = where{x.Kind, x.Path, x.State, x.Field}
	}
	return out
}

// TestValidateAll_ReportsEveryViolationInCheckOrder: four areas broken at once
// (top level, a state's handler, local, a transition) all come back, in the
// order Validate has always checked them, each addressed.
func TestValidateAll_ReportsEveryViolationInCheckOrder(t *testing.T) {
	d := Definition{
		Entry:         "a",
		MaxIterations: -1,
		Vars:          map[string]string{"tone": "{{x}}"},
		States: []State{
			{ID: "a", Handler: Handler{Kind: HandlerAgent, Agent: "./ghost"}},
			{ID: "wave", Handler: Handler{Kind: HandlerStarter, Source: &StarterSource{Channel: "in"},
				Fanout: &StarterFanout{Agent: "w", Per: FanoutPerMessage}}},
			{ID: "done", Handler: Handler{Kind: HandlerTerminal}},
		},
		Transitions: []Transition{
			{From: "a", To: "wave", On: OnSuccess},
			{From: "wave", To: "done", On: "maybe"},
		},
		Local: &Local{Agents: map[string]json.RawMessage{"bad name": json.RawMessage(`{}`)}},
	}
	got := ValidateAll(d)
	want := []where{
		{"", "max_iterations", "", ""},
		{"", "vars.tone", "", ""},
		{"", "states[1].handler.fanout.max", "wave", "fanout.max"},
		{"", `local.agents["bad name"]`, "", ""},
		{IssueLocalAgentMissing, "states[0].handler.agent", "a", "agent"},
		{"", "transitions[1].on", "", ""},
	}
	if !reflect.DeepEqual(wheres(got), want) {
		t.Fatalf("ValidateAll =\n  %+v\nwant\n  %+v\nmessages: %v", wheres(got), want, got)
	}
	if err := Validate(d); err == nil || err.Error() != got[0].Msg {
		t.Errorf("Validate = %v, want the first issue %q", err, got[0].Msg)
	}
	for _, is := range got {
		if !strings.HasPrefix(is.Msg, "team definition: ") || is.Error() != is.Msg {
			t.Errorf("issue %s: Error() = %q, Msg = %q", is.Path, is.Error(), is.Msg)
		}
	}
}

// TestValidateAll_AddressesEachValidatorFamily pins the path of one
// representative violation of every family of check.
func TestValidateAll_AddressesEachValidatorFamily(t *testing.T) {
	agentState := func(d *Definition) *Handler { return &d.States[0].Handler }
	cases := []struct {
		name   string
		mutate func(d *Definition)
		want   where
		msg    string // a substring of the issue's message
	}{
		{"empty entry", func(d *Definition) { d.Entry = "" },
			where{"", "entry", "", ""}, "`entry` is required"},
		{"no states", func(d *Definition) { d.States = nil; d.Transitions = nil },
			where{"", "states", "", ""}, "at least one state"},
		{"max_iterations", func(d *Definition) { d.MaxIterations = 5000 },
			where{"", "max_iterations", "", ""}, "exceeds the maximum"},
		{"walk hook event", func(d *Definition) { d.Hooks = hooks.EventHooks{hooks.PhasePre: {{Ref: "gate"}}} },
			where{"", "hooks.pre", "", ""}, "only run_end fires"},
		{"walk hook entry", func(d *Definition) {
			d.Hooks = hooks.EventHooks{hooks.PhaseRunEnd: {{Ref: "gate"}, {Ref: "gate@x"}}}
		}, where{"", "hooks.run_end[1]", "", ""}, "the version after @"},
		{"state hooks on a kind that starts no run", func(d *Definition) {
			d.States[1].Handler.ToolHooks = hooks.ToolHooks{"Bash": {hooks.PhasePre: {{Ref: "gate"}}}}
		}, where{"", "states[1].handler.tool_hooks", "done", "tool_hooks"}, "starts no run"},
		{"state hook entry", func(d *Definition) {
			agentState(d).Hooks = hooks.EventHooks{hooks.PhasePre: {{Ref: "ok"}, {Ref: ""}}}
		}, where{"", "states[0].handler.hooks.pre[1]", "review", "hooks.pre[1]"}, "name is required"},
		{"state tool hook", func(d *Definition) {
			agentState(d).ToolHooks = hooks.ToolHooks{"Bash": {hooks.PhaseRunEnd: {{Ref: "gate"}}}}
		}, where{"", "states[0].handler.tool_hooks.Bash", "review", "tool_hooks.Bash"}, "is a run event"},
		{"var key", func(d *Definition) { d.Vars = map[string]string{"has space": "x"} },
			where{"", `vars["has space"]`, "", ""}, "vars key"},
		{"empty state id", func(d *Definition) { d.States[1].ID = "" },
			where{"", "states[1].state", "", ""}, "empty `state` id"},
		{"duplicate state id", func(d *Definition) { d.States[1].ID = "review" },
			where{"", "states[1].state", "review", ""}, "duplicate state id"},
		{"handler kind", func(d *Definition) { agentState(d).Kind = "delay" },
			where{"", "states[0].handler.kind", "review", "kind"}, "unknown handler kind"},
		{"parallel member", func(d *Definition) {
			*agentState(d) = Handler{Kind: HandlerParallel, Agents: []string{"a", " "}, Consolidator: "c"}
		}, where{"", "states[0].handler.agents[1]", "review", "agents[1]"}, "empty agent name"},
		{"parallel wait", func(d *Definition) {
			*agentState(d) = Handler{Kind: HandlerParallel, Agents: []string{"a"}, Consolidator: "c", Wait: "most"}
		}, where{"", "states[0].handler.wait", "review", "wait"}, "invalid wait"},
		{"set key", func(d *Definition) {
			*agentState(d) = Handler{Kind: HandlerVars, Set: map[string]string{"a.b": "1"}}
		}, where{"", `states[0].handler.set["a.b"]`, "review", `set["a.b"]`}, "set key"},
		{"capture path", func(d *Definition) { agentState(d).Capture = map[string]string{"draft": "nope"} },
			where{"", "states[0].handler.capture.draft", "review", "capture.draft"}, `capture "draft"`},
		{"misplaced source", func(d *Definition) { agentState(d).Source = &StarterSource{Channel: "c"} },
			where{"", "states[0].handler.source", "review", "source"}, "starter only"},
		{"timeout", func(d *Definition) { agentState(d).TimeoutMS = -1 },
			where{"", "states[0].handler.timeout_ms", "review", "timeout_ms"}, "timeout_ms"},
		{"prompt slot", func(d *Definition) { agentState(d).InputTemplate = StarterMessageSlot },
			where{"", "states[0].handler.input_template", "review", "input_template"}, "only a starter has a work item"},
		{"payload", func(d *Definition) { agentState(d).Payload = PayloadRaw },
			where{"", "states[0].handler.payload", "review", "payload"}, "channel only"},
		{"input publish", func(d *Definition) {
			*agentState(d) = Handler{Kind: HandlerInput, Publish: &InputPublish{}}
		}, where{"", "states[0].handler.publish.channel", "review", "publish.channel"}, "names no channel"},
		{"starter source kind", func(d *Definition) {
			h := okStarter()
			h.Source.Kind = "queue"
			*agentState(d) = h
		}, where{"", "states[0].handler.source.kind", "review", "source.kind"}, "invalid kind"},
		{"channel source wait", func(d *Definition) {
			h := okStarter()
			h.Source.Wait = WaitAll
			*agentState(d) = h
		}, where{"", "states[0].handler.source.wait", "review", "source.wait"}, "counts CHANNELS"},
		{"document source path", func(d *Definition) {
			h := okStarter()
			h.Source = &StarterSource{Kind: SourceDocument, Path: "specs"}
			h.Fanout.Per = FanoutPerChunk
			*agentState(d) = h
		}, where{"", "states[0].handler.source.path", "review", "source.path"}, "absolute document path"},
		{"input source batch", func(d *Definition) {
			h := okStarter()
			h.Source = &StarterSource{Kind: SourceInput, Batch: 2}
			*agentState(d) = h
		}, where{"", "states[0].handler.source.batch", "review", "source.batch"}, "read whole"},
		{"fanout member", func(d *Definition) {
			h := okStarter()
			h.Fanout = &StarterFanout{Agents: []string{"a", ""}, Max: 2}
			*agentState(d) = h
		}, where{"", "states[0].handler.fanout.agents[1]", "review", "fanout.agents[1]"}, "empty agent name"},
		{"fanout wait", func(d *Definition) {
			h := okStarter()
			h.Fanout.Wait = "at_least:0"
			*agentState(d) = h
		}, where{"", "states[0].handler.fanout.wait", "review", "fanout.wait"}, "positive integer"},
		{"sink", func(d *Definition) {
			h := okStarter()
			h.Sink = &StarterSink{}
			*agentState(d) = h
		}, where{"", "states[0].handler.sink.channel", "review", "sink.channel"}, "names no channel"},
		{"binds", func(d *Definition) {
			h := okStarter()
			h.Binds = map[string]string{"pr": "$["}
			*agentState(d) = h
		}, where{"", "states[0].handler.binds.pr", "review", "binds.pr"}, `capture "pr"`},
		{"entry resolves", func(d *Definition) { d.Entry = "ghost" },
			where{"", "entry", "", ""}, "does not resolve"},
		{"input starter placement", func(d *Definition) {
			d.States[1].Handler = Handler{Kind: HandlerStarter, Source: &StarterSource{Kind: SourceInput},
				Fanout: &StarterFanout{Agent: "w", Max: 2}}
			d.Transitions = append(d.Transitions, Transition{From: "done", To: "review", On: OnSuccess})
		}, where{"", "states[1].handler.source.kind", "done", "source.kind"}, "must be the definition's `entry`"},
		{"transition into the input starter", func(d *Definition) {
			*agentState(d) = Handler{Kind: HandlerStarter, Source: &StarterSource{Kind: SourceInput},
				Fanout: &StarterFanout{Agent: "w", Max: 2}}
			d.States[1].Handler = Handler{Kind: HandlerAgent, Agent: "x"}
			d.Transitions = append(d.Transitions, Transition{From: "done", To: "review", On: OnSuccess})
		}, where{"", "transitions[1].to", "", ""}, "cannot be re-entered"},
		{"local skill name", func(d *Definition) {
			d.Local = &Local{Skills: map[string]LocalSkill{"a/b": {Body: "b"}}}
		}, where{"", `local.skills["a/b"]`, "", ""}, "local.skills"},
		{"own channel in the ACL", func(d *Definition) {
			d.Channels = &TeamChannels{Subscribe: []string{"ok", "./mine"}}
		}, where{"", "channels.subscribe[1]", "", ""}, "remove it from the ACL"},
		{"local skill grant", func(d *Definition) {
			d.Local = &Local{Agents: map[string]json.RawMessage{"rev": json.RawMessage(`{"skills":["-x","./ghost"]}`)}}
		}, where{"", "local.agents.rev.skills[1]", "", ""}, "does not declare under local.skills"},
		{"local schedule cadence", func(d *Definition) {
			d.Local = &Local{Channels: map[string]json.RawMessage{"tick": json.RawMessage(`{}`)},
				Schedules: map[string]LocalSchedule{"t": {Schedule: "@every 1s", Channel: "./tick"}}}
		}, where{"", "local.schedules.t.schedule", "", ""}, "fires at most every"},
		{"local webhook channel", func(d *Definition) {
			d.Local = &Local{Webhooks: map[string]json.RawMessage{"in": json.RawMessage(`{"channel":"bare"}`)}}
		}, where{"", "local.webhooks.in.channel", "", ""}, "publishes only into the team"},
		{"undeclared local channel", func(d *Definition) {
			*agentState(d) = Handler{Kind: HandlerChannel, Channel: "./out"}
		}, where{IssueLocalChannelMissing, "states[0].handler.channel", "review", "channel"}, "local.channels"},
		{"transition from", func(d *Definition) {
			d.Transitions = append(d.Transitions, Transition{From: "ghost", To: "done", On: OnSuccess})
		}, where{"", "transitions[1].from", "", ""}, "does not resolve"},
		{"terminal outbound", func(d *Definition) {
			d.Transitions = append(d.Transitions, Transition{From: "done", To: "review", On: OnSuccess})
		}, where{"", "transitions[1]", "", ""}, "no outbound transitions"},
		{"duplicate label", func(d *Definition) {
			d.Transitions = append(d.Transitions, Transition{From: "review", To: "review", On: OnSuccess})
		}, where{"", "transitions[1].on", "", ""}, "duplicate outbound transition label"},
		{"unreachable", func(d *Definition) {
			d.States = append(d.States, State{ID: "island", Handler: Handler{Kind: HandlerTerminal}})
		}, where{"", "states[2]", "island", ""}, "unreachable"},
		{"dead end", func(d *Definition) { d.Transitions = nil; d.Entry = "review" },
			where{"", "states[0]", "review", ""}, "dead end"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := baseDef()
			tc.mutate(&d)
			all := ValidateAll(d)
			for _, is := range all {
				if strings.Contains(is.Msg, tc.msg) {
					if got := (where{is.Kind, is.Path, is.State, is.Field}); got != tc.want {
						t.Errorf("issue %q at %+v, want %+v", is.Msg, got, tc.want)
					}
					return
				}
			}
			t.Errorf("no issue containing %q; got %v", tc.msg, all)
		})
	}
}

// TestValidateAll_SuppressesCascades: a finding that makes a later check
// meaningless reports itself, not the echo.
func TestValidateAll_SuppressesCascades(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d *Definition)
		want   []string // every issue's path, in order
	}{
		{"empty entry: no 'does not resolve', no reachability",
			func(d *Definition) { d.Entry = "" }, []string{"entry"}},
		{"unresolved entry: no reachability",
			func(d *Definition) { d.Entry = "ghost" }, []string{"entry"}},
		{"no states: no 'does not resolve'",
			func(d *Definition) { d.States = nil; d.Transitions = nil }, []string{"states"}},
		{"unknown kind: only the kind, and no dead end",
			func(d *Definition) {
				d.States[0].Handler = Handler{Kind: "delay", Source: &StarterSource{}, Set: map[string]string{"a": "b"}, TimeoutMS: -1}
				d.Transitions = nil
			}, []string{"states[0].handler.kind", "states[1]"}},
		{"starter without fanout: sink, ack and binds still checked",
			func(d *Definition) {
				d.States[0].Handler = Handler{Kind: HandlerStarter, Source: &StarterSource{Channel: "c"},
					Sink: &StarterSink{}, Ack: "later", Binds: map[string]string{"x": "nope"}}
			}, []string{"states[0].handler.fanout", "states[0].handler.sink.channel", "states[0].handler.ack", "states[0].handler.binds.x"}},
		{"channel state's source: refused once",
			func(d *Definition) {
				d.States[0].Handler = Handler{Kind: HandlerChannel, Channel: "c", Source: &StarterSource{Channel: "c"}}
			}, []string{"states[0].handler.source"}},
		{"document source per=message: no max echo",
			func(d *Definition) {
				d.States[0].Handler = Handler{Kind: HandlerStarter, Source: &StarterSource{Kind: SourceDocument, Path: "/d"},
					Fanout: &StarterFanout{Agent: "w"}}
			}, []string{"states[0].handler.fanout.per"}},
		{"input source per=chunk: refused once",
			func(d *Definition) {
				d.States[0].Handler = Handler{Kind: HandlerStarter, Source: &StarterSource{Kind: SourceInput},
					Fanout: &StarterFanout{Agent: "w", Per: FanoutPerChunk}}
			}, []string{"states[0].handler.fanout.per"}},
		{"document source ack: refused once, not also as an invalid value",
			func(d *Definition) {
				d.States[0].Handler = Handler{Kind: HandlerStarter, Source: &StarterSource{Kind: SourceDocument, Path: "/d"},
					Fanout: &StarterFanout{Agent: "w", Per: FanoutPerOnce}, Ack: "bogus"}
			}, []string{"states[0].handler.ack"}},
		{"payload on the wrong kind: not also an invalid value",
			func(d *Definition) { d.States[0].Handler.Payload = "weird" }, []string{"states[0].handler.payload"}},
		{"duplicate id: the first keeps it, the second is judged on its id alone",
			func(d *Definition) {
				d.States = append(d.States, State{ID: "review", Handler: Handler{Kind: HandlerAgent, Agent: "x"}})
			},
			[]string{"states[2].state"}},
		{"dangling transition: endpoints and on reported; no dead end for its source",
			func(d *Definition) { d.Transitions = []Transition{{From: "review", To: "ghost", On: "maybe"}} },
			[]string{"transitions[0].to", "transitions[0].on", "states[1]"}},
		{"transition out of nowhere: on still checked",
			func(d *Definition) {
				d.Transitions = append(d.Transitions, Transition{From: "ghost", To: "done", On: "pushback:"})
			}, []string{"transitions[1].from", "transitions[1].on"}},
		{"bad walk hook event: its entries are not re-judged",
			func(d *Definition) { d.Hooks = hooks.EventHooks{"bogus": {{Ref: ""}}} }, []string{"hooks.bogus"}},
		{"input starter off the entry while the entry is unresolved: entry only",
			func(d *Definition) {
				d.Entry = "ghost"
				d.States[0].Handler = Handler{Kind: HandlerStarter, Source: &StarterSource{Kind: SourceInput},
					Fanout: &StarterFanout{Agent: "w", Max: 1}}
			}, []string{"entry"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := baseDef()
			tc.mutate(&d)
			all := ValidateAll(d)
			var got []string
			for _, is := range all {
				got = append(got, is.Path)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("paths = %q, want %q\nissues: %v", got, tc.want, all)
			}
		})
	}
}

// malformedDefs is a set of definitions that are wrong in every way a nil or
// empty field can be — the shapes a hand-edited JSON body produces.
func malformedDefs() map[string]Definition {
	every := []string{HandlerAgent, HandlerParallel, HandlerConsolidator, HandlerTerminal, HandlerVars,
		HandlerInput, HandlerStarter, HandlerChannel, "", "delay"}
	out := map[string]Definition{
		"zero":                {},
		"empty state":         {States: []State{{}}},
		"dangling only":       {Entry: "x", Transitions: []Transition{{}, {From: "x", To: "y"}}},
		"empty local":         {Entry: "a", States: []State{{ID: "a"}}, Local: &Local{}},
		"empty channels":      {Entry: "a", States: []State{{ID: "a"}}, Channels: &TeamChannels{}},
		"nil hook lists":      {Entry: "a", States: []State{{ID: "a"}}, Hooks: hooks.EventHooks{hooks.PhaseRunEnd: nil, "x": nil}},
		"nil tool hook event": {Entry: "a", States: []State{{ID: "a", Handler: Handler{Kind: HandlerAgent, ToolHooks: hooks.ToolHooks{"": nil, "Bash": nil}}}}},
		"local with empty bodies": {Entry: "a", States: []State{{ID: "a", Handler: Handler{Kind: HandlerChannel, Channel: "./c"}}},
			Local: &Local{Agents: map[string]json.RawMessage{"": nil}, Channels: map[string]json.RawMessage{"": nil},
				Schedules: map[string]LocalSchedule{"": {}}, Webhooks: map[string]json.RawMessage{"": nil}, Skills: map[string]LocalSkill{"": {}}}},
	}
	for _, k := range every {
		out["bare "+k] = Definition{Entry: "s", States: []State{{ID: "s", Handler: Handler{Kind: k}}}}
		out["empty blocks "+k] = Definition{Entry: "s", States: []State{{ID: "s", Handler: Handler{Kind: k,
			Source: &StarterSource{}, Fanout: &StarterFanout{}, Sink: &StarterSink{}, Prompt: &StarterPrompt{}, Publish: &InputPublish{},
			Agents: []string{""}, Set: map[string]string{"": ""}, Capture: map[string]string{"": ""}, Binds: map[string]string{"": ""},
			Hooks: hooks.EventHooks{"": {{}}}, ToolHooks: hooks.ToolHooks{"": {"": {{}}}}}}}}
		for _, src := range []string{SourceDocument, SourceInput, "weird"} {
			out["nil fanout "+k+" "+src] = Definition{Entry: "s", States: []State{{ID: "s", Handler: Handler{Kind: k, Source: &StarterSource{Kind: src}}}}}
		}
	}
	return out
}

// TestValidateAll_NeverPanicsOnMalformedDefinitions: every entry point that
// reads a definition survives one with nil blocks, empty ids and dangling
// transitions — and the error forms agree with the collecting forms.
func TestValidateAll_NeverPanicsOnMalformedDefinitions(t *testing.T) {
	for name, d := range malformedDefs() {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			all := ValidateAll(d)
			_ = ChannelRefs(d)
			_ = AgentRefs(d)
			_ = CheckLocalRunNamesAll(d, "team")
			for _, f := range []func(Definition) error{CheckLocalRefs, CheckLocalChannelRefs, CheckLocalSchedules, CheckLocalWebhooks, ValidateHooks} {
				_ = f(d)
			}
			// The only shapes here that are actually valid: a lone terminal
			// state, with or without empty optional blocks.
			validShape := map[string]bool{"empty local": true, "empty channels": true, "bare terminal": true}
			if !validShape[name] && len(all) == 0 {
				t.Errorf("a malformed definition validated clean")
			}
			for _, is := range all {
				if is.Path == "" || is.Msg == "" {
					t.Errorf("issue without a path or message: %+v", is)
				}
			}
		})
	}
}

// TestValidate_IsTheFirstIssueOfValidateAll: the error-returning form is the
// collecting form's first finding, as an *Issue, and a valid definition is a
// plain nil error (not a nil *Issue inside an interface).
func TestValidate_IsTheFirstIssueOfValidateAll(t *testing.T) {
	defs := malformedDefs()
	defs["valid"] = baseDef()
	defs["valid starter"] = starterDef(okStarter())
	for name, d := range defs {
		err := Validate(d)
		all := ValidateAll(d)
		if len(all) == 0 {
			if err != nil {
				t.Errorf("%s: Validate = %v with no issues", name, err)
			}
			continue
		}
		var is *Issue
		if !errors.As(err, &is) {
			t.Fatalf("%s: Validate returned %T, want *Issue", name, err)
		}
		if *is != *all[0] {
			t.Errorf("%s: Validate = %+v, want first issue %+v", name, *is, *all[0])
		}
	}
	if err := Validate(baseDef()); err != nil {
		t.Fatalf("baseDef must validate: %v", err)
	}
}

// TestCheckLocalRefs_IsTheFirstOfEveryUndeclaredReference: the collecting
// sweep reports every undeclared "./name" with the verify sweep's kinds, and
// CheckLocalRefs still returns only the first.
func TestCheckLocalRefs_IsTheFirstOfEveryUndeclaredReference(t *testing.T) {
	d := Definition{Entry: "a", States: []State{
		{ID: "a", Handler: Handler{Kind: HandlerParallel, Agents: []string{"./x", "g", "./y"}, Consolidator: "c"}},
		{ID: "b", Handler: Handler{Kind: HandlerChannel, Channel: "./out"}},
	}}
	got := wheres(localRefIssues(d))
	want := []where{
		{IssueLocalAgentMissing, "states[0].handler.agents[0]", "a", "agents[0]"},
		{IssueLocalAgentMissing, "states[0].handler.agents[2]", "a", "agents[2]"},
		{IssueLocalChannelMissing, "states[1].handler.channel", "b", "channel"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("localRefIssues =\n  %+v\nwant\n  %+v", got, want)
	}
	err := CheckLocalRefs(d)
	if err == nil || !strings.Contains(err.Error(), `state "a" agents: "./x" names a local agent`) {
		t.Errorf("CheckLocalRefs = %v, want the first reference", err)
	}
}

// TestCheckLocalRunNamesAll_ReportsEveryBareRunName: each state naming the
// team's own agent by its bare run name is reported at its own path.
func TestCheckLocalRunNamesAll_ReportsEveryBareRunName(t *testing.T) {
	d := Definition{
		States: []State{
			{ID: "a", Handler: Handler{Kind: HandlerAgent, Agent: "sdlc/rev", Consolidator: "sdlc/rev"}},
			{ID: "b", Handler: Handler{Kind: HandlerAgent, Agent: "./rev"}},
		},
		Local: &Local{Agents: map[string]json.RawMessage{"rev": json.RawMessage(`{}`)}},
	}
	got := wheres(CheckLocalRunNamesAll(d, "sdlc"))
	want := []where{
		{"", "states[0].handler.agent", "a", "agent"},
		{"", "states[0].handler.consolidator", "a", "consolidator"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CheckLocalRunNamesAll =\n  %+v\nwant\n  %+v", got, want)
	}
	if err := CheckLocalRunNames(d, "sdlc"); err == nil || err.Error() != CheckLocalRunNamesAll(d, "sdlc")[0].Msg {
		t.Errorf("CheckLocalRunNames = %v, want the first issue", err)
	}
	if got := CheckLocalRunNamesAll(d, "other"); got != nil {
		t.Errorf("another team's name is not this team's run name: %v", got)
	}
}

func TestPathKey_QuotesKeysThatAreNotIdentifiers(t *testing.T) {
	for _, tc := range []struct{ base, key, want string }{
		{"vars", "tone", "vars.tone"},
		{"local.agents", "code-rev_2", "local.agents.code-rev_2"},
		{"vars", "has space", `vars["has space"]`},
		{"vars", "a.b", `vars["a.b"]`},
		{"vars", "", `vars[""]`},
		{"vars", `q"<x>`, `vars["q\"<x>"]`},
		{"vars", "tab\there", `vars["tab\there"]`},
		{"vars", "\x00", `vars["\u0000"]`},
		{"", "top", "top"},
	} {
		if got := PathKey(tc.base, tc.key); got != tc.want {
			t.Errorf("PathKey(%q, %q) = %s, want %s", tc.base, tc.key, got, tc.want)
		}
	}
}

// TestValidateAll_IsDeterministic: map-driven checks are reported in sorted
// key order, so two runs over the same definition agree issue for issue.
func TestValidateAll_IsDeterministic(t *testing.T) {
	d := baseDef()
	d.Vars = map[string]string{}
	for i := 0; i < 20; i++ {
		d.Vars[fmt.Sprintf("bad key %02d", i)] = "x"
	}
	d.Hooks = hooks.EventHooks{"zeta": nil, "alpha": nil, hooks.PhasePre: nil}
	first := wheres(ValidateAll(d))
	for i := 0; i < 20; i++ {
		if got := wheres(ValidateAll(d)); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n  %v\nvs\n  %v", i, got, first)
		}
	}
	if first[0].Path != "hooks.alpha" {
		t.Errorf("first hook event = %s, want the sorted-first hooks.alpha", first[0].Path)
	}
}
