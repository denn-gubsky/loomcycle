package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// openai_compat.go — v0.11.3 OpenAI Chat Completions compatibility
// shim. Serves POST /v1/chat/completions in the exact wire shape the
// OpenAI Python / TypeScript SDKs (and every "use OpenAI as your
// LLM" tutorial) expect.
//
// Architecture:
//   1. Parse the OpenAI-shaped request.
//   2. Translate into loomcycle's native llmChatRequest.
//   3. Delegate to prepareGatewayDispatch (shared with /v1/_llm/chat)
//      so security policy, resolver pin precedence, per-user quota
//      acquisition, and structured audit logging happen in one place.
//   4. Translate the dispatched response back into OpenAI's chunk /
//      completion shapes.
//
// The shim itself owns ZERO substrate logic — it's pure wire-format
// translation. A bug in routing / quota / retry shows up in the
// native /v1/_llm/chat path too; a bug here is a translation bug.
//
// Consumers benefit: every existing OpenAI-SDK tool (Aider, Goose,
// Continue, Cursor, Cody, custom code, every "use OpenAI" tutorial)
// can route through loomcycle by changing only their base URL +
// auth token to point at the loomcycle server. They get loomcycle's
// resolver routing (multi-provider, tier-aware), retry, per-user
// quota, audit — without writing a single line of loomcycle-specific
// code.

const openaiCompatMaxRequestBytes = 1 << 20 // 1 MiB; same cap as native gateway

// handleOpenAICompatChat serves POST /v1/chat/completions.
func (s *Server) handleOpenAICompatChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, openaiCompatMaxRequestBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "empty body")
		return
	}

	var oreq openaiChatRequest
	if err := json.Unmarshal(body, &oreq); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}

	req, err := openaiRequestToLLMChatRequest(&oreq)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	d, ok := s.prepareGatewayDispatch(w, r, req)
	if !ok {
		return
	}
	defer d.release()

	if req.Stream {
		s.serveOpenAICompatStream(w, r, req, d)
		return
	}
	s.serveOpenAICompatJSON(w, r, req, d)
}

