package loop

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// choiceProvider has tool_choice on the wire (or not) and may still decline to
// enforce it per model, the way a driver does for a model in thinking mode.
type choiceProvider struct {
	onWire, enforces bool
}

func (p *choiceProvider) ID() string                                   { return "choice" }
func (p *choiceProvider) Probe(context.Context) error                  { return nil }
func (p *choiceProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *choiceProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{SupportsToolChoice: p.onWire}
}
func (p *choiceProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	return nil, nil
}
func (p *choiceProvider) EnforcesToolChoice(string, string, providers.ToolChoice) bool {
	return p.enforces
}

// A provider WITH tool_choice on the wire still drops a forced one for a
// thinking model (DeepSeek in thinking mode, Anthropic under budgeted thinking).
// The note asked only the provider's coarse bit, so a failed stateful step on
// such a model claimed nothing, as if the state tool had been enforced.
func TestUnforcedNote_SaysSoWhenTheModelDoesNotEnforceTheChoice(t *testing.T) {
	opts := RunOptions{Provider: &choiceProvider{onWire: true, enforces: false}, Model: "deepseek-v4-flash"}
	got := unforcedNote(opts)
	if !strings.Contains(got, "NOT enforced") || !strings.Contains(got, `"deepseek-v4-flash"`) {
		t.Errorf("note = %q, want it to say the call was not enforced for the model", got)
	}
}

func TestUnforcedNote_EnforcedChoiceAndNoWireChoice(t *testing.T) {
	if got := unforcedNote(RunOptions{Provider: &choiceProvider{onWire: true, enforces: true}, Model: "m"}); got != "" {
		t.Errorf("an enforced choice carries no note, got %q", got)
	}
	got := unforcedNote(RunOptions{Provider: &choiceProvider{onWire: false, enforces: false}, Model: "m"})
	if !strings.Contains(got, "no tool_choice on the wire") {
		t.Errorf("a provider without tool_choice keeps its own note, got %q", got)
	}
}
