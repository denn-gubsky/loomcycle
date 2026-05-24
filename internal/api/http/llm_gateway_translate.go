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

// usageFrameFromEvent translates an EventUsage into the gateway's
// message_delta frame shape. Returns nil for events that carry no
// usage payload (the driver shouldn't emit such events but defend
// anyway). Caller decides whether to send.
func usageFrameFromEvent(ev providers.Event) *llmStreamMessageDelta {
	if ev.Usage == nil {
		return nil
	}
	return &llmStreamMessageDelta{
		Delta: llmMessageDeltaPayload{StopReason: ev.StopReason},
		Usage: llmUsage{
			InputTokens:              ev.Usage.InputTokens,
			OutputTokens:             ev.Usage.OutputTokens,
			CacheCreationInputTokens: ev.Usage.CacheCreationTokens,
			CacheReadInputTokens:     ev.Usage.CacheReadTokens,
		},
	}
}

// toolUseBlockFromEvent extracts the tool_use content block payload
// from an EventToolCall. Returns nil for malformed events (ToolUse
// pointer unset). The caller wraps it in a content_block_start with
// the correct index — index management is exclusively the caller's
// responsibility so block-0-for-tool-use-only responses don't end up
// shifted to index 1.
func toolUseBlockFromEvent(ev providers.Event) *llmContentBlock {
	if ev.ToolUse == nil {
		return nil
	}
	return &llmContentBlock{
		Type:  "tool_use",
		ID:    ev.ToolUse.ID,
		Name:  ev.ToolUse.Name,
		Input: ev.ToolUse.Input,
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
