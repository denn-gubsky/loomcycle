package http

import (
	"encoding/json"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/loop"
)

// runResultRecord is what runs.result holds (RFC DI): the part of a run's
// answer the run row had no column for. Stop reason, error, usage and cost
// already have columns and already travel on every run read, so they are not
// repeated here — a reader assembles the whole answer from the row.
//
// The shape is additive: DI-P2 adds `structured` (the output_format result).
type runResultRecord struct {
	FinalText string         `json:"final_text,omitempty"`
	State     map[string]any `json:"state,omitempty"`
}

// runResultJSON renders a loop result for FinishRun. nil when the run produced
// nothing to report — no text and no state — so the column stays NULL rather
// than holding an empty object a reader would have to special-case.
func runResultJSON(res loop.RunResult) json.RawMessage {
	if res.FinalText == "" && len(res.State) == 0 {
		return nil
	}
	b, err := json.Marshal(runResultRecord{FinalText: res.FinalText, State: res.State})
	if err != nil {
		// State is model-produced JSON that already round-tripped once; a
		// marshal failure here means a value the encoder cannot represent.
		// Keep the text rather than lose the whole answer.
		log.Printf("run result: state not encodable (%v); persisting final text only", err)
		b, err = json.Marshal(runResultRecord{FinalText: res.FinalText})
		if err != nil {
			return nil
		}
	}
	return b
}
