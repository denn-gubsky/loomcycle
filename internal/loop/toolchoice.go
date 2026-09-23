package loop

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// toolChoicePolicy applies a run's tool_choice across its model calls (RFC DI).
//
// A forced choice is a property of a CALL, not of a run, so the policy decides
// per call: first_call applies until the first model turn completes,
// until_called until a turn makes a call that satisfies it, always on every
// call. It is consumed by a COMPLETED turn, never by a request that failed and
// is being retried — a first_call whose call errored still applies to the retry.
type toolChoicePolicy struct {
	tc   *config.ToolChoice
	done bool
	// reported remembers the (provider, model, effort) it last checked, so an
	// unenforceable choice is reported once per target — at the start and again
	// only if a fallback moves the run somewhere the answer may differ.
	reported string
}

func newToolChoicePolicy(tc *config.ToolChoice) *toolChoicePolicy {
	if tc.IsZero() {
		return &toolChoicePolicy{done: true}
	}
	return &toolChoicePolicy{tc: tc}
}

func (p *toolChoicePolicy) choice() providers.ToolChoice {
	return providers.ToolChoice{Mode: p.tc.Mode, Name: p.tc.Name}
}

// forCall is the choice to send on the next request: the configured one while
// it still applies, the zero value (auto) after.
func (p *toolChoicePolicy) forCall() providers.ToolChoice {
	if p.done {
		return providers.ToolChoice{}
	}
	return p.choice()
}

// observe advances the policy after a model turn COMPLETED, given the tool
// calls that turn made.
func (p *toolChoicePolicy) observe(calls []providers.ToolUse) {
	if p.done {
		return
	}
	switch p.tc.EffectiveUntil() {
	case config.ToolChoiceUntilFirstCall:
		p.done = true
	case config.ToolChoiceUntilUntilCalled:
		for _, c := range calls {
			if p.tc.Mode == config.ToolChoiceModeRequired || c.Name == p.tc.Name {
				p.done = true
				return
			}
		}
	}
}

// checkTool refuses a named choice for a tool the run does not have: the model
// could not comply, and a driver would either 400 or silently ignore it.
func (p *toolChoicePolicy) checkTool(specs []providers.ToolSpec) error {
	if p.tc == nil || p.tc.Mode != config.ToolChoiceModeTool {
		return nil
	}
	for _, s := range specs {
		if s.Name == p.tc.Name {
			return nil
		}
	}
	return fmt.Errorf("tool_choice names tool %q, which this run does not have — add it to the agent's tools or change the choice", p.tc.Name)
}

// reportIfUnenforced emits a capability_inert event when the resolved target
// will not hold the call to this choice. The run continues: forcing is an
// optimisation of what the prompt asks, and refusing would strand every run on
// a provider without the parameter. What must not happen is the choice
// vanishing without a word.
func (p *toolChoicePolicy) reportIfUnenforced(prov providers.Provider, model, effort string, emit func(providers.Event)) {
	if p.done || prov == nil {
		return
	}
	key := prov.ID() + "|" + model + "|" + effort
	if key == p.reported {
		return
	}
	p.reported = key
	if providers.EnforcesToolChoice(prov, model, effort, p.choice()) {
		return
	}
	what := "mode " + p.tc.Mode
	if p.tc.Name != "" {
		what = "tool " + p.tc.Name
	}
	msg := fmt.Sprintf("tool_choice (%s) is not enforced by model %q on provider %q: the model is asked "+
		"through the prompt instead, and may not comply", what, model, prov.ID())
	emit(providers.Event{
		Type: providers.EventCapabilityInert,
		Text: msg,
		CapabilityInert: &providers.CapabilityInertInfo{
			Tool: p.tc.Name, Gate: "tool_choice", Message: msg,
		},
	})
}
