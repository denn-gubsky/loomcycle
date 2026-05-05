// Package http serves the HTTP+SSE API.
//
// One endpoint matters at v0.1: POST /v1/runs streams agent events as SSE.
// /healthz is the unauthenticated liveness probe.
package http

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
)

// ProviderResolver returns a Provider by ID. The cmd/loomcycle main constructs one
// per provider on startup and passes the lookup in. Keeping this an interface
// keeps the api package free of concrete Anthropic/OpenAI/Ollama wiring.
type ProviderResolver interface {
	Get(id string) (providers.Provider, error)
}

// Server holds dependencies and serves HTTP requests.
type Server struct {
	cfg       *config.Config
	providers ProviderResolver
	tools     []tools.Tool
	sem       *concurrency.Semaphore
	store     store.Store // optional; nil means "don't persist"

	// sessionLocks maps session IDs to *sync.Mutex. A continuation POST
	// (handleMessages, or handleRuns with a non-empty SessionID) try-locks
	// the session before replaying transcript + running. Concurrent POSTs
	// to the same session fast-fail with 409, since the alternative is to
	// leave a second SSE stream waiting indefinitely behind the first —
	// and a partial-transcript replay would corrupt history.
	//
	// Entries accumulate; never deleted. ~32 B per session is acceptable
	// for v0.3.2; periodic GC is a future cleanup.
	sessionLocks sync.Map
}

// New constructs a Server. If st is non-nil, every run is recorded as a
// session+run+events tuple in the store; pass nil to keep v0.2 behaviour.
//
// The Agent built-in tool is registered automatically here (not in
// cmd/loomcycle/main.go) because its SubAgentRunner closes over the
// Server's own runSubAgent method — we'd otherwise have a chicken-and-
// egg between tool list and Server. Per-agent allow-list still gates
// access (an agent without "Agent" in `allowed_tools` won't see it).
func New(cfg *config.Config, pr ProviderResolver, builtinTools []tools.Tool, sem *concurrency.Semaphore, st store.Store) *Server {
	s := &Server{cfg: cfg, providers: pr, tools: builtinTools, sem: sem, store: st}
	s.tools = append(s.tools, &builtin.AgentTool{Run: s.runSubAgent})
	return s
}

// trySessionLock try-locks the session-scoped mutex for id. Returns
// (release, true) on success and (nil, false) if another caller already
// holds it — in which case the caller should respond 409 / session_busy.
// id must be non-empty; an empty id is a programmer error and panics.
//
// Callers MUST validate the session exists in the store before calling
// this — sessionLocks entries are never GC'd, so taking a lock on an
// unknown id would leak permanently and is a vector for an unbounded
// memory DoS.
func (s *Server) trySessionLock(id string) (release func(), ok bool) {
	if id == "" {
		panic("trySessionLock: empty session id")
	}
	v, _ := s.sessionLocks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// lockedSessionCount returns the number of entries in sessionLocks.
// Test-only: used to assert the DoS fix (unknown IDs must not grow
// the table). sync.Map exposes no Len, so we range.
func (s *Server) lockedSessionCount() int {
	n := 0
	s.sessionLocks.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// Mux returns the http.Handler ready to be served.
//
// /v1 routes are wrapped with recovery middleware so a panic in the agent
// loop, a tool, or a provider driver returns a 500 to the caller instead
// of taking down the process. /healthz stays bare — it should never panic
// and a panic there is a programmer error worth crashing on.
func (s *Server) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("POST /v1/runs", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleRuns))))
	mux.Handle("GET /v1/sessions/{id}/transcript", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleTranscript))))
	mux.Handle("POST /v1/sessions/{id}/messages", recoveryMiddleware(s.authMiddleware(http.HandlerFunc(s.handleMessages))))
	return mux
}

// recoveryMiddleware turns a panicking handler into a 500. If headers have
// already been sent (the SSE path opens the stream before running anything
// that could panic), we can't write a status — we log and let the connection
// terminate.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic recovered in %s %s: %v", r.Method, r.URL.Path, rec)
				// Best-effort 500. If headers are already sent (SSE has
				// started writing) the WriteHeader call is a no-op and the
				// client sees the connection close, which is the cleanest
				// signal we can give at that point.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"internal server error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- handlers ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// runRequest is the JSON body shape for POST /v1/runs.
