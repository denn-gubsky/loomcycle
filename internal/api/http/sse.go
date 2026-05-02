package http

import (
	"encoding/json"
	"fmt"
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

func newSSE(w http.ResponseWriter) *sse {
	flusher, _ := w.(http.Flusher)
	return &sse{w: w, flusher: flusher}
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
		// Best-effort: emit a fallback error frame.
		fmt.Fprintf(s.w, "event: error\ndata: {\"type\":\"error\",\"error\":\"marshal: %s\"}\n\n", err.Error())
		s.flush()
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev.Type, payload)
	s.flush()
}

func (s *sse) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
