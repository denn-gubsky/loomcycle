// An EXTERNAL test package: the drivers live in the internal/providers module,
// which cannot import this one, so the tests that send the real Agent schema
// through a driver live beside the schema.
package builtin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/anthropic"
	"github.com/denn-gubsky/loomcycle/internal/providers/gemini"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// agentWireBody runs one Call offering only the Agent tool against a stub
// server and returns the request body the driver sent.
func agentWireBody(t *testing.T, newDriver func(baseURL string) providers.Provider, model, sse string) []byte {
	t.Helper()
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case captured <- body:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sse)
	}))
	defer srv.Close()

	agent := &builtin.AgentTool{}
	ch, err := newDriver(srv.URL).Call(context.Background(), providers.Request{
		Model:    model,
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:    []providers.ToolSpec{{Name: agent.Name(), Description: agent.Description(), InputSchema: agent.InputSchema()}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for range ch {
	}
	select {
	case body := <-captured:
		return body
	default:
		t.Fatal("the driver sent no request")
		return nil
	}
}

// opsAndRequired reads the op enum and the top-level required list of a tool
// schema.
func opsAndRequired(t *testing.T, schema []byte) (ops string, required []string) {
	t.Helper()
	var s struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatal(err)
	}
	return strings.Join(s.Properties.Op.Enum, ","), s.Required
}

// What a Claude model is told about the Agent tool: every operation, and no
// field required on every call. When Agent's schema was a top-level oneOf,
// this sanitizer flattened it to `op: ["spawn"]` with name, prompt, op, spawns
// and child_run_id all required.
func TestAgentSchema_ReachesClaudeWithEveryOperation(t *testing.T) {
	in := (&builtin.AgentTool{}).InputSchema()
	body := agentWireBody(t, func(baseURL string) providers.Provider {
		return anthropic.New("test-key", baseURL, streamhttp.Options{}, nil)
	}, "claude-sonnet-4-6", "event: message_stop\ndata: {}\n\n")
	var wire struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Name != "Agent" {
		t.Fatalf("tools on the wire = %s", body)
	}
	out := wire.Tools[0].InputSchema
	// An unchanged schema reaches the wire as json.Marshal writes a
	// json.RawMessage: compacted, with <, > and & escaped. Anything else is a
	// rewrite by the sanitizer.
	var compact, unchanged bytes.Buffer
	if err := json.Compact(&compact, in); err != nil {
		t.Fatal(err)
	}
	json.HTMLEscape(&unchanged, compact.Bytes())
	if string(out) != unchanged.String() {
		t.Errorf("the sanitizer rewrote Agent's schema; it should pass a flat schema through unchanged")
	}
	ops, required := opsAndRequired(t, out)
	if ops != "spawn,parallel_spawn,open,send,poll,cancel,close" {
		t.Errorf("op enum sent to Claude = %q", ops)
	}
	if len(required) != 0 {
		t.Errorf("required sent to Claude = %v; each op's fields are required only for that op", required)
	}
}

// What a Gemini model is told about the Agent tool: every operation, and no
// field required on every call. When Agent's schema was a top-level oneOf,
// this sanitizer flattened it to `op: ["spawn"]` with name, prompt, op, spawns
// and child_run_id all required.
func TestAgentSchema_ReachesGeminiWithEveryOperation(t *testing.T) {
	// Gemini's sanitizer always rewrites (it drops keywords Gemini rejects), so
	// what matters is what survives: the ops and the required list.
	body := agentWireBody(t, func(baseURL string) providers.Provider {
		return gemini.New("test-key", baseURL, streamhttp.Options{}, nil)
	}, "gemini-2.5-flash", `data: {"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`+"\n\n")
	var wire struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if len(wire.Tools) != 1 || len(wire.Tools[0].FunctionDeclarations) != 1 || wire.Tools[0].FunctionDeclarations[0].Name != "Agent" {
		t.Fatalf("tools on the wire = %s", body)
	}
	ops, required := opsAndRequired(t, wire.Tools[0].FunctionDeclarations[0].Parameters)
	if ops != "spawn,parallel_spawn,open,send,poll,cancel,close" {
		t.Errorf("op enum sent to Gemini = %q", ops)
	}
	if len(required) != 0 {
		t.Errorf("required sent to Gemini = %v; each op's fields are required only for that op", required)
	}
}
