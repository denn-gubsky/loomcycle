package http

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// llm_gateway.go — v0.11.0 LLM Gateway endpoint.
//
// POST /v1/_llm/chat exposes the resolver + provider auth + retry
// layer as a direct LLM call surface, bypassing the agent loop.
//
// What this handler DOES:
//   - Parse + validate the wire request shape
//   - Run the resolver with the request's tier / explicit-pin hints
//   - Look up the chosen provider from the registry
//   - Acquire a per-user semaphore slot when user_id is set
//   - Translate the wire shape into a providers.Request
//   - Call provider.Call() directly (no agent loop)
//   - Stream events as SSE OR collect into a non-streaming response
//   - Log a structured "llm_gateway: ..." line per request for
//     always-on audit (v0.11.1 will add a dedicated audit-event table)
//
// What this handler does NOT do (deliberately):
//   - No runs-table row per request — gateway calls are too high-cardinality
//   - No cross-provider mid-call fallback — single call per request;
//     same-provider rate-limit retry inside the driver still applies
//   - No hooks, no snapshots, no transcript persistence
//   - No `runs/<id>` audit-event row (the events table has a NOT NULL
//     FK to runs which we don't want to fake)
//
// Bearer-authed admin scope (same authMiddleware as every /v1/_*
// endpoint). Operator-trust callers only.

const (
	llmGatewayMaxRequestBytes  = 1 << 20 // 1 MiB cap on request body
	llmGatewayDefaultMaxTokens = 4096
)

// handleLLMChat serves POST /v1/_llm/chat. Supports both stream:false
// (single JSON response) and stream:true (SSE) based on the request
// body flag.
func (s *Server) handleLLMChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, llmGatewayMaxRequestBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "empty body")
		return
	}

	var req llmChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}

	d, ok := s.prepareGatewayDispatch(w, r, &req)
	if !ok {
		return
	}
	defer d.release()

	if req.Stream {
		s.serveLLMChatStream(w, r, &req, d.requestID, d.providerID, d.modelID, d.provider, d.provReq, d.startedAt)
		return
	}
	s.serveLLMChatJSON(w, r, &req, d.requestID, d.providerID, d.modelID, d.provider, d.provReq, d.startedAt)
}

// gatewayDispatch carries the validated + resolved state both the
// native loomcycle handler and the v0.11.3 OpenAI-compat shim need to
// proceed past the parse step. Centralising the validation +
// resolve + semaphore-acquire flow here keeps security policy
// (per-user quota, resolver pin precedence, log auditing) in one
// place — the shim is a wire-format translator, not a separate
// dispatch path.
type gatewayDispatch struct {
	requestID  string
	providerID string
	modelID    string
	provider   providers.Provider
	provReq    providers.Request
	startedAt  time.Time
	release    func()
}

// prepareGatewayDispatch runs the shared validation + resolution +
// quota-acquire steps. Writes the error response and returns ok=false
// when any step fails; otherwise returns a dispatch handle the caller
// uses to serve the response in its native wire format. Caller MUST
// defer dispatch.release() to release the semaphore slot.
func (s *Server) prepareGatewayDispatch(w http.ResponseWriter, r *http.Request, req *llmChatRequest) (gatewayDispatch, bool) {
	if len(req.Messages) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "messages is required and must be non-empty")
		return gatewayDispatch{}, false
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = llmGatewayDefaultMaxTokens
	}

	requestID := newRequestID()
	startedAt := time.Now()

	providerID, modelID, effort, err := s.resolveGatewayRequest(req)
	if err != nil {
		// Mirror the agent-run path: ErrTierUnavailable /
		// ErrPinUnavailable map to 503 (retryable) so adapter
		// consumers can apply retry-with-backoff. Everything else
		// stays 400 (client config issue).
		writeJSONError(w, resolveErrorToStatus(err), "resolve_failed", err.Error())
		return gatewayDispatch{}, false
	}
	provider, err := s.providers.Get(providerID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "provider_unavailable", err.Error())
		return gatewayDispatch{}, false
	}

	// Per-subject quota acquisition. RFC L: when an authenticated
	// principal is present its Subject is the authoritative fairness key
	// (a caller can't forge a different user_id to dodge their cap); the
	// wire user_id is only the fallback for open / un-authed mode. Empty
	// key bypasses the per-user cap (global semaphore only).
	release, err := s.sem.AcquireForUser(r.Context(), auth.SubjectForFairness(r.Context(), req.UserID))
	if err != nil {
		writeQuotaError(w, err)
		return gatewayDispatch{}, false
	}

	provReq, err := llmRequestToProviderRequest(req, modelID, effort)
	if err != nil {
		release()
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
		return gatewayDispatch{}, false
	}
	// Force-stream the provider call: every driver streams natively,
	// and we re-aggregate for the non-streaming response path. Lets
	// us emit content_block_delta frames live when stream:true.
	provReq.Stream = true

	return gatewayDispatch{
		requestID:  requestID,
		providerID: providerID,
		modelID:    modelID,
		provider:   provider,
		provReq:    provReq,
		startedAt:  startedAt,
		release:    release,
	}, true
}

