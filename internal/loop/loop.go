// Package loop runs the model→tool_use→tool_result→model cycle.
//
// One Run() call drives one agent run to completion. It calls the provider,
// streams events to the caller, dispatches tool_use to the dispatcher, sends
// tool_result back to the provider on the next iteration, and stops when the
// model signals end_turn (or hits MaxIterations).
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// PromptSegment mirrors the shape used by jobs-search-agent so the TS adapter
// is a 1:1 wrapper. A segment is a system or user message composed of typed
// content blocks (trusted-text or untrusted-block; both flatten to provider
// content blocks at request time).
type PromptSegment struct {
	Role    string               `json:"role"` // "system" | "user"
	Content []PromptContentBlock `json:"content"`
}

// PromptContentBlock is the typed content union the caller sends in.
//   - "trusted-text"     : text the loop trusts; goes through verbatim.
//   - "untrusted-block"  : text from an external source; wrapped in <untrusted>
//     tags before being sent to the model.
type PromptContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Cacheable bool   `json:"cacheable,omitempty"`
	Kind      string `json:"kind,omitempty"` // for untrusted-block: e.g. "web_content", "uploaded_cv"
}

// RunOptions is one Run() invocation.
type RunOptions struct {
	Provider      providers.Provider
	Model         string
	Tools         []tools.Tool
	Dispatcher    *tools.Dispatcher
	Segments      []PromptSegment
	OnEvent       func(providers.Event) // streaming hook (called from loop goroutine)
	MaxIterations int                   // safety cap; default 16
}

// RunResult is the terminal state after a Run.
type RunResult struct {
	StopReason string
	FinalText  string // concatenated text from the last assistant turn
	Iterations int
	Usage      providers.Usage // sum across iterations
}

// Run drives the agent loop to completion.
func Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	if opts.MaxIterations == 0 {
		opts.MaxIterations = 16
	}
	if opts.Provider == nil {
		return RunResult{}, fmt.Errorf("loop: provider is nil")
	}

	system, messages := splitSegments(opts.Segments)

	var toolSpecs []providers.ToolSpec
	if opts.Dispatcher != nil {
		toolSpecs = opts.Dispatcher.Specs(opts.Tools)
	}

	emit := func(ev providers.Event) {
		if opts.OnEvent != nil {
			opts.OnEvent(ev)
		}
	}

	emit(providers.Event{Type: providers.EventStarted})

	var totalUsage providers.Usage
	var finalText string
	var stopReason string

	for iter := 0; iter < opts.MaxIterations; iter++ {
		req := providers.Request{
			Model:    opts.Model,
			System:   system,
			Messages: messages,
			Tools:    toolSpecs,
		}
		ch, err := opts.Provider.Call(ctx, req)
		if err != nil {
			emit(providers.Event{Type: providers.EventError, Error: err.Error()})
			return RunResult{Iterations: iter}, err
		}

		// Collect this iteration: assistant text, any tool_use blocks, usage.
		var assistantBlocks []providers.ContentBlock
		var pendingTools []providers.ToolUse
		var iterText string
		var iterStop string
		var iterUsage *providers.Usage

		for ev := range ch {
			switch ev.Type {
			case providers.EventText:
				iterText += ev.Text
				emit(ev)
			case providers.EventToolCall:
				// Some providers (Ollama) don't issue tool_call IDs. Anthropic
				// and OpenAI both 400 if we replay an empty-ID tool_use in the
				// next turn's history, so we synthesise one here. The synth ID
				// is deterministic per (run, iter, slot) so a replay produces
				// the same value.
				tu := *ev.ToolUse
				if tu.ID == "" {
					tu.ID = fmt.Sprintf("lc-%d-%d", iter, len(pendingTools))
				}
				pendingTools = append(pendingTools, tu)
				assistantBlocks = append(assistantBlocks, providers.ContentBlock{
					Type:      "tool_use",
					ToolUseID: tu.ID,
					ToolName:  tu.Name,
					ToolInput: tu.Input,
				})
				emit(providers.Event{Type: providers.EventToolCall, ToolUse: &tu})
			case providers.EventDone:
				iterStop = ev.StopReason
				iterUsage = ev.Usage
			case providers.EventError:
				emit(ev)
				return RunResult{Iterations: iter}, fmt.Errorf("provider error: %s", ev.Error)
			}
		}

		// Prepend any text before tool_use blocks so the assistant turn is well-formed.
		if iterText != "" {
			assistantBlocks = append(
				[]providers.ContentBlock{{Type: "text", Text: iterText}},
				assistantBlocks...,
			)
		}
		messages = append(messages, providers.Message{Role: "assistant", Content: assistantBlocks})

		if iterUsage != nil {
			totalUsage.InputTokens += iterUsage.InputTokens
			totalUsage.OutputTokens += iterUsage.OutputTokens
			totalUsage.CacheCreationTokens += iterUsage.CacheCreationTokens
			totalUsage.CacheReadTokens += iterUsage.CacheReadTokens
			totalUsage.Model = iterUsage.Model
			emit(providers.Event{Type: providers.EventUsage, Usage: iterUsage})
		}

		stopReason = iterStop
		finalText = iterText

		// Terminal: model is done.
		if iterStop != "tool_use" || len(pendingTools) == 0 {
			break
		}

		// Execute pending tools and append a single user turn with all results.
		toolResults := make([]providers.ContentBlock, 0, len(pendingTools))
		for _, tu := range pendingTools {
			res := executeTool(ctx, opts.Dispatcher, tu)
			emit(providers.Event{
				Type:    providers.EventToolResult,
				ToolUse: &providers.ToolUse{ID: tu.ID, Name: tu.Name, Input: tu.Input},
				Text:    res.Text,
			})
			toolResults = append(toolResults, providers.ContentBlock{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Text:      res.Text,
				IsError:   res.IsError,
			})
		}
		messages = append(messages, providers.Message{Role: "user", Content: toolResults})
	}

	// If the for loop exited by exhausting MaxIterations while the model was
	// still mid-tool-use, the stop_reason will be stuck at "tool_use" but no
	// tools ran on this final iteration. Surface that distinctly to the
	// caller — they can decide whether to bump MaxIterations and retry, or
	// surface a different error to the user.
	if stopReason == "tool_use" {
		stopReason = "max_iterations"
	}

	emit(providers.Event{Type: providers.EventDone, StopReason: stopReason, Usage: &totalUsage})

	return RunResult{
		StopReason: stopReason,
		FinalText:  finalText,
		Iterations: iterationCount(messages),
		Usage:      totalUsage,
	}, nil
}