type runRequest struct {
	Agent        string               `json:"agent"`
	Segments     []loop.PromptSegment `json:"segments"`
	AllowedTools []string             `json:"allowed_tools,omitempty"`
	// AllowedHosts narrows the HTTP/WebFetch/WebSearch host allowlist
	// for THIS run only. nil/omitted = no narrowing (operator's static
	// list applies). Empty array `[]` = deny all (every network call
	// refuses). Non-empty = intersection with the operator list (caller
	// can shrink, never widen). The pointer-to-slice shape lets us
	// distinguish nil from empty in JSON.
	AllowedHosts *[]string `json:"allowed_hosts,omitempty"`
	// WebSearchFilter selects what happens to Brave search results
	// whose URL host isn't in the intersected AllowedHosts list:
	//   - "drop" (default when AllowedHosts is non-nil) omits non-
	//     matching results entirely; the model only sees URLs it can
	//     follow up on with WebFetch.
	//   - "keep" returns Brave's full result set; the caller filters
	//     downstream. Useful when the caller wants visibility into
	//     what Brave found before narrowing.
	// Ignored when AllowedHosts is nil.
	WebSearchFilter string `json:"web_search_filter,omitempty"`
	// SessionID is optional. When set, the new run is appended to that
	// session (the prior transcript is NOT replayed by /v1/runs — use
	// /v1/sessions/{id}/messages for continuation). When empty, a fresh
	// session is created. The new session ID is announced as the first
	// SSE event so the caller can address subsequent calls to it.
	SessionID string `json:"session_id,omitempty"`
	TenantID  string `json:"tenant_id,omitempty"`
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	// Cap body at 1 MiB so a malicious caller can't exhaust memory by
	// streaming a huge body. ReadHeaderTimeout doesn't cover the body.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		http.Error(w, `agent is required`, http.StatusBadRequest)
		return
	}

	agentDef, ok := s.cfg.Agents[req.Agent]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown agent %q", req.Agent), http.StatusBadRequest)
		return
	}

	providerID, model, err := s.cfg.ResolveAgentModel(req.Agent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Per-session continuation lock: when the caller is resuming an
	// existing session, serialize at the session level so two concurrent
	// POSTs can't replay overlapping transcripts. Fresh runs (empty
	// SessionID) skip this — they have no prior history to corrupt.
	//
	// CRITICAL: validate the session exists BEFORE taking the lock.
	// Otherwise an attacker can spam unknown IDs and each LoadOrStore
	// grows sessionLocks permanently (entries are never GC'd at v0.3.2).
	if req.SessionID != "" {
		if s.store == nil {
			http.Error(w, "session_id requires persistence (Store not configured)", http.StatusBadRequest)
			return
		}
		if _, err := s.store.GetSession(r.Context(), req.SessionID); err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		releaseSess, ok := s.trySessionLock(req.SessionID)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"code":"session_busy","error":"another request is in flight on session %q"}`, req.SessionID)
			return
		}
		defer releaseSess()
	}

	// Acquire concurrency slot first so backpressure is reported as 429
	// before we open the SSE stream.
	release, err := s.sem.Acquire(r.Context())
	if err != nil {
		if concurrency.IsBackpressure(err) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"code":"backpressure","error":%q}`, err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer release()

	// Filter tools by agent allowlist + caller request.
	allowedTools := filterTools(s.tools, agentDef.AllowedTools, req.AllowedTools)
	// Per-run host narrowing for HTTP/WebFetch/WebSearch. Behaviour
	// depends on LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE — see NarrowHosts
	// doc comment. In caller-authoritative mode we ALWAYS call so the
	// nil-fallback-to-operator path works; in default mode we only
	// call when the caller actually supplied a list.
	if req.AllowedHosts != nil || s.cfg.Env.HTTPCallerAuthoritative {
		var caller []string
		if req.AllowedHosts != nil {
			caller = *req.AllowedHosts
		}
		allowedTools = builtin.NarrowHosts(allowedTools, caller, req.WebSearchFilter, s.cfg.Env.HTTPCallerAuthoritative)
	}
	dispatcher := tools.NewDispatcher(allowedTools)

	// Optional system prompt from agent def.
	if agentDef.SystemPrompt != "" {
		req.Segments = append([]loop.PromptSegment{{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type:      "trusted-text",
				Text:      agentDef.SystemPrompt,
				Cacheable: true,
			}},
		}}, req.Segments...)
	}

	// Persistence: resolve or create a session, create a run, route every
	// emitted event through the store before forwarding to SSE. With
	// s.store == nil the recording becomes a no-op so v0.2 callers see no
	// behaviour change.
	sessionID, runID, sessErr := s.openOrCreateSessionAndRun(r.Context(), req.SessionID, req.Agent, req.TenantID)
	if sessErr != nil {
		var nf *store.ErrNotFound
		if errors.As(sessErr, &nf) {
			http.Error(w, sessErr.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, sessErr.Error(), http.StatusInternalServerError)
		return
	}

	// If we're persisting, record the caller's input segments as the first
	// event in the run. The loop never emits the caller's input itself, so
	// without this the transcript would start with the assistant's first
	// turn — and replay couldn't reconstruct the user prompt.
	if s.store != nil && runID != "" {
		if inputJSON, err := json.Marshal(req.Segments); err == nil {
			if err := s.store.AppendEvent(r.Context(), runID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed: %v", err)
			}
		}
	}

	stream, ok := newSSE(w)
	if !ok {
		// ResponseWriter doesn't implement http.Flusher — every frame would
		// be buffered until handler return, defeating SSE. Refuse cleanly so
		// the caller gets a useful error instead of silent buffering.
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()

	// Announce the (possibly newly-created) session/run IDs so the caller
	// can address continuation requests at the same session.
	if sessionID != "" {
		stream.send(providers.Event{
			Type: "session", // not part of providers.EventType — just a side-channel
			Text: sessionID,
		})
	}

	emit := s.makeRecordingEmit(r.Context(), runID, stream.send)

	// Pass the agent's effective tool names to the dispatcher so tools
	// that need a runtime view of "what this agent can use" (e.g. the
	// Skill tool's subset check on each call) read it via ctx instead
	// of being constructed per-run.
	loopCtx := tools.WithAgentTools(r.Context(), toolNames(allowedTools))

	loopRes, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:   provider,
		Model:      model,
		Tools:      allowedTools,
		Dispatcher: dispatcher,
		Segments:   req.Segments,
		OnEvent:    emit,
	})
	if runErr != nil {
		stream.send(providers.Event{Type: providers.EventError, Error: runErr.Error()})
	}

	s.finishRun(r.Context(), runID, loopRes, runErr)
}