// serveLLMChatJSON drains the provider channel and writes a single
// JSON response per the non-streaming wire shape.
func (s *Server) serveLLMChatJSON(
	w http.ResponseWriter, r *http.Request,
	req *llmChatRequest, requestID, providerID, modelID string,
	provider providers.Provider, provReq providers.Request,
	startedAt time.Time,
) {
	ch, err := provider.Call(r.Context(), provReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "provider_call_failed", err.Error())
		return
	}
	id := newLLMID()
	resp, err := collectProviderEventsIntoResponse(ch, id, requestID, providerID, modelID)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "provider_call_failed", err.Error())
		return
	}
	logGatewayRequest(requestID, providerID, modelID, req.Tier, req.UserID,
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.StopReason,
		time.Since(startedAt), nil)
	writeJSONOK(w, resp)
}

// serveLLMChatStream proxies the provider event channel out as SSE
// frames in the Anthropic-style streaming format. Emits provider_chosen
// first, then content_block_delta per token, message_delta + done at
// completion.
func (s *Server) serveLLMChatStream(
	w http.ResponseWriter, r *http.Request,
	req *llmChatRequest, requestID, providerID, modelID string,
	provider providers.Provider, provReq providers.Request,
	startedAt time.Time,
) {
	stream, ok := newSSE(w)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming_unsupported",
			"response writer does not support streaming")
		return
	}
	stream.start()
	stream.startKeepalive(r.Context(), 15*time.Second)
	stream.sendRaw("provider_chosen", llmStreamProviderChosen{
		Provider:  providerID,
		Model:     modelID,
		RequestID: requestID,
	})

	ch, err := provider.Call(r.Context(), provReq)
	if err != nil {
		stream.sendRaw("error", llmStreamError{
			Type:    "provider_error",
			Code:    "provider_call_failed",
			Message: err.Error(),
		})
		logGatewayRequest(requestID, providerID, modelID, req.Tier, req.UserID, 0, 0, "error", time.Since(startedAt), err)
		return
	}

	// Block-index lifecycle: the gateway owns the index exclusively
	// here (translate helpers in llm_gateway_translate.go don't
	// mutate it). nextBlockIndex is the index that WILL be assigned
	// to the next content_block_start. openBlockIndex is the index
	// of the currently-open block, or -1 when no block is open.
	// Starting at 0 guarantees the first block — text or tool_use —
	// lands at index 0, matching Anthropic's protocol contract that
	// adapters key off.
	nextBlockIndex := 0
	openBlockIndex := -1
	var (
		usage      llmUsage
		stopReason string
		id         = newLLMID()
		runErr     error
	)
	closeOpenBlock := func() {
		if openBlockIndex >= 0 {
			stream.sendRaw("content_block_stop", llmStreamContentBlockStop{Index: openBlockIndex})
			openBlockIndex = -1
		}
	}
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			if openBlockIndex < 0 {
				stream.sendRaw("content_block_start", llmStreamContentBlockStart{
					Index: nextBlockIndex,
					Block: llmContentBlock{Type: "text", Text: ""},
				})
				openBlockIndex = nextBlockIndex
				nextBlockIndex++
			}
			stream.sendRaw("content_block_delta", llmStreamContentBlockDelta{
				Index: openBlockIndex,
				Delta: llmStreamDelta{Type: "text_delta", Text: ev.Text},
			})
		case providers.EventToolCall:
			block := toolUseBlockFromEvent(ev)
			if block == nil {
				continue
			}
			closeOpenBlock()
			stream.sendRaw("content_block_start", llmStreamContentBlockStart{
				Index: nextBlockIndex,
				Block: *block,
			})
			openBlockIndex = nextBlockIndex
			nextBlockIndex++
			closeOpenBlock()
		case providers.EventUsage:
			if ev.Usage != nil {
				usage = llmUsage{
					InputTokens:              ev.Usage.InputTokens,
					OutputTokens:             ev.Usage.OutputTokens,
					CacheCreationInputTokens: ev.Usage.CacheCreationTokens,
					CacheReadInputTokens:     ev.Usage.CacheReadTokens,
				}
			}
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
			if frame := usageFrameFromEvent(ev); frame != nil {
				stream.sendRaw("message_delta", *frame)
			}
		case providers.EventDone:
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
		case providers.EventError:
			closeOpenBlock()
			stream.sendRaw("error", llmStreamError{
				Type:    "provider_error",
				Code:    "provider_call_failed",
				Message: ev.Error,
			})
			runErr = errors.New(ev.Error)
		}
	}
	closeOpenBlock()
	if stopReason == "" && runErr == nil {
		stopReason = "end_turn"
	}
	if runErr == nil {
		stream.sendRaw("done", llmStreamDone{
			ID:         id,
			StopReason: stopReason,
			Usage:      usage,
		})
	}
	logGatewayRequest(requestID, providerID, modelID, req.Tier, req.UserID,
		usage.InputTokens, usage.OutputTokens, stopReason,
		time.Since(startedAt), runErr)
}