// openaiRequestToLLMChatRequest is the request-side translator.
// Returns an error when the request shape is malformed (unknown
// role, message with neither content nor tool_calls, etc.).
func openaiRequestToLLMChatRequest(o *openaiChatRequest) (*llmChatRequest, error) {
	messages := make([]llmChatMessage, 0, len(o.Messages))
	for i, m := range o.Messages {
		text, err := openaiMessageContentToString(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d].content: %w", i, err)
		}
		msg := llmChatMessage{
			Role:       m.Role,
			Content:    text,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			// OpenAI tool_call_function.arguments is a JSON STRING
			// (provider streamed it that way); loomcycle's native
			// shape wants the parsed object. Try to parse; if the
			// model emitted invalid JSON, fall back to passing the
			// raw string through (debugging-friendly — the
			// downstream provider sees what the upstream model
			// emitted verbatim).
			var input json.RawMessage = []byte(tc.Function.Arguments)
			if tc.Function.Arguments == "" {
				input = []byte("{}")
			}
			msg.ToolCalls = append(msg.ToolCalls, llmChatToolCall{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		messages = append(messages, msg)
	}

	tools := make([]llmChatTool, 0, len(o.Tools))
	for i, t := range o.Tools {
		if t.Type != "function" && t.Type != "" {
			return nil, fmt.Errorf("tools[%d].type: only %q supported (got %q)", i, "function", t.Type)
		}
		tools = append(tools, llmChatTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}

	// Resolve the user_id with this precedence: explicit
	// `loomcycle_user_id` (operator opt-in) > OpenAI's `user`
	// (drop-in from SDK callers). Neither set → anonymous bypass of
	// per-user cap (operator-trust bearer is still required).
	userID := o.LoomcycleUserID
	if userID == "" {
		userID = o.User
	}

	return &llmChatRequest{
		Messages:    messages,
		Tools:       tools,
		MaxTokens:   o.MaxTokens,
		Temperature: o.Temperature,
		Stream:      o.Stream,
		Model:       o.Model,
		Provider:    o.LoomcycleProvider,
		Tier:        o.LoomcycleTier,
		UserID:      userID,
		UserTier:    o.LoomcycleUserTier,
	}, nil
}

// openaiMessageContentToString flattens OpenAI's polymorphic
// `content` field into a plain string. v1 handles three shapes:
//   - `null` → empty string (assistant turns with tool_calls)
//   - `"text"` → unwrapped
//   - `[{type:"text", text:"..."}, ...]` → text parts concatenated
//     (any non-text parts skipped with no error — multimodal isn't
//     supported yet)
func openaiMessageContentToString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content must be string, null, or array of parts; got %s", typeOfRaw(raw))
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
		// image/audio parts silently skipped (v1 scope cut).
	}
	return sb.String(), nil
}

func typeOfRaw(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if len(s) == 0 {
		return "empty"
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

// stopReasonToFinishReason maps loomcycle's stop_reason values onto
// OpenAI's finish_reason values per the OpenAI spec.
func stopReasonToFinishReason(stop string) string {
	switch stop {
	case "end_turn", "stop_sequence", "":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	return "stop"
}

// serveOpenAICompatJSON drains the provider channel into a single
// OpenAI-shaped completion response.
func (s *Server) serveOpenAICompatJSON(
	w http.ResponseWriter, r *http.Request,
	req *llmChatRequest, d gatewayDispatch,
) {
	ch, err := d.provider.Call(r.Context(), d.provReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "provider_call_failed", err.Error())
		return
	}
	id := newLLMID()
	resp, err := collectProviderEventsIntoResponse(ch, id, d.requestID, d.providerID, d.modelID)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "provider_call_failed", err.Error())
		return
	}
	logGatewayRequest(d.requestID, d.providerID, d.modelID, req.Tier, req.UserID,
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.StopReason,
		time.Since(d.startedAt), nil)
	writeJSONOK(w, llmResponseToOpenAI(&resp))
}

// llmResponseToOpenAI translates loomcycle's native non-streaming
// response into the OpenAI Chat Completion shape.
func llmResponseToOpenAI(r *llmChatResponse) openaiChatResponse {
	msg := openaiChatMessage{Role: "assistant"}
	var textBuf strings.Builder
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			textBuf.WriteString(block.Text)
		case "tool_use":
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, openaiToolCall{
				ID:   block.ID,
				Type: "function",
				Function: openaiToolCallFunction{
					Name:      block.Name,
					Arguments: args,
				},
			})
		}
	}
	if textBuf.Len() > 0 {
		raw, _ := json.Marshal(textBuf.String())
		msg.Content = raw
	} else {
		// OpenAI spec requires `content` to be present on every
		// assistant message — string or null, never absent. With
		// the `omitempty` tag, a nil json.RawMessage drops the
		// field entirely from the JSON output; the OpenAI Python
		// + TypeScript SDKs key off `content: null` to recognise
		// tool-call-only turns and the absence trips Zod / Pydantic
		// strict-mode validators. Explicitly emit the literal
		// `null` bytes so the field appears on the wire.
		msg.Content = json.RawMessage("null")
	}
	return openaiChatResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   r.Model,
		Choices: []openaiChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: stopReasonToFinishReason(r.StopReason),
		}},
		Usage: openaiUsage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
		},
	}
}

