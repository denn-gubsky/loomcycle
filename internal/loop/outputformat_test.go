package loop

import (
	"context"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// answerProvider makes one tool call, then answers with `final`, recording
// every request. structured is its claim to enforce an output_format.
type answerProvider struct {
	mu         sync.Mutex
	final      string
	structured bool
	requests   []providers.Request
}

func (p *answerProvider) ID() string                                   { return "answer" }
func (p *answerProvider) Probe(context.Context) error                  { return nil }
func (p *answerProvider) ListModels(context.Context) ([]string, error) { return []string{"m"}, nil }
func (p *answerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsStructuredOutput: p.structured}
}
func (p *answerProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	turn := len(p.requests)
	p.requests = append(p.requests, req)
	ch := make(chan providers.Event, 4)
	if turn == 0 {
		tu := call("Echo")
		ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &tu}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	} else {
		ch <- providers.Event{Type: providers.EventText, Text: p.final}
		ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	}
	close(ch)
	return ch, nil
}

var answerFormat = &config.OutputFormat{Type: "json_schema", Name: "verdict",
	Schema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}

func runWithFormat(t *testing.T, prov *answerProvider, of *config.OutputFormat) (RunResult, []string) {
	t.Helper()
	echo := &echoTool{reply: "echoed"}
	var reports []string
	var mu sync.Mutex
	res, err := Run(context.Background(), RunOptions{
		Provider:     prov,
		Model:        "m",
		Tools:        []tools.Tool{echo},
		Dispatcher:   tools.NewDispatcher([]tools.Tool{echo}),
		Segments:     []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "go"}}}},
		OutputFormat: of,
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventCapabilityInert && ev.CapabilityInert.Gate == "output_format" {
				mu.Lock()
				reports = append(reports, ev.CapabilityInert.Message)
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, reports
}

// Unlike tool_choice, the schema never expires: every call carries it, because
// the model cannot know which turn is its last. The answer is parsed into
// Structured and the text is kept as it came.
func TestOutputFormat_EveryCallCarriesTheSchemaAndTheAnswerIsParsed(t *testing.T) {
	prov := &answerProvider{final: `{"ok":true}`, structured: true}
	res, reports := runWithFormat(t, prov, answerFormat)
	if len(prov.requests) != 2 {
		t.Fatalf("calls = %d, want 2", len(prov.requests))
	}
	for i, r := range prov.requests {
		if r.OutputFormat == nil || r.OutputFormat.Name != "verdict" || len(r.OutputFormat.Schema) == 0 {
			t.Errorf("call %d: OutputFormat = %+v, want the run's schema", i, r.OutputFormat)
		}
	}
	if res.Structured["ok"] != true || res.FinalText != `{"ok":true}` {
		t.Errorf("result = %q / %v", res.FinalText, res.Structured)
	}
	if len(reports) != 0 {
		t.Errorf("an enforced format was reported: %v", reports)
	}
}

// A target that cannot enforce the schema is sent none — a driver handed a
// field it does not map drops it silently or 400s — and the run says so ONCE,
// not per call. A JSON answer is still parsed (here, fenced the way a model
// asked in prose tends to answer).
func TestOutputFormat_AnUnenforcingTargetIsSentNoneAndReportedOnce(t *testing.T) {
	prov := &answerProvider{final: "```json\n{\"ok\":false}\n```", structured: false}
	res, reports := runWithFormat(t, prov, answerFormat)
	for i, r := range prov.requests {
		if r.OutputFormat != nil {
			t.Errorf("call %d carried a format the target cannot enforce", i)
		}
	}
	if len(reports) != 1 {
		t.Errorf("reports = %v, want exactly one", reports)
	}
	if res.Structured["ok"] != false {
		t.Errorf("a fenced JSON answer was not parsed: %v", res.Structured)
	}
}

// An answer that is not a JSON object leaves Structured nil and says why; the
// text is untouched in FinalText.
func TestOutputFormat_ANonObjectAnswerLeavesStructuredEmptyAndSaysSo(t *testing.T) {
	for _, final := range []string{"it looks fine", `["ok"]`} {
		prov := &answerProvider{final: final, structured: true}
		res, reports := runWithFormat(t, prov, answerFormat)
		if res.Structured != nil || res.FinalText != final {
			t.Errorf("%q: result = %q / %v", final, res.FinalText, res.Structured)
		}
		if len(reports) != 1 {
			t.Errorf("%q: reports = %v, want the parse report", final, reports)
		}
	}
}

// No format, nothing on the request and nothing parsed — a JSON-looking answer
// is not promoted into Structured unasked.
func TestOutputFormat_UnsetSendsNothingAndParsesNothing(t *testing.T) {
	prov := &answerProvider{final: `{"ok":true}`, structured: true}
	res, reports := runWithFormat(t, prov, nil)
	for _, r := range prov.requests {
		if r.OutputFormat != nil {
			t.Error("a format was sent unasked")
		}
	}
	if res.Structured != nil || len(reports) != 0 {
		t.Errorf("structured = %v, reports = %v", res.Structured, reports)
	}
}

// A stateful run's product is Σ, shaped by state_schema — said, not dropped.
func TestOutputFormat_StatefulRunReportsItIsNotApplied(t *testing.T) {
	prov := &statefulScriptProvider{scripts: []string{`{"patch":{"n":1},"done":true,"final":"ok"}`}}
	echo := &echoTool{reply: "observed"}
	var reported bool
	res, err := Run(context.Background(), RunOptions{
		Provider:     prov,
		Model:        "x",
		Tools:        []tools.Tool{echo},
		Dispatcher:   tools.NewDispatcher([]tools.Tool{echo}),
		Segments:     statefulTaskSegs(),
		Context:      statefulCtx(nil),
		OutputFormat: answerFormat,
		OnEvent: func(ev providers.Event) {
			if ev.Type == providers.EventCapabilityInert && ev.CapabilityInert.Gate == "output_format" {
				reported = true
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reported || res.Structured != nil {
		t.Errorf("reported = %v, structured = %v", reported, res.Structured)
	}
}

func TestStripJSONFence(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                  `{"a":1}`,
		"```json\n{\"a\":1}\n```":  `{"a":1}`,
		"```\n{\"a\":1}\n```":      `{"a":1}`,
		"``` {\"a\":1} ```":        `{"a":1}`,
		"  \n{\"a\":1}\n ":         `{"a":1}`,
		"prose ```json\n{}\n``` x": "prose ```json\n{}\n``` x",
	} {
		if got := stripJSONFence(in); got != want {
			t.Errorf("stripJSONFence(%q) = %q, want %q", in, got, want)
		}
	}
}