// resolveGatewayRequest applies the RFC §"Provider routing" precedence:
//
//  1. Explicit provider + model — both pinned; resolver still validates.
//  2. Explicit provider only — resolver picks the best model in that
//     provider given tier/user_tier.
//  3. Explicit model only — resolver picks the provider hosting that
//     model.
//  4. Neither — full resolver pick by tier/user_tier.
//
// All paths honor the user_tier overlay when set.
func (s *Server) resolveGatewayRequest(req *llmChatRequest) (providerID, modelID, effort string, err error) {
	// When BOTH provider and model are set, the resolver's pin path
	// is essentially a validation pass: it checks the candidate is in
	// the operator's matrix (or accepts it as an explicit pin). No
	// tier needed.
	if req.Provider != "" && req.Model != "" {
		// v1 simplifies by trusting an explicit pin. The provider
		// lookup downstream (s.providers.Get) is the validation
		// gate — registry returns an error if the provider isn't
		// registered.
		return req.Provider, req.Model, "", nil
	}

	// Tier-driven path. Need a resolver for any path that isn't a
	// full explicit pin.
	if s.resolver == nil {
		return "", "", "", errors.New("resolver not configured; explicit provider+model required")
	}
	tier := req.Tier
	if tier == "" {
		tier = "default"
	}
	r := resolve.AgentRequest{
		Name:        "llm_gateway",
		PinProvider: req.Provider,
		PinModel:    req.Model,
		Tier:        tier,
		UserTier:    s.userTierOverlay(req.UserTier),
	}
	dec, err := s.resolver.Resolve(r)
	if err != nil {
		return "", "", "", err
	}
	return dec.Provider, dec.Model, dec.Effort, nil
}

// logGatewayRequest emits the always-on structured audit line. The
// `llm_gateway:` prefix lets operators scrape via `grep llm_gateway`
// or a log shipper rule. v0.11.1 follow-up adds a dedicated gateway
// events table backed by a queryable HTTP endpoint.
func logGatewayRequest(
	requestID, providerID, modelID, tier, userID string,
	inputTokens, outputTokens int,
	stopReason string,
	latency time.Duration,
	runErr error,
) {
	status := "ok"
	if runErr != nil {
		status = "error"
	}
	log.Printf(
		"llm_gateway: request_id=%s provider=%s model=%s tier=%q user_id=%q input_tokens=%d output_tokens=%d stop_reason=%s latency_ms=%d status=%s err=%q",
		requestID, providerID, modelID, tier, userID,
		inputTokens, outputTokens, stopReason,
		latency.Milliseconds(),
		status,
		errString(runErr),
	)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

func newLLMID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "llm_" + hex.EncodeToString(b[:])
}
