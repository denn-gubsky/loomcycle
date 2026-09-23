package ollama

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// `format` carries the schema itself on a self-hosted Ollama, and only on a
// tool-free request: it is a grammar over every token, so with tools the model
// could not call one. The hosted registration never sends it.
func TestOutputFormat_FormatOnlyOnAToolFreeSelfHostedRequest(t *testing.T) {
	format := &providers.OutputFormat{Name: "a", Schema: json.RawMessage(`{"type":"object","properties":{"k":{"type":"string"}}}`)}
	tool := []providers.ToolSpec{{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	for _, tc := range []struct {
		id    string
		tools []providers.ToolSpec
		want  bool
	}{
		{"ollama-local", nil, true},
		{"ollama-local", tool, false},
		{"ollama", nil, false},
	} {
		d := New(tc.id, "", "", streamhttp.Options{}, nil)
		b, err := d.buildRequestBody(providers.Request{Model: "qwen3:8b", Tools: tc.tools, OutputFormat: format,
			Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(b), `"format":{`); got != tc.want {
			t.Errorf("%s tools=%d: format on the wire = %v, want %v: %s", tc.id, len(tc.tools), got, tc.want, b)
		}
		if got := d.EnforcesStructuredOutput("qwen3:8b", len(tc.tools) > 0); got != tc.want {
			t.Errorf("%s tools=%d: EnforcesStructuredOutput = %v, want %v", tc.id, len(tc.tools), got, tc.want)
		}
	}
}