// messagesRequest is the JSON body for POST /v1/sessions/{id}/messages. It
// only accepts new segments — agent / model / tools come from the session's
// existing config (looked up by session.Agent → cfg.Agents).
type messagesRequest struct {
	Segments     []loop.PromptSegment `json:"segments"`
	AllowedTools []string             `json:"allowed_tools,omitempty"`
	// AllowedHosts and WebSearchFilter mirror runRequest — see there
	// for the full semantics. Per-call: continuations re-supply the
	// list each time rather than inheriting from the seed call. This
	// keeps "what hosts can this run reach?" answerable from the
	// request alone, no session state to chase.
	AllowedHosts    *[]string `json:"allowed_hosts,omitempty"`
	WebSearchFilter string    `json:"web_search_filter,omitempty"`
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "session continuation requires persistence (Store not configured)", http.StatusNotFound)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id is required", http.StatusBadRequest)
		return
	}

	var body messagesRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the session exists BEFORE taking the per-session lock.
	// Otherwise an attacker can spam unknown IDs and each LoadOrStore
	// grows sessionLocks permanently (entries are never GC'd at v0.3.2).
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Per-session continuation lock: take the lock before transcript
	// replay so two concurrent POSTs to the same session can't read
	// half-written history. Fast-fail with 409 since the alternative —
	// blocking on an SSE handler — would hold an HTTP connection open
	// for the full length of the in-flight run.
	releaseSess, ok := s.trySessionLock(id)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"code":"session_busy","error":"another request is in flight on session %q"}`, id)
		return
	}
	defer releaseSess()

	// Resolve provider+model from the session's stored agent so the
	// continuation runs against the same model as the original session.
	agentDef, ok := s.cfg.Agents[sess.Agent]
	if !ok {
		http.Error(w, fmt.Sprintf("session refers to unknown agent %q", sess.Agent), http.StatusBadRequest)
		return
	}
	providerID, model, err := s.cfg.ResolveAgentModel(sess.Agent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Replay prior conversation history from the transcript.
	transcript, err := s.store.GetTranscript(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	priorMessages := replayTranscript(transcript)

	// Acquire concurrency slot before opening the SSE stream so backpressure
	// is reported as 429.
	release, err := s.sem.Acquire(r.Context())
	if err != nil {
		if concurrency.IsBackpressure(err) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"code":"backpressure","error":%q}`, err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer release()

	allowedTools := filterTools(s.tools, agentDef.AllowedTools, body.AllowedTools)
	if body.AllowedHosts != nil || s.cfg.Env.HTTPCallerAuthoritative {
		var caller []string
		if body.AllowedHosts != nil {
			caller = *body.AllowedHosts
		}
		allowedTools = builtin.NarrowHosts(allowedTools, caller, body.WebSearchFilter, s.cfg.Env.HTTPCallerAuthoritative)
	}
	dispatcher := tools.NewDispatcher(allowedTools)

	// Re-prepend the agent's system prompt — it isn't in the transcript
	// (it's per-call configuration, not conversation content).
	segments := body.Segments
	if agentDef.SystemPrompt != "" {
		segments = append([]loop.PromptSegment{{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type: "trusted-text", Text: agentDef.SystemPrompt, Cacheable: true,
			}},
		}}, segments...)
	}

	// Create a new run inside the existing session.
	run, err := s.store.CreateRun(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Persist the new user input segments so a future replay sees them.
	if inputJSON, err := json.Marshal(body.Segments); err == nil {
		if err := s.store.AppendEvent(r.Context(), run.ID, "user_input", inputJSON); err != nil {
			log.Printf("store: AppendEvent(user_input) failed: %v", err)
		}
	}

	stream, ok := newSSE(w)
	if !ok {
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
	stream.send(providers.Event{Type: "session", Text: id})

	emit := s.makeRecordingEmit(r.Context(), run.ID, stream.send)

	loopCtx := tools.WithAgentTools(r.Context(), toolNames(allowedTools))
	loopRes, runErr := loop.Run(loopCtx, loop.RunOptions{
		Provider:      provider,
		Model:         model,
		Tools:         allowedTools,
		Dispatcher:    dispatcher,
		Segments:      segments,
		PriorMessages: priorMessages,
		OnEvent:       emit,
	})
	if runErr != nil {
		stream.send(providers.Event{Type: providers.EventError, Error: runErr.Error()})
	}

	s.finishRun(r.Context(), run.ID, loopRes, runErr)
}

// replayTranscript walks the persisted events of a session and reconstructs
// the conversation history as []providers.Message, ready to feed into
// loop.Run via PriorMessages.
//
// The structure of a run in the event log:
//   - user_input        — segments the caller posted (one per run start)
//   - text              — assistant text deltas
//   - tool_call         — assistant requested a tool
//   - tool_result       — loop reports tool output (next user turn)
//   - usage / done      — loop bookkeeping; ignored for replay
//
// Each run boundary (new user_input event) marks the end of the previous
// assistant/user-tool-result turn pair.
func replayTranscript(events []store.Event) []providers.Message {
	var messages []providers.Message
	var asstText strings.Builder
	var asstTools []providers.ContentBlock
	var pendingToolResults []providers.ContentBlock

	flushAssistant := func() {
		if asstText.Len() == 0 && len(asstTools) == 0 {
			return
		}
		var content []providers.ContentBlock
		if asstText.Len() > 0 {
			content = append(content, providers.ContentBlock{Type: "text", Text: asstText.String()})
		}
		content = append(content, asstTools...)
		messages = append(messages, providers.Message{Role: "assistant", Content: content})
		asstText.Reset()
		asstTools = nil
	}
	flushPendingTools := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		messages = append(messages, providers.Message{Role: "user", Content: pendingToolResults})
		pendingToolResults = nil
	}

	for _, ev := range events {
		switch ev.Type {
		case "user_input":
			// New user turn: flush any in-progress assistant + tool_result accumulation.
			flushAssistant()
			flushPendingTools()
			var segs []loop.PromptSegment
			if err := json.Unmarshal(ev.Payload, &segs); err != nil {
				continue
			}
			var userBlocks []providers.ContentBlock
			for _, seg := range segs {
				if seg.Role != "user" {
					continue
				}
				for _, c := range seg.Content {
					userBlocks = append(userBlocks, loop.FlattenContent(c))
				}
			}
			if len(userBlocks) > 0 {
				messages = append(messages, providers.Message{Role: "user", Content: userBlocks})
			}
		case "text":
			// New assistant turn starting → close any prior user(tool_result)
			// turn that's still pending. We can't use "usage" as the boundary
			// because the loop emits usage BEFORE tool_result within an
			// iteration (see loop.go:163 vs loop.go:178), so usage-as-flush
			// would close the user turn before the tool_results land in it.
			flushPendingTools()
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil {
				asstText.WriteString(pe.Text)
			}
		case "tool_call":
			// Same reasoning as "text": this is a new assistant turn signal.
			flushPendingTools()
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ToolUse != nil {
				asstTools = append(asstTools, providers.ContentBlock{
					Type:      "tool_use",
					ToolUseID: pe.ToolUse.ID,
					ToolName:  pe.ToolUse.Name,
					ToolInput: pe.ToolUse.Input,
				})
			}
		case "tool_result":
			// The assistant turn that emitted tool_use is now complete; flush it.
			flushAssistant()
			var pe providers.Event
			if err := json.Unmarshal(ev.Payload, &pe); err == nil && pe.ToolUse != nil {
				pendingToolResults = append(pendingToolResults, providers.ContentBlock{
					Type:      "tool_result",
					ToolUseID: pe.ToolUse.ID,
					Text:      pe.Text,
					IsError:   pe.IsError,
				})
			}
			// Don't flush pendingToolResults yet — multiple tools at the
			// same boundary belong to one user message, and the next text
			// or tool_call event will close this user turn.
		case "done":
			// End-of-run boundary — only used when the final iteration was
			// purely textual (no tool_results to carry over).
			flushAssistant()
			flushPendingTools()
		}
	}
	flushAssistant()
	flushPendingTools()
	return messages
}

