package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// sse wraps an http.ResponseWriter for server-sent-events output. One sse
// per connection.
//
// Concurrency: every write to s.w goes through s.mu so the main agent-loop
// goroutine and the optional keepalive goroutine (started by startKeepalive)
// don't interleave bytes on the wire. net/http does NOT serialise concurrent
// writes from multiple goroutines on a response writer — that's our job.
type sse struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	s.w.WriteHeader(http.StatusOK)
	s.flushLocked()
}

func (s *sse) send(ev providers.Event) {
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
		s.mu.Lock()
		defer s.mu.Unlock()
		fmt.Fprintf(s.w, "event: error\ndata: %s\n\n", fallback)
		s.flushLocked()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev.Type, payload)
	s.flushLocked()
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
	payload, err := json.Marshal(data)
	if err != nil {
		log.Printf("sse: sendRaw marshal failed for %q: %v", eventName, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventName, payload)
	s.flushLocked()
}

// startKeepalive starts a goroutine that emits SSE comment-only frames
// (`:keepalive\n\n`) on the configured interval until ctx fires. SSE
// comments are required-ignored by clients per WHATWG, so they don't
// surface as events to downstream consumers — they exist purely to
// keep the underlying TCP/HTTP path from going idle.
//
// Why this matters: agent runs that fan out to sub-agents (parent +
// company-researcher children, for example) can sit minutes between
// real events while a child is mid-WebFetch. Networks with idle
// connection timeouts (Tailscale, NAT routers, some reverse proxies)
// can drop a silent stream and undici-side surfaces this as
// `TypeError: terminated` with no diagnostic context. Periodic
// comment frames keep bytes flowing and make this class of drops a
// non-event.
//
// Safe to call once per stream after start(). No-op when the writer
// doesn't support streaming (newSSE returned ok=false). The goroutine
// holds no references that could outlive the request — when ctx
// cancels (handler return, client disconnect), it exits next tick.
func (s *sse) startKeepalive(ctx context.Context, interval time.Duration) {
	if s.flusher == nil || interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.writeKeepalive()
			}
		}
	}()
}

// writeKeepalive emits one comment-only SSE frame. Errors are
// swallowed: a write failure means the connection is gone, in which
// case the next real send() will surface the underlying error or the
// handler will return on ctx done.
func (s *sse) writeKeepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.WriteString(s.w, ": keepalive\n\n"); err != nil {
		return
	}
	s.flushLocked()
}

// flushLocked must be called with s.mu held.
func (s *sse) flushLocked() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
