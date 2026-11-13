// helpinject.go — the SERVER side of the {{help:<topic>}} placeholder (RFC GL
// P4): renders each named topic's call examples from the help corpus embedded
// in the binary. The grammar, the budget and the trust argument live in
// internal/promptinject/helpinject.go.
package http

import (
	"log/slog"

	"github.com/denn-gubsky/loomcycle/internal/help/builtin"
)

// renderHelpExamples renders the call examples of each topic, keyed by topic.
// A pure read of embedded bytes: no store, no tool, no network, so it is the
// same at every run start, spawn and resume.
//
// Every authoring surface refuses a topic that does not render, so
// a miss here means a stored definition outlived its topic across a binary
// upgrade. The placeholder then renders nothing, as a {{tool:…}} ref with no
// body does — a run must not fail on prompt assembly — and the miss is logged
// so the operator can fix the definition.
func renderHelpExamples(agent string, topics []string) map[string]string {
	if len(topics) == 0 {
		return nil
	}
	out := make(map[string]string, len(topics))
	for _, topic := range topics {
		body, err := builtin.RenderExamples(topic)
		if err != nil {
			slog.Warn("prompt: {{help:...}} placeholder renders nothing", "agent", agent, "topic", topic, "err", err)
			continue
		}
		out[topic] = body
	}
	return out
}
