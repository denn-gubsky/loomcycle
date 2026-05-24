package http

import (
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// llm_gateway_translate.go — wire shape ⇄ providers.Request mapping.
//
// The gateway endpoint accepts an LangChain-friendly request shape
// (flat content strings, tool_calls / tool_call_id correlation) and
// emits an Anthropic-style streaming format on the wire. Internally
// the resolver + provider drivers already speak `providers.Request`
// with `[]ContentBlock` content arrays and `[]ToolSpec` tools. This
// file is the two adapter funcs that bridge the two shapes.
//
// The per-provider tool-schema translation (Anthropic's input_schema
// vs OpenAI's function.parameters vs Gemini's function_declarations
// nesting) happens inside each driver's buildRequestBody — NOT here.
// The gateway hands every driver the same providers.ToolSpec and the
// driver translates to its own wire. No new translation code.

// llmRequestToProviderRequest maps the gateway wire request into a
// providers.Request the driver consumes. Returns an error when the
// request is malformed (unknown role; tool message missing
// tool_call_id; etc.). The decided (provider, model, effort) are
// supplied by the caller — the gateway resolves them before this
// translation step.
func llmRequestToProviderRequest(req *llmChatRequest, model, effort string) (providers.Request, error) {
	var systemBlocks []providers.ContentBlock
	messages := make([]providers.Message, 0, len(req.Messages))

	for i, m := range req.Messages {
		switch m.Role {
		case "system":
			systemBlocks = append(systemBlocks, providers.ContentBlock{
				Type: "text",
				Text: m.Content,
			})
		case "user":
			messages = append(messages, providers.Message{
				Role: "user",
				Content: []providers.ContentBlock{
					{Type: "text", Text: m.Content},
				},
			})
		case "assistant":
			var blocks []providers.ContentBlock
			if m.Content != "" {
				blocks = append(blocks, providers.ContentBlock{
					Type: "text",
					Text: m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, providers.ContentBlock{
					Type:      "tool_use",
					ToolUseID: tc.ID,
					ToolName:  tc.Name,
					ToolInput: tc.Input,
				})
			}
			messages = append(messages, providers.Message{
				Role:    "assistant",
				Content: blocks,
			})
		case "tool":
			if m.ToolCallID == "" {
				return providers.Request{}, &jsonGatewayErr{
					Field:   "messages[" + itoa(i) + "].tool_call_id",
					Message: "tool message requires tool_call_id",
				}
			}
			messages = append(messages, providers.Message{
				Role: "user",
				Content: []providers.ContentBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Text:      m.Content,
				}},
			})
		default:
			return providers.Request{}, &jsonGatewayErr{
				Field:   "messages[" + itoa(i) + "].role",
				Message: "unknown role: " + m.Role,
			}
		}
	}

	toolSpecs := make([]providers.ToolSpec, 0, len(req.Tools))
	for _, t := range req.Tools {
		toolSpecs = append(toolSpecs, providers.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	return providers.Request{
		Model:       model,
		System:      systemBlocks,
		Messages:    messages,
		Tools:       toolSpecs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
		Effort:      effort,
	}, nil
}

// providerEventToLLMStreamFrame translates one provider Event into a
// gateway SSE frame name + payload. Returns ("", nil) for events the
// gateway deliberately drops (EventStarted; EventChannelPublish; etc.
// — events meaningful only inside the loop).
//
// Anthropic-style frame names are stable wire contract; consumers
// (the TS adapter's llmStream method first) string-match against them.
func providerEventToLLMStreamFrame(ev providers.Event, currentBlockIndex *int) (eventName string, payload any) {
	switch ev.Type {
	case providers.EventText:
		// One text delta on the current content block. The first delta
		// for a new "text" block needs a prior content_block_start —
		// the caller tracks index transitions via currentBlockIndex.
		// For simplicity v1 emits text deltas inline without
		// start/stop pairs; consumers reading content_block_delta with
		// type=text_delta know to concatenate.
		return "content_block_delta", llmStreamContentBlockDelta{
			Index: *currentBlockIndex,
			Delta: llmStreamDelta{Type: "text_delta", Text: ev.Text},
		}
	case providers.EventToolCall:
		if ev.ToolUse == nil {
			return "", nil
		}
		// Bump to a new content block for the tool_use. v1 emits the
		// full tool_use as a start-then-stop pair (no partial JSON
		// streaming) since most providers (except Anthropic's
		// extended-thinking-aware streaming) return the input as one
		// complete delta. Adapters that want partial-JSON streaming
		// can iterate v0.11.x.
		*currentBlockIndex++
		idx := *currentBlockIndex
		return "content_block_start", llmStreamContentBlockStart{
			Index: idx,
			Block: llmContentBlock{
				Type:  "tool_use",
				ID:    ev.ToolUse.ID,
				Name:  ev.ToolUse.Name,
				Input: ev.ToolUse.Input,
			},
		}
	case providers.EventUsage:
		if ev.Usage == nil {
			return "", nil
		}
		return "message_delta", llmStreamMessageDelta{
			Delta: llmMessageDeltaPayload{StopReason: ev.StopReason},
			Usage: llmUsage{
				InputTokens:              ev.Usage.InputTokens,
				OutputTokens:             ev.Usage.OutputTokens,
				CacheCreationInputTokens: ev.Usage.CacheCreationTokens,
				CacheReadInputTokens:     ev.Usage.CacheReadTokens,
			},
		}
	case providers.EventError:
		return "error", llmStreamError{
			Type:    "provider_error",
			Code:    "provider_call_failed",
			Message: ev.Error,
		}
	default:
		// EventStarted, EventDone, EventThinking, EventChannelPublish,
		// EventToolResult, EventRetry, EventCacheInvalidated,
		// EventProviderFallback, EventFallbackSuppressed,
		// EventReasoningInvalidated, EventChannelDelivery — these are
		// either loop-internal or surfaced via the dedicated done /
		// message_delta paths the caller emits.
		return "", nil
	}
}

// collectProviderEventsIntoResponse drains a provider's event channel
// into a non-streaming llmChatResponse. Used when the caller passed
// stream:false. Tool calls become tool_use content blocks; text
// chunks concatenate into a single text block.
func collectProviderEventsIntoResponse(
	ch <-chan providers.Event,
	id, requestID, provider, model string,
) (llmChatResponse, error) {
	resp := llmChatResponse{
		ID:        id,
		RequestID: requestID,
		Provider:  provider,
		Model:     model,
	}
	var textBuf string
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			textBuf += ev.Text
		case providers.EventToolCall:
			if textBuf != "" {
				resp.Content = append(resp.Content, llmContentBlock{
					Type: "text",
					Text: textBuf,
				})
				textBuf = ""
			}
			if ev.ToolUse != nil {
				resp.Content = append(resp.Content, llmContentBlock{
					Type:  "tool_use",
					ID:    ev.ToolUse.ID,
					Name:  ev.ToolUse.Name,
					Input: ev.ToolUse.Input,
				})
			}
		case providers.EventUsage:
			if ev.Usage != nil {
				resp.Usage = llmUsage{
					InputTokens:              ev.Usage.InputTokens,
					OutputTokens:             ev.Usage.OutputTokens,
					CacheCreationInputTokens: ev.Usage.CacheCreationTokens,
					CacheReadInputTokens:     ev.Usage.CacheReadTokens,
				}
			}
			if ev.StopReason != "" {
				resp.StopReason = ev.StopReason
			}
		case providers.EventDone:
			if ev.StopReason != "" {
				resp.StopReason = ev.StopReason
			}
		case providers.EventError:
			return resp, &gatewayProviderErr{Message: ev.Error}
		}
	}
	if textBuf != "" {
		resp.Content = append(resp.Content, llmContentBlock{
			Type: "text",
			Text: textBuf,
		})
	}
	if resp.StopReason == "" {
		resp.StopReason = "end_turn"
	}
	return resp, nil
}

// jsonGatewayErr signals a request-validation failure; the handler
// returns it as HTTP 400 + a JSON error envelope.
type jsonGatewayErr struct {
	Field   string
	Message string
}

func (e *jsonGatewayErr) Error() string {
	return e.Field + ": " + e.Message
}

// gatewayProviderErr wraps a provider-side failure surfaced via
// EventError. The handler maps it to HTTP 502 + a JSON error envelope
// (or an SSE error frame in the streaming path).
type gatewayProviderErr struct {
	Message string
}

func (e *gatewayProviderErr) Error() string {
	return e.Message
}

// itoa avoids strconv import drag — one tiny helper for slice-index
// error messages.
func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
