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
//
// Lifetime: the handler that streams defers end(). net/http cancels the
// request ctx only AFTER ServeHTTP returns and then flushes the response's
// buffered writer back to a pool shared with other connections, so a write
// that arrives once the handler is gone can race that teardown or land in
// another connection's response. end() runs before the return and makes
// every later write a no-op.
type sse struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
	// ended is set by end(); every write after it is dropped. Guarded by mu.
	ended bool
	// Set once startKeepalive has started its goroutine (guarded by mu):
	// end() closes keepaliveStop and waits for keepaliveDone.
	keepaliveStop chan struct{}
	keepaliveDone chan struct{}
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
	if s.ended {
		return
	}
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
		if s.ended {
			return
		}
		fmt.Fprintf(s.w, "event: error\ndata: %s\n\n", fallback)
		s.flushLocked()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
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
	if s.ended {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventName, payload)
	s.flushLocked()
}

// sendOpenAIData emits one OpenAI-style SSE chunk: `data: <json>\n\n`
// with NO `event:` line. The OpenAI Chat Completions stream protocol
// uses bare `data:` frames (unlike loomcycle's native event-named
// frames). Used by the v0.11.3 OpenAI-compat shim.
func (s *sse) sendOpenAIData(data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		log.Printf("sse: sendOpenAIData marshal failed: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", payload)
	s.flushLocked()
}

// sendOpenAIDone emits the literal `data: [DONE]\n\n` terminator
// frame OpenAI SDKs key off to know the stream ended.
func (s *sse) sendOpenAIDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	fmt.Fprint(s.w, "data: [DONE]\n\n")
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
// stops on ctx (client disconnect) or on end(), which also waits for it
// to exit: the request ctx alone is cancelled only after the handler has
// returned, too late to keep a tick off a finished response.
func (s *sse) startKeepalive(ctx context.Context, interval time.Duration) {
	if s.flusher == nil || interval <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || s.keepaliveStop != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	s.keepaliveStop, s.keepaliveDone = stop, done
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
				s.writeKeepalive()
			}
		}
	}()
}

// end closes the stream for writing: it drops every later write and stops
// the keepalive goroutine, waiting for it to exit. Taking mu also waits out
// a write already in flight, so once end returns nothing in this package
// touches the ResponseWriter again. Handlers defer it right after starting
// the stream. Idempotent.
func (s *sse) end() {
	s.mu.Lock()
	already := s.ended
	s.ended = true
	stop, done := s.keepaliveStop, s.keepaliveDone
	s.mu.Unlock()
	if stop == nil {
		return
	}
	if !already {
		close(stop)
	}
	// Outside mu: the goroutine may be waiting on it to find the stream ended.
	<-done
}

// writeKeepalive emits one comment-only SSE frame. Errors are
// swallowed: a write failure means the connection is gone, in which
// case the next real send() will surface the underlying error or the
// handler will return on ctx done.
func (s *sse) writeKeepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
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
