package http

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// sse wraps an http.ResponseWriter for server-sent-events output. One sse per
// connection. send is safe to call from the loop's goroutine — net/http
// serializes writes to the response writer for one handler invocation.
type sse struct {
	w       http.ResponseWriter
	flusher http.Flusher
	closed  bool
}

// newSSE returns an sse and a boolean indicating whether the writer supports
// streaming. When false, the caller should NOT call start() and should fall
// back to a JSON response — the writer would otherwise buffer every frame
// until handler return, defeating the point of SSE.
func newSSE(w http.ResponseWriter) (*sse, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return &sse{w: w}, false
	}
	return &sse{w: w, flusher: flusher}, true
}

func (s *sse) start() {
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	s.w.WriteHeader(http.StatusOK)
	s.flush()
}

func (s *sse) send(ev providers.Event) {
	if s.closed {
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		// Marshal can fail for unencodable values in ev.Payload-style fields.
		// Build the fallback frame as JSON too so a newline in err.Error()
		// can't escape the SSE data: line.
		fallback, mErr := json.Marshal(map[string]string{
			"type":  "error",
			"error": "marshal: " + err.Error(),
		})
		if mErr != nil {
			log.Printf("sse: fallback marshal failed: %v (orig: %v)", mErr, err)
			return
		}
		fmt.Fprintf(s.w, "event: error\ndata: %s\n\n", fallback)
		s.flush()
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev.Type, payload)
	s.flush()
}

// sendRaw emits an SSE frame with a custom event name and a JSON-
// marshalled payload. Used for side-channel events that don't fit the
// `providers.Event` shape — currently the v0.4 `event: agent` frame
// that announces the run's agent_id alongside the existing
// `event: session` frame.
//
// data may be any json-marshalable value; on marshal failure we log
// and silently drop (the run is still happening; an SSE-side hiccup
// shouldn't tear down the response). The caller is responsible for
// keeping the data shape stable since adapters parse it.
func (s *sse) sendRaw(eventName string, data any) {
	if s.closed {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		log.Printf("sse: sendRaw marshal failed for %q: %v", eventName, err)
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventName, payload)
	s.flush()
}

func (s *sse) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