// executeTool runs one tool through the dispatcher; returns a marker error
// result if no dispatcher is wired up (defensive — Run() should reject earlier).
func executeTool(ctx context.Context, d *tools.Dispatcher, tu providers.ToolUse) tools.Result {
	if d == nil {
		return tools.Result{Text: "no tool dispatcher", IsError: true}
	}
	return d.Execute(ctx, tu.Name, tu.Input)
}

// splitSegments separates "system" segments (which become provider System
// blocks) from "user" segments (which become the first user Message).
func splitSegments(segs []PromptSegment) (system []providers.ContentBlock, messages []providers.Message) {
	var firstUser []providers.ContentBlock
	for _, s := range segs {
		switch s.Role {
		case "system":
			for _, c := range s.Content {
				system = append(system, flattenContent(c))
			}
		case "user":
			for _, c := range s.Content {
				firstUser = append(firstUser, flattenContent(c))
			}
		}
	}
	if len(firstUser) > 0 {
		messages = append(messages, providers.Message{Role: "user", Content: firstUser})
	}
	return
}

// allowedUntrustedKinds is the set of `kind` values an untrusted-block may
// declare. Anything else is normalised to "untrusted" so a caller can't
// inject a tag that the model treats as a trusted boundary (e.g. "system").
var allowedUntrustedKinds = map[string]bool{
	"untrusted":     true,
	"web_content":   true,
	"uploaded_cv":   true,
	"qa_question":   true,
	"user_input":    true,
	"tool_output":   true,
	"search_result": true,
}

// flattenContent converts the caller's typed content union into a provider
// ContentBlock. Untrusted blocks are wrapped in <kind>...</kind> tags so any
// embedded "instructions" lose force. Two protections:
//
//   - kind is validated against allowedUntrustedKinds; unknown values are
//     normalised to "untrusted" so a caller can't open a "system"- or
//     "trusted"-shaped tag.
//
//   - the body is escaped: every `<` becomes `&lt;`. Without this, content
//     containing `</web_content>` followed by attacker text and a re-opened
//     `<web_content>` would syntactically close our wrapping and present
//     the inner text to the model as if it were trusted.
func flattenContent(c PromptContentBlock) providers.ContentBlock {
	switch c.Type {
	case "untrusted-block":
		kind := c.Kind
		if kind == "" || !allowedUntrustedKinds[kind] {
			kind = "untrusted"
		}
		safe := strings.ReplaceAll(c.Text, "<", "&lt;")
		return providers.ContentBlock{
			Type: "text",
			Text: fmt.Sprintf("<%s>\n%s\n</%s>", kind, safe, kind),
		}
	default: // "trusted-text"
		return providers.ContentBlock{Type: "text", Text: c.Text, Cacheable: c.Cacheable}
	}
}

func iterationCount(messages []providers.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

var _ = json.Valid // keep encoding/json in deps for json.RawMessage docs above