// transcriptResponse is the JSON shape of GET /v1/sessions/{id}/transcript.
type transcriptResponse struct {
	Session store.Session     `json:"session"`
	Events  []transcriptEvent `json:"events"`
}

// transcriptEvent is one event row, with payload re-decoded into a typed
// providers.Event so the caller doesn't have to round-trip through
// json.RawMessage. ts is unix-nanos so it round-trips losslessly.
type transcriptEvent struct {
	Seq   int64           `json:"seq"`
	RunID string          `json:"run_id"`
	TsNs  int64           `json:"ts_ns"`
	Type  string          `json:"type"`
	Event providers.Event `json:"event"`
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "transcript persistence is not configured", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id is required", http.StatusBadRequest)
		return
	}
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	transcript, err := s.store.GetTranscript(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := transcriptResponse{Session: sess, Events: make([]transcriptEvent, 0, len(transcript))}
	for _, ev := range transcript {
		te := transcriptEvent{
			Seq:   ev.Seq,
			RunID: ev.RunID,
			TsNs:  ev.Timestamp.UnixNano(),
			Type:  ev.Type,
		}
		// Decode payload back to a typed Event. If it fails (corrupt row),
		// surface a minimal record so the rest of the transcript still ships.
		if err := json.Unmarshal(ev.Payload, &te.Event); err != nil {
			te.Event = providers.Event{Type: providers.EventType(ev.Type)}
		}
		resp.Events = append(resp.Events, te)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("transcript: encode failed: %v", err)
	}
}

