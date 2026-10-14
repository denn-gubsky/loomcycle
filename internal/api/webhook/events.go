package webhook

import (
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// Verdicts of the event check. ping and ignored are answers, not refusals:
// the request authenticated and nothing was wrong with it.
const (
	verdictPing          = "ping"
	verdictIgnored       = "ignored"
	verdictRejectedEvent = "rejected_event"
	verdictRejectedShape = "rejected_shape"
)

// eventOutcome is what the event check decided for a delivery.
type eventOutcome struct {
	// proceed: the delivery goes on to be handled. When false, status and
	// verdict say how it is answered and body is the JSON to answer with
	// (nil for an error code).
	proceed bool
	status  int
	verdict string
	code    string // the error code of a refusal
	event   string
}

// classifyEvent applies a definition's events to a delivery that has passed
// the signature check — never before: an unauthenticated request learns
// nothing, not even that a ping would be answered. A definition with no
// events proceeds, as every delivery did before events existed.
//
// The event header is not signed, so it counts only when the signed body has
// the shape of the event it names: a push body sent again as pull_request is
// refused. wd is the definition with its provider's defaults applied.
func classifyEvent(wd config.Webhook, header http.Header, body []byte) eventOutcome {
	if len(wd.Events) == 0 {
		return eventOutcome{proceed: true}
	}
	values := header.Values(wd.EventHeader)
	if len(values) != 1 || !config.ValidWebhookEvent(values[0]) {
		return eventOutcome{status: http.StatusBadRequest, verdict: verdictRejectedEvent, code: "bad_event"}
	}
	event := wd.CanonicalEvent(values[0])
	switch {
	case wd.IsPingEvent(event):
		return eventOutcome{status: http.StatusOK, verdict: verdictPing, event: event}
	case !wd.ListsEvent(event):
		return eventOutcome{status: http.StatusAccepted, verdict: verdictIgnored, event: event}
	}
	doc, err := decodeJSONBody(body)
	if err != nil {
		return eventOutcome{status: http.StatusBadRequest, verdict: "rejected_mapping", code: "bad_mapping", event: event}
	}
	shape, _ := wd.EventShape(event) // a listed event has one: checked when written
	if len(shape) == 0 {
		// A definition that reached the store some other way: count
		// nothing on the unsigned header alone.
		return eventOutcome{status: http.StatusBadRequest, verdict: verdictRejectedShape, code: "event_shape_mismatch", event: event}
	}
	for _, path := range shape {
		segs, err := jsonpath.Parse(path)
		if err != nil {
			return eventOutcome{status: http.StatusBadRequest, verdict: verdictRejectedShape, code: "event_shape_mismatch", event: event}
		}
		if v, ok := jsonpath.Eval(doc, segs); !ok || v == nil {
			return eventOutcome{status: http.StatusBadRequest, verdict: verdictRejectedShape, code: "event_shape_mismatch", event: event}
		}
	}
	return eventOutcome{proceed: true, event: event}
}

// answerEvent writes the response for an outcome that does not proceed and
// records it against the webhook. did is the delivery's id, for the answer.
func (rec *Receiver) answerEvent(w http.ResponseWriter, span trace.Span, whKey, name, did string, o eventOutcome) {
	rec.finish(span, whKey, did, o.verdict, "")
	if o.code != "" {
		rec.logf("webhook %q: delivery refused: %s (event %q)", name, o.code, o.event)
		writeError(w, o.status, o.code, "")
		return
	}
	writeJSON(w, o.status, map[string]string{
		"webhook_name": name, "delivery_id": did, "event": o.event, "outcome": o.verdict,
	})
}
