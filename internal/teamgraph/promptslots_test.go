package teamgraph

import (
	"strings"
	"testing"
)

// {{thread.output}} is where a template says the previous state's output goes.
// Every kind that takes an input_template accepts it there — the entry state
// included, where it is the walk's input.
func TestValidate_ThreadedOutputSlotAcceptedInInputTemplate(t *testing.T) {
	const tmpl = "Tone: ${var.tone}\n\n" + ThreadedOutputSlot
	cases := map[string]Handler{
		"agent":        {Kind: HandlerAgent, Agent: "writer", InputTemplate: tmpl},
		"parallel":     {Kind: HandlerParallel, Agents: []string{"a", "b"}, Consolidator: "judge", InputTemplate: tmpl},
		"consolidator": {Kind: HandlerConsolidator, Agent: "judge", InputTemplate: tmpl},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Validate(oneStateDef(h)); err != nil {
				t.Fatalf("%s input_template naming %s must validate: %v", name, ThreadedOutputSlot, err)
			}
		})
	}
}

// Each refusal names the state and the field, so the author knows which text
// to change.
func TestValidate_ReservedSlotInTheWrongPromptIsRefused(t *testing.T) {
	starter := func(p StarterPrompt) Handler {
		h := okStarter()
		h.Prompt = &p
		return h
	}
	cases := []struct {
		name string
		h    Handler
		want []string
	}{
		// The hand-off is another agent's output: never in the system segment.
		{"agent system_prompt", Handler{Kind: HandlerAgent, Agent: "w", SystemPrompt: "Follow: " + ThreadedOutputSlot},
			[]string{`state "s"`, "`system_prompt`", ThreadedOutputSlot}},
		{"parallel system_prompt", Handler{Kind: HandlerParallel, Agents: []string{"a"}, Consolidator: "j", SystemPrompt: ThreadedOutputSlot},
			[]string{`state "s"`, "`system_prompt`"}},
		{"consolidator system_prompt", Handler{Kind: HandlerConsolidator, Agent: "j", SystemPrompt: ThreadedOutputSlot},
			[]string{`state "s"`, "`system_prompt`"}},
		{"system_prompt even with the slot in input_template too", Handler{Kind: HandlerAgent, Agent: "w",
			SystemPrompt: ThreadedOutputSlot, InputTemplate: ThreadedOutputSlot},
			[]string{"`system_prompt`"}},
		// A starter threads nothing to its runs.
		{"starter prompt.input", starter(StarterPrompt{Input: "Review " + ThreadedOutputSlot}),
			[]string{`state "s"`, "`prompt.input`", StarterMessageSlot}},
		{"starter prompt.system", starter(StarterPrompt{System: ThreadedOutputSlot}),
			[]string{`state "s"`, "`prompt.system`"}},
		// Only a starter has a work item.
		{"starter.message in an agent input_template", Handler{Kind: HandlerAgent, Agent: "w", InputTemplate: "Do " + StarterMessageSlot},
			[]string{`state "s"`, "`input_template`", StarterMessageSlot, ThreadedOutputSlot}},
		{"starter.messages in an agent system_prompt", Handler{Kind: HandlerAgent, Agent: "w", SystemPrompt: StarterMessagesSlot},
			[]string{`state "s"`, "`system_prompt`", StarterMessagesSlot}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(oneStateDef(c.h))
			if err == nil {
				t.Fatalf("validated, want a refusal naming %v", c.want)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

// A starter's own slots stay valid in its own prompt.
func TestValidate_StarterSlotsAcceptedInAStarterPrompt(t *testing.T) {
	h := okStarter()
	h.Prompt = &StarterPrompt{System: "Batch: " + StarterMessagesSlot, Input: "Review " + StarterMessageSlot}
	if err := Validate(oneStateDef(h)); err != nil {
		t.Fatalf("a starter's own slots must validate: %v", err)
	}
}