// openOrCreateSessionAndRun resolves the session (creating one if the caller
// didn't pass an ID), then creates a run inside it. Returns ("", "", nil) when
// no store is configured — the caller treats both empty IDs as "skip persistence".
func (s *Server) openOrCreateSessionAndRun(ctx context.Context, requestedSessionID, agent, tenantID string) (string, string, error) {
	if s.store == nil {
		return "", "", nil
	}
	var sess store.Session
	var err error
	if requestedSessionID != "" {
		sess, err = s.store.GetSession(ctx, requestedSessionID)
		if err != nil {
			return "", "", err
		}
	} else {
		sess, err = s.store.CreateSession(ctx, tenantID, agent)
		if err != nil {
			return "", "", err
		}
	}
	run, err := s.store.CreateRun(ctx, sess.ID)
	if err != nil {
		return "", "", err
	}
	return sess.ID, run.ID, nil
}

// makeRecordingEmit returns an OnEvent callback that records each event into
// the store before forwarding to the SSE stream. Persistence failures are
// logged but never block the stream — the caller has already received the
// event and should not be punished for our IO problems.
func (s *Server) makeRecordingEmit(ctx context.Context, runID string, fwd func(providers.Event)) func(providers.Event) {
	if s.store == nil || runID == "" {
		return fwd
	}
	return func(ev providers.Event) {
		payload, err := json.Marshal(ev)
		if err == nil {
			if err := s.store.AppendEvent(ctx, runID, string(ev.Type), payload); err != nil {
				log.Printf("store: AppendEvent failed (run=%s type=%s): %v", runID, ev.Type, err)
			}
		}
		fwd(ev)
	}
}

