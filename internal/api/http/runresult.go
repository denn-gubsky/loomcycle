package http

import (
	"encoding/json"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// runResultRecord is what runs.result holds (RFC DI): the part of a run's
// answer the run row had no column for. Stop reason, error, usage and cost
// already have columns and already travel on every run read, so they are not
// repeated here — a reader assembles the whole answer from the row.
//
// The shape is additive: fields are only ever added.
type runResultRecord struct {
	FinalText string         `json:"final_text,omitempty"`
	State     map[string]any `json:"state,omitempty"`
	// Structured is the answer parsed against the run's output_format; absent
	// when the run had none or the answer was not a JSON object.
	Structured map[string]any `json:"structured,omitempty"`
}

// runResultJSON renders a loop result for FinishRun. nil when the run produced
// nothing to report — no text and no state — so the column stays NULL rather
// than holding an empty object a reader would have to special-case.
//
// Masked with r, like the events it summarises: the final text and the
// context_state events that carry Σ are redacted before they are persisted, so
// storing the same text and Σ here unmasked would put the secret back at rest
// on the run row. The live caller already got the original.
func runResultJSON(r *redact.Redactor, res loop.RunResult) json.RawMessage {
	if res.FinalText == "" && len(res.State) == 0 && len(res.Structured) == 0 {
		return nil
	}
	b, err := json.Marshal(runResultRecord{FinalText: res.FinalText, State: res.State, Structured: res.Structured})
	if err != nil {
		// State and Structured are model-produced JSON that already
		// round-tripped once; a marshal failure here means a value the encoder
		// cannot represent. Keep the text rather than lose the whole answer.
		log.Printf("run result: state/structured not encodable (%v); persisting final text only", err)
		b, err = json.Marshal(runResultRecord{FinalText: res.FinalText})
		if err != nil {
			return nil
		}
	}
	return r.Bytes(b)
}