// serveOpenAICompatStream proxies the provider event channel out as
// SSE in OpenAI's `chat.completion.chunk` format. Frames are
// `data: <json>\n\n` only — OpenAI doesn't use named SSE events
// (unlike the native loomcycle stream). Terminal frame is the
// literal `data: [DONE]\n\n`.
func (s *Server) serveOpenAICompatStream(
	w http.ResponseWriter, r *http.Request,
	req *llmChatRequest, d gatewayDispatch,
) {
	stream, ok := newSSE(w)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming_unsupported",
			"response writer does not support streaming")
		return
	}
	stream.start()
	stream.startKeepalive(r.Context(), 15*time.Second)

	id := newLLMID()
	created := time.Now().Unix()

	// First chunk announces the assistant role (per OpenAI's stream
	// protocol; SDKs key off this to know the response started).
	emitChunk := func(delta openaiChunkDelta, finishReason *string, usage *openaiUsage) {
		chunk := openaiChunkResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   d.modelID,
			Choices: []openaiChunkChoice{{
				Index:        0,
				Delta:        delta,
				FinishReason: finishReason,
			}},
			Usage: usage,
		}
		stream.sendOpenAIData(chunk)
	}

	emitChunk(openaiChunkDelta{Role: "assistant"}, nil, nil)

	ch, err := d.provider.Call(r.Context(), d.provReq)
	if err != nil {
		// Emit an error chunk + terminal [DONE]. OpenAI's stream
		// protocol doesn't have a standardised error event; some
		// SDKs detect non-2xx HTTP, but we're already in 200-OK SSE
		// at this point. Emit the error as a content delta on the
		// `error` channel + force-close with finish_reason: "stop"
		// so SDK iterators terminate.
		fr := "stop"
		emitChunk(openaiChunkDelta{Content: "[loomcycle: provider call failed: " + err.Error() + "]"}, &fr, nil)
		stream.sendOpenAIDone()
		logGatewayRequest(d.requestID, d.providerID, d.modelID, req.Tier, req.UserID, 0, 0, "error", time.Since(d.startedAt), err)
		return
	}

	// Tool-call index tracking — OpenAI requires per-tool index +
	// `id`/`type`/`function.name` on the FIRST chunk for each tool,
	// then arguments-only deltas. v1 emits each tool as a single
	// chunk (full args at once); the index counter starts at 0 per
	// message and increments per tool.
	nextToolIndex := 0
	var (
		usage      openaiUsage
		stopReason string
		runErr     error
	)
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			emitChunk(openaiChunkDelta{Content: ev.Text}, nil, nil)
		case providers.EventToolCall:
			if ev.ToolUse == nil {
				continue
			}
			args := string(ev.ToolUse.Input)
			if args == "" {
				args = "{}"
			}
			emitChunk(openaiChunkDelta{
				ToolCalls: []openaiChunkToolCall{{
					Index: nextToolIndex,
					ID:    ev.ToolUse.ID,
					Type:  "function",
					Function: &openaiChunkToolCallFn{
						Name:      ev.ToolUse.Name,
						Arguments: args,
					},
				}},
			}, nil, nil)
			nextToolIndex++
		case providers.EventUsage:
			if ev.Usage != nil {
				usage = openaiUsage{
					PromptTokens:     ev.Usage.InputTokens,
					CompletionTokens: ev.Usage.OutputTokens,
					TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
				}
			}
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
		case providers.EventDone:
			if ev.StopReason != "" {
				stopReason = ev.StopReason
			}
		case providers.EventError:
			// SECURITY: provider error messages can contain
			// operator-internal diagnostic text (rate-limit tier
			// names, model identifiers, endpoint paths, fragments
			// of request headers from auth-failure responses). The
			// native /v1/_llm/chat path emits a structured
			// `event: error` frame so this stays in a clearly-
			// labelled side-channel; OpenAI's protocol has no
			// equivalent typed-error frame, so we'd otherwise
			// embed the diagnostic in user-visible delta.content.
			// Emit a fixed placeholder instead — the full error
			// surfaces server-side via logGatewayRequest(runErr).
			fr := "stop"
			emitChunk(openaiChunkDelta{Content: "[loomcycle: provider error]"}, &fr, nil)
			runErr = fmt.Errorf("%s", ev.Error)
			// Drain the rest of the channel without emitting
			// further chunks — current drivers close the channel
			// immediately after EventError, but unguarded the
			// loop would emit content frames AFTER the terminal
			// finish_reason chunk on any future driver that
			// emits trailing events. SDK iterators on the client
			// side have already committed to terminal state.
			for range ch {
			}
		}
	}

	// Final chunk carries the finish_reason + usage.
	if runErr == nil {
		fr := stopReasonToFinishReason(stopReason)
		emitChunk(openaiChunkDelta{}, &fr, &usage)
	}
	stream.sendOpenAIDone()
	logGatewayRequest(d.requestID, d.providerID, d.modelID, req.Tier, req.UserID,
		usage.PromptTokens, usage.CompletionTokens, stopReason,
		time.Since(d.startedAt), runErr)
}