// runSubAgent is the SubAgentRunner closure injected into the Agent
// built-in tool. It looks up the named agent in cfg.Agents, builds a
// fresh session+run for the sub-execution, drives loop.Run with the
// sub-agent's full declared tool set, and returns the FinalText.
//
// Trust model: the sub-agent's `allowed_tools` is the SOLE authority on
// its tool surface. Parent and child are both operator-vetted YAML
// definitions; neither widens nor narrows the other. The Agent tool's
// availability to the parent is the gate (a parent without "Agent" in
// its allowed_tools cannot call this in the first place).
//
// Sub-runs are persisted as their OWN sessions for replayability. The
// parent's transcript records only the tool_call (with input) and
// tool_result (with the sub's final text); the sub's intermediate
// events are reachable via GET /v1/sessions/{sub-session-id}/transcript.
//
// Concurrency: sub-runs DO NOT acquire a fresh semaphore slot. They run
// inside the parent's slot — the entire run tree counts as one against
// MAX_CONCURRENT_RUNS. This avoids deadlocks at low concurrency caps
// and matches the cost model (a parent's compute budget already covers
// the work it delegates).
//
// Errors propagate as Go errors back to the Agent tool, which surfaces
// them as IsError tool_results to the parent's model rather than
// tearing down the parent run.
func (s *Server) runSubAgent(ctx context.Context, name string, prompt string) (string, error) {
	def, ok := s.cfg.Agents[name]
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q (not in loomcycle.yaml agents map)", name)
	}

	providerID, model, err := s.cfg.ResolveAgentModel(name)
	if err != nil {
		return "", fmt.Errorf("resolve sub-agent %q model: %w", name, err)
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		return "", fmt.Errorf("provider for sub-agent %q: %w", name, err)
	}

	// Sub-run gets its OWN session. Tenant inherited as empty for v0.4.0
	// MVP — multi-tenant agent inheritance lands when per-tenant
	// fairness does (later in v0.4 / v0.5).
	subSessionID, subRunID, err := s.openOrCreateSessionAndRun(ctx, "", name, "")
	if err != nil {
		return "", fmt.Errorf("create sub-session for %q: %w", name, err)
	}

	// Build segments: agent's system_prompt (with cache_control) + the
	// caller-supplied prompt as the first user message. Mirrors the
	// shape of /v1/runs.
	var segs []loop.PromptSegment
	if def.SystemPrompt != "" {
		segs = append(segs, loop.PromptSegment{
			Role: "system",
			Content: []loop.PromptContentBlock{{
				Type:      "trusted-text",
				Text:      def.SystemPrompt,
				Cacheable: true,
			}},
		})
	}
	segs = append(segs, loop.PromptSegment{
		Role: "user",
		Content: []loop.PromptContentBlock{{
			Type: "trusted-text",
			Text: prompt,
		}},
	})

	subTools := filterTools(s.tools, def.AllowedTools, nil)
	// Caller-authoritative HTTP host narrowing doesn't apply here:
	// sub-agents fall back to operator's static allowlist (no per-call
	// caller list). If the sub-agent itself uses HTTP and needs hosts
	// the operator's static list lacks, the operator must add them or
	// run loomcycle in CALLER_AUTHORITATIVE mode (where the static
	// list IS the fallback for runs without per-call narrowing).
	subDispatcher := tools.NewDispatcher(subTools)

	// Persist the input segments as the first event so transcript
	// replay reconstructs the user prompt the same way fresh runs do.
	if s.store != nil && subRunID != "" {
		if inputJSON, err := json.Marshal(segs); err == nil {
			if err := s.store.AppendEvent(ctx, subRunID, "user_input", inputJSON); err != nil {
				log.Printf("store: AppendEvent(user_input) failed for sub-run %s: %v", subRunID, err)
			}
		}
	}

	// Sub-emit records to the sub's transcript only — the parent's SSE
	// stream is fwd=no-op so sub events don't bleed into the parent's
	// event stream. The parent observes only the wrapping
	// tool_call/tool_result on its own stream.
	subEmit := s.makeRecordingEmit(ctx, subRunID, func(providers.Event) {})

	// Sub-run gets ITS OWN agent tools attached to ctx — the parent's
	// tool list does not leak to the child (and vice versa). This
	// matches the trust model: each agent's allowed_tools is its own
	// authority for any subset checks done inside its run.
	subCtx := tools.WithAgentTools(ctx, toolNames(subTools))

	res, runErr := loop.Run(subCtx, loop.RunOptions{
		Provider:   provider,
		Model:      model,
		Tools:      subTools,
		Dispatcher: subDispatcher,
		Segments:   segs,
		OnEvent:    subEmit,
	})
	s.finishRun(ctx, subRunID, res, runErr)

	if runErr != nil {
		// Wrap with session/run IDs so a developer reading parent logs
		// can locate the sub's transcript directly. The parent agent's
		// model sees the unwrapped error message.
		return "", fmt.Errorf("sub-agent %q failed (session=%s run=%s): %w",
			name, subSessionID, subRunID, runErr)
	}
	return res.FinalText, nil
}

