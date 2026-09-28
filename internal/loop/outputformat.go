package loop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// outputFormatPolicy applies a run's output_format across its model calls
// (RFC DI). Unlike tool_choice it never expires: every call carries the schema,
// because the model cannot know which turn will be its last.
type outputFormatPolicy struct {
	// src is the option this policy was built from (see adopt).
	src  *config.OutputFormat
	of   *config.OutputFormat
	wire *providers.OutputFormat
	// reported remembers the (provider, model, tools?) target it last checked,
	// so an unenforceable format is reported once per target.
	reported string
}

func newOutputFormatPolicy(of *config.OutputFormat) *outputFormatPolicy {
	if of.IsZero() {
		return &outputFormatPolicy{src: of}
	}
	// Validate ran at intake, so the schema is a JSON object and marshals.
	schema, _ := json.Marshal(of.Schema)
	return &outputFormatPolicy{src: of, of: of, wire: &providers.OutputFormat{Name: of.EffectiveName(), Schema: schema}}
}

// adopt returns the policy for of: p itself while of is the option p was built
// from, else a fresh one (a retune replaced the schema).
func (p *outputFormatPolicy) adopt(of *config.OutputFormat) *outputFormatPolicy {
	if of == p.src {
		return p
	}
	return newOutputFormatPolicy(of)
}

// forCall is the format to send on the next request, or nil. The loop, not
// each driver, decides: a target that cannot enforce the schema is sent none
// (a driver given a field it does not map would drop it silently, or 400), and
// the run says so once through emit.
func (p *outputFormatPolicy) forCall(prov providers.Provider, model string, hasTools bool, emit func(providers.Event)) *providers.OutputFormat {
	if p.wire == nil || prov == nil {
		return nil
	}
	if providers.EnforcesStructuredOutput(prov, model, hasTools) {
		return p.wire
	}
	key := fmt.Sprintf("%s|%s|%t", prov.ID(), model, hasTools)
	if key != p.reported {
		p.reported = key
		msg := fmt.Sprintf("output_format %q is not enforced by model %q on provider %q: the answer is not "+
			"held to the schema — it is given in the system prompt instead — and result.structured is set only if the answer parses", p.of.EffectiveName(), model, prov.ID())
		emit(providers.Event{
			Type:            providers.EventCapabilityInert,
			Text:            msg,
			CapabilityInert: &providers.CapabilityInertInfo{Gate: "output_format", Message: msg},
		})
	}
	return nil
}

// promptNote is the system block that shows the model the schema, or false
// when the target already does (it enforces the format AND is a native
// structured-output API). A grammar-only backend constrains the tokens but
// never shows the schema, so the model fills well-formed fields with little
// idea of what they mean; a target that cannot enforce it at all has only the
// prompt to go on. Code-js replays a script and reads no prompt.
func (p *outputFormatPolicy) promptNote(prov providers.Provider, model string, hasTools bool) (providers.ContentBlock, bool) {
	if p.wire == nil || prov == nil || prov.ID() == codeJSProviderID {
		return providers.ContentBlock{}, false
	}
	if prov.Capabilities().StructuredOutputNative && providers.EnforcesStructuredOutput(prov, model, hasTools) {
		return providers.ContentBlock{}, false
	}
	return providers.ContentBlock{Type: "text", Text: "Your final answer must be a single JSON object that follows this " +
		"JSON Schema, with no text before or after it:\n" + string(p.wire.Schema)}, true
}

// report is what Context op=self shows for this policy on the given target, or
// nil when the run has no output_format. Enforcement is derived from the same
// two answers forCall and promptNote give, so it cannot disagree with what the
// request actually carries.
func (p *outputFormatPolicy) report(prov providers.Provider, model string, hasTools bool) *tools.OutputFormatReport {
	if p.wire == nil {
		return nil
	}
	r := &tools.OutputFormatReport{Type: p.of.EffectiveType(), Name: p.of.EffectiveName(), Schema: p.of.Schema}
	enforced := prov != nil && providers.EnforcesStructuredOutput(prov, model, hasTools)
	_, inPrompt := p.promptNote(prov, model, hasTools)
	switch {
	case enforced && !inPrompt:
		r.Enforcement = tools.EnforcementNative
	case enforced:
		r.Enforcement = tools.EnforcementGrammar
	case inPrompt:
		r.Enforcement = tools.EnforcementPrompt
	default:
		r.Enforcement = tools.EnforcementNone
	}
	return r
}

// structured parses the run's final answer for RunResult.Structured. A JSON
// object is kept whether or not a provider enforced it; anything else is nil,
// reported, and the text stays in FinalText untouched. The schema is not
// re-validated here: where it was enforced the provider already did, and where
// it was not, the report above already said the answer is unchecked.
func (p *outputFormatPolicy) structured(finalText string, emit func(providers.Event)) map[string]any {
	if p.wire == nil || strings.TrimSpace(finalText) == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stripJSONFence(finalText)), &out); err != nil || out == nil {
		msg := fmt.Sprintf("output_format %q: the final answer is not a JSON object, so result.structured is empty", p.of.EffectiveName())
		emit(providers.Event{
			Type:            providers.EventCapabilityInert,
			Text:            msg,
			CapabilityInert: &providers.CapabilityInertInfo{Gate: "output_format", Message: msg},
		})
		return nil
	}
	return out
}

// stripJSONFence removes one surrounding ```json … ``` fence — what a model
// that was asked for JSON but not held to it most often wraps its answer in.
func stripJSONFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") || !strings.HasSuffix(t, "```") || len(t) < 6 {
		return t
	}
	t = strings.TrimSuffix(strings.TrimPrefix(t, "```"), "```")
	if nl := strings.IndexByte(t, '\n'); nl >= 0 && !strings.ContainsAny(t[:nl], "{[") {
		t = t[nl+1:] // the info string ("json")
	}
	return strings.TrimSpace(t)
}