// finishRun marks the run terminal in the store. status is derived from
// runErr: nil → completed, non-nil → failed. ctx may already be cancelled
// (the client disconnected); we use a fresh background context with a short
// timeout so the FinishRun write isn't lost.
func (s *Server) finishRun(_ context.Context, runID string, res loop.RunResult, runErr error) {
	if s.store == nil || runID == "" {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status := store.RunCompleted
	errMsg := ""
	if runErr != nil {
		status = store.RunFailed
		errMsg = runErr.Error()
	}
	usage := store.Usage{
		InputTokens:         res.Usage.InputTokens,
		OutputTokens:        res.Usage.OutputTokens,
		CacheCreationTokens: res.Usage.CacheCreationTokens,
		CacheReadTokens:     res.Usage.CacheReadTokens,
		Model:               res.Usage.Model,
	}
	if err := s.store.FinishRun(bg, runID, status, res.StopReason, usage, errMsg); err != nil {
		log.Printf("store: FinishRun failed (run=%s): %v", runID, err)
	}
}

// authMiddleware enforces LOOMCYCLE_AUTH_TOKEN bearer auth, except for /healthz which
// is mounted bare (this middleware is only wrapped around /v1/* routes).
//
// Comparison uses subtle.ConstantTimeCompare to prevent a timing oracle that
// could let a network-adjacent attacker recover the token byte-by-byte.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Env.AuthToken == "" {
			// No token configured = open mode (dev only). Startup logged a
			// warning so the operator knows.
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.cfg.Env.AuthToken
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// toolNames returns the names of a slice of tools — used to populate
// the per-run AgentTools context value for tools that need a runtime
// view of "what this agent can use" (e.g. the Skill tool's subset
// check on each call).
func toolNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// filterTools applies the agent + caller allowlists to the registered builtins.
// Glob suffixes ("mcp__brave-search__*") work via internal/tools/policy.
func filterTools(all []tools.Tool, agentAllowed, callerAllowed []string) []tools.Tool {
	if len(agentAllowed) == 0 {
		return nil
	}
	available := make([]string, 0, len(all))
	byName := make(map[string]tools.Tool, len(all))
	for _, t := range all {
		available = append(available, t.Name())
		byName[t.Name()] = t
	}
	allowed := policy.Apply(available, agentAllowed, callerAllowed)
	out := make([]tools.Tool, 0, len(allowed))
	for _, name := range allowed {
		if t, ok := byName[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Logger is the package-level logger; cmd/loomcycle may swap it out.
var Logger = log.Default()
