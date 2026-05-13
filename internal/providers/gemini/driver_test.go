package gemini

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
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// fakeStream serves a canned SSE script as one streamGenerateContent
// response. Asserts the x-goog-api-key header lands and that the
// model name is in the URL path (Gemini reads it from there, not
// from the body).
func fakeStream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("x-goog-api-key = %q, want test-key", got)
		}
		// URL shape: /v1beta/models/{model}:streamGenerateContent
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("path = %q, want suffix :streamGenerateContent", r.URL.Path)
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("query alt = %q, want sse", r.URL.Query().Get("alt"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, f := range frames {
			fmt.Fprint(w, f)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
}

func TestStreamTextThenStop(t *testing.T) {
	frames := []string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hello "}],"role":"model"}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"parts":[{"text":"world"}],"role":"model"}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"parts":[],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":42,"candidatesTokenCount":7,"totalTokenCount":49},"modelVersion":"gemini-2.0-flash-001"}` + "\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "gemini-2.0-flash",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var text strings.Builder
	var done providers.Event
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			text.WriteString(ev.Text)
		case providers.EventDone:
			done = ev
		case providers.EventError:
			t.Fatalf("unexpected error: %s", ev.Error)
		}
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q, want hello world", text.String())
	}
	if done.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn (mapped from STOP)", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 42 || done.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", done.Usage)
	}
	// modelVersion from chunk envelope MUST land on Usage.Model so
	// downstream cost rollups can attribute. Same regression class
	// as the v0.6.0 OpenAI driver fix.
	if done.Usage.Model != "gemini-2.0-flash-001" {
		t.Errorf("Usage.Model = %q, want gemini-2.0-flash-001 (modelVersion from chunk)", done.Usage.Model)
	}
}

func TestStreamFunctionCall(t *testing.T) {
	frames := []string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Berlin"}}}],"role":"model"}}]}` + "\n\n",
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":10}}` + "\n\n",
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	ch, _ := d.Call(context.Background(), providers.Request{
		Model: "gemini-2.0-flash",
		Tools: []providers.ToolSpec{
			{Name: "get_weather", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "weather"}}}},
	})

	var toolCall *providers.ToolUse
	var stop string
	for ev := range ch {
		if ev.Type == providers.EventToolCall {
			toolCall = ev.ToolUse
		}
		if ev.Type == providers.EventDone {
			stop = ev.StopReason
		}
	}
	if toolCall == nil {
		t.Fatal("no tool_call emitted")
	}
	if toolCall.Name != "get_weather" {
		t.Errorf("Name = %q, want get_weather", toolCall.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(toolCall.Input, &args); err != nil {
		t.Fatalf("Input not JSON: %v (raw %s)", err, toolCall.Input)
	}
	if args["city"] != "Berlin" {
		t.Errorf("args = %v, want city=Berlin", args)
	}
	// finishReason was STOP, but pending function calls force the
	// stop_reason remap to tool_use so the loop iterates.
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (function call must override STOP)", stop)
	}
}

// TestRequestShape pins the wire body against Gemini's expected
// schema: contents (with role="model" not "assistant"),
// systemInstruction, tools.functionDeclarations, generationConfig.
// The model name must NOT appear in the body — Gemini reads it from
// the URL path. A regression that smuggled the model into the body
// would make the request 4xx on every Vertex AI gateway.
func TestRequestShape(t *testing.T) {
	captured := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, `data: {"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	temp := 0.5
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:       "gemini-2.5-flash",
		Temperature: &temp,
		MaxTokens:   8192,
		Effort:      "high",
		System:      []providers.ContentBlock{{Type: "text", Text: "you are concise"}},
		Tools: []providers.ToolSpec{
			{Name: "calc", Description: "do math", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "hello"}}},
		},
	})
	for range ch {
	}

	body := <-captured
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("body not JSON: %v (raw %s)", err, body)
	}
	// No "model" field in the body.
	if bytes.Contains(body, []byte(`"model":`)) {
		t.Errorf("body carries a model field — Gemini expects model in the URL only:\n%s", body)
	}
	// systemInstruction populated.
	if w.SystemInstruction == nil || len(w.SystemInstruction.Parts) == 0 || w.SystemInstruction.Parts[0].Text != "you are concise" {
		t.Errorf("systemInstruction = %+v, want one text part", w.SystemInstruction)
	}
	// Roles translated: assistant → model.
	if len(w.Contents) != 2 {
		t.Fatalf("contents = %d entries, want 2", len(w.Contents))
	}
	if w.Contents[0].Role != "user" || w.Contents[1].Role != "model" {
		t.Errorf("roles = [%q, %q], want [user, model] (assistant → model translation)", w.Contents[0].Role, w.Contents[1].Role)
	}
	// Tools wrapped in functionDeclarations.
	if len(w.Tools) != 1 || len(w.Tools[0].FunctionDeclarations) != 1 || w.Tools[0].FunctionDeclarations[0].Name != "calc" {
		t.Errorf("tools = %+v, want one functionDeclaration name=calc", w.Tools)
	}
	// generationConfig with thinkingBudget=8192-1024=7168 (effort=high
	// clamped to maxTokens-1024 since 8192 == maxTokens).
	if w.GenerationConfig == nil {
		t.Fatal("generationConfig missing")
	}
	if w.GenerationConfig.ThinkingConfig == nil {
		t.Fatal("thinkingConfig missing for effort=high")
	}
	if w.GenerationConfig.ThinkingConfig.ThinkingBudget != 7168 {
		t.Errorf("thinkingBudget = %d, want 7168 (clamped from 8192 against maxTokens=8192)",
			w.GenerationConfig.ThinkingConfig.ThinkingBudget)
	}
}

// TestEffortBudget pins the effort → thinkingBudget translation.
// Mirrors the same effort vocabulary the loop uses for Anthropic so
// operator semantics stay consistent across providers.
func TestEffortBudget(t *testing.T) {
	cases := []struct {
		effort    string
		maxTokens int
		want      int
	}{
		{"", 0, -1},
		{"low", 0, 0},
		{"medium", 0, 2048},
		{"high", 0, 8192},
		{"high", 16384, 8192},
		{"high", 8192, 7168}, // clamps to maxTokens-1024
		{"high", 1500, 0},    // would clamp below 1024 → drop
		{"medium", 4096, 2048},
		{"medium", 1024, 768}, // clamps to maxTokens-256
		{"medium", 256, 0},    // budget would be 0 after clamp
		{"unknown", 0, -1},
	}
	for _, tc := range cases {
		got := geminiEffortBudget(tc.effort, tc.maxTokens)
		if got != tc.want {
			t.Errorf("geminiEffortBudget(%q, %d) = %d, want %d", tc.effort, tc.maxTokens, got, tc.want)
		}
	}
}

// TestFunctionResponseRoundtrip pins that a tool_result content
// block translates to a functionResponse part with the loomcycle
// text wrapped under {"content": "..."}. Required by Gemini's
// schema — bare strings aren't accepted in functionResponse.response.
func TestFunctionResponseRoundtrip(t *testing.T) {
	body, err := buildRequestBody(providers.Request{
		Model: "gemini-2.0-flash",
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "do it"}}},
			{Role: "assistant", Content: []providers.ContentBlock{
				{Type: "tool_use", ToolName: "get_weather", ToolUseID: "x", ToolInput: json.RawMessage(`{"city":"Berlin"}`)},
			}},
			{Role: "user", Content: []providers.ContentBlock{
				{Type: "tool_result", ToolName: "get_weather", ToolUseID: "x", Text: "sunny, 22°C"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(w.Contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(w.Contents))
	}
	// 3rd message: user role (loomcycle role) carrying a
	// functionResponse part. Gemini interprets it correctly because
	// of the part shape; role stays "user" on the wire.
	last := w.Contents[2]
	if len(last.Parts) != 1 || last.Parts[0].FunctionResponse == nil {
		t.Fatalf("third content lacks functionResponse: %+v", last)
	}
	fr := last.Parts[0].FunctionResponse
	if fr.Name != "get_weather" {
		t.Errorf("functionResponse.name = %q, want get_weather", fr.Name)
	}
	var resp map[string]string
	if err := json.Unmarshal(fr.Response, &resp); err != nil {
		t.Fatalf("functionResponse.response not JSON: %v (raw %s)", err, fr.Response)
	}
	if resp["content"] != "sunny, 22°C" {
		t.Errorf("functionResponse.response.content = %q, want sunny, 22°C", resp["content"])
	}
}

// --- probe / list models ---

func TestListModels_StripsModelsPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("x-goog-api-key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{"name":"models/gemini-2.5-flash"},
				{"name":"models/gemini-2.0-flash"},
				{"name":"models/gemini-1.5-pro-001"}
			]
		}`))
	}))
	defer srv.Close()

	d := New("test-key", srv.URL, streamhttp.Options{}, nil)
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"gemini-2.5-flash", "gemini-2.0-flash", "gemini-1.5-pro-001"}
	if len(models) != len(want) {
		t.Fatalf("got %d models, want %d (got %v)", len(models), len(want), models)
	}
	for i, m := range models {
		if m != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, m, want[i])
		}
	}
}

func TestProbe_PropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"API key invalid"}}`))
	}))
	defer srv.Close()

	d := New("bad-key", srv.URL, streamhttp.Options{}, nil)
	if err := d.Probe(context.Background()); err == nil {
		t.Fatal("Probe with 401 didn't surface an error")
	}
}

// TestSanitizeGeminiSchema_StripsAdditionalProperties pins the v0.8.4
// follow-up fix: Gemini's function_declarations.parameters rejects
// JSON Schema's additionalProperties (and $schema / $id). The Channel
// + Memory built-in tools both ship schemas with these fields, which
// every other driver (Anthropic / OpenAI / DeepSeek / Ollama)
// accepts silently. Without the sanitizer, every tool call against
// Gemini returns 400 INVALID_ARGUMENT.
func TestSanitizeGeminiSchema_StripsAdditionalProperties(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{
			"op":{"type":"string","enum":["get","set"]},
			"value":{"type":"object","additionalProperties":false}
		},
		"required":["op"],
		"additionalProperties":false,
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"$id":"channel-input"
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)

	// All three offending fields must be stripped from both the
	// root schema AND any nested object schema. Without recursion,
	// the inner "value.additionalProperties" would survive and the
	// API still rejects.
	if strings.Contains(gotStr, "additionalProperties") {
		t.Errorf("additionalProperties not stripped: %s", gotStr)
	}
	if strings.Contains(gotStr, "$schema") {
		t.Errorf("$schema not stripped: %s", gotStr)
	}
	if strings.Contains(gotStr, "$id") {
		t.Errorf("$id not stripped: %s", gotStr)
	}
	// Allowed fields must survive.
	if !strings.Contains(gotStr, `"properties"`) {
		t.Errorf("properties missing from sanitized output: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"required"`) {
		t.Errorf("required missing from sanitized output: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"enum"`) {
		t.Errorf("enum missing from sanitized output: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_PreservesOnMalformedInput pins the best-
// effort contract: if the schema is somehow malformed JSON (shouldn't
// happen at runtime since the loop builds it from validated tool
// specs), the sanitizer returns the input verbatim so Gemini surfaces
// a clear 400 rather than the driver silently swallowing the problem.
func TestSanitizeGeminiSchema_PreservesOnMalformedInput(t *testing.T) {
	in := json.RawMessage(`not json`)
	got := sanitizeGeminiSchema(in)
	if string(got) != "not json" {
		t.Errorf("malformed input not preserved: got %q", string(got))
	}
}

// TestSanitizeGeminiSchema_EmptyInputPassthrough — zero-length
// schemas (rare; would mean a tool with no parameters) pass through
// unchanged.
func TestSanitizeGeminiSchema_EmptyInputPassthrough(t *testing.T) {
	in := json.RawMessage(``)
	got := sanitizeGeminiSchema(in)
	if len(got) != 0 {
		t.Errorf("empty input not preserved: got %d bytes", len(got))
	}
}

// TestSanitizeGeminiSchema_InlinesTopLevelDefs — the canonical
// shape zod-to-json-schema produces: a top-level `$defs` map
// keyed by type name, plus inline `$ref` pointers into it. Gemini
// rejects `$ref`; we must inline.
func TestSanitizeGeminiSchema_InlinesTopLevelDefs(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{
			"primary":{"$ref":"#/$defs/Address"},
			"billing":{"$ref":"#/$defs/Address"}
		},
		"$defs":{
			"Address":{
				"type":"object",
				"properties":{
					"street":{"type":"string"},
					"city":{"type":"string"}
				},
				"required":["street","city"]
			}
		}
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)

	if strings.Contains(gotStr, "$ref") {
		t.Errorf("$ref not inlined: %s", gotStr)
	}
	if strings.Contains(gotStr, "$defs") {
		t.Errorf("$defs not stripped from output: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"street"`) {
		t.Errorf("inlined definition missing `street` property: %s", gotStr)
	}
	// Both refs should be independently inlined (deep-copy, not
	// shared aliases — subsequent edits to one shouldn't affect
	// the other). Count the property definition (the type-info
	// shape) rather than the bare key — `street` also appears in
	// each variant's `required` slice.
	if c := strings.Count(gotStr, `"street":{"type":"string"}`); c != 2 {
		t.Errorf("expected inlined `street` property definition twice (once per ref site); got %d: %s", c, gotStr)
	}
}

// TestSanitizeGeminiSchema_InlinesDefinitionsAlias — older
// JSON-Schema drafts use `definitions` rather than `$defs`.
// Both must be supported (some MCP servers emit `definitions`,
// others emit `$defs`).
func TestSanitizeGeminiSchema_InlinesDefinitionsAlias(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{"x":{"$ref":"#/definitions/Inner"}},
		"definitions":{
			"Inner":{"type":"object","properties":{"v":{"type":"string"}}}
		}
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)
	if strings.Contains(gotStr, "$ref") || strings.Contains(gotStr, "definitions") {
		t.Errorf("definitions-style $ref not inlined: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"v"`) {
		t.Errorf("inlined `v` property missing: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_CycleSafe — direct cycles produce
// `{}` rather than infinite recursion. (Indirect / mutual
// recursion is rare in practice but uses the same visited-set
// guard.)
func TestSanitizeGeminiSchema_CycleSafe(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{"node":{"$ref":"#/$defs/Tree"}},
		"$defs":{
			"Tree":{
				"type":"object",
				"properties":{
					"value":{"type":"string"},
					"child":{"$ref":"#/$defs/Tree"}
				}
			}
		}
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)
	if strings.Contains(gotStr, "$ref") {
		t.Errorf("cycle not broken — $ref still present: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"value"`) {
		t.Errorf("Tree.value missing — cycle handling shouldn't drop the first inline: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_UnresolvedRefEmitsEmpty — a `$ref`
// pointing at a missing definition emits `{}` rather than
// preserving the ref string (which would fail Gemini's parser).
func TestSanitizeGeminiSchema_UnresolvedRefEmitsEmpty(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{"x":{"$ref":"#/$defs/Missing"}}
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)
	if strings.Contains(gotStr, "$ref") {
		t.Errorf("unresolved $ref leaked into output: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_CollapseOneOf — `oneOf` collapses to
// a UNION of all variants' properties + required. Each variant's
// payload shape survives; the discriminator field's per-variant
// `enum` collapses to bare `type: string` (Gemini's OpenAPI
// subset can't represent a discriminator anyway). This is the
// failure mode the v0.8.x sanitizer fixes — every Zod
// `z.discriminatedUnion` was previously losing every variant
// past the first.
func TestSanitizeGeminiSchema_CollapseOneOf(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{
				"oneOf":[
					{"type":"object","properties":{"kind":{"type":"string","enum":["create"]},"name":{"type":"string"}},"required":["kind","name"]},
					{"type":"object","properties":{"kind":{"type":"string","enum":["delete"]},"id":{"type":"string"}},"required":["kind","id"]}
				]
			}
		}
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)
	if strings.Contains(gotStr, "oneOf") {
		t.Errorf("oneOf not collapsed: %s", gotStr)
	}
	// BOTH variants' payload properties survive — this is the
	// load-bearing assertion for the fix.
	if !strings.Contains(gotStr, `"name"`) {
		t.Errorf("first variant `name` property missing: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"id"`) {
		t.Errorf("second variant `id` property missing — discriminated union still lossy: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_CollapseAnyOf — same merge semantics
// as oneOf.
func TestSanitizeGeminiSchema_CollapseAnyOf(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{
			"u":{
				"anyOf":[
					{"type":"object","properties":{"a":{"type":"string"}}},
					{"type":"object","properties":{"b":{"type":"number"}}}
				]
			}
		}
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)
	if strings.Contains(gotStr, "anyOf") {
		t.Errorf("anyOf not collapsed: %s", gotStr)
	}
	// Both variants merge.
	if !strings.Contains(gotStr, `"a"`) || !strings.Contains(gotStr, `"b"`) {
		t.Errorf("anyOf merge dropped a variant: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_TypeConflictDefense — if combinator
// variants declare different `type` values (e.g. `array` vs
// `object`), `mergeGeminiSchemaInto` must NOT fold the conflicting
// variant's structural fields (properties / items / required) into
// the parent. The alternative produces a schema with conflicting
// `type` + `properties` + `items` that's malformed and would 400.
// dst's type wins; src's structural fields drop.
func TestSanitizeGeminiSchema_TypeConflictDefense(t *testing.T) {
	in := json.RawMessage(`{
		"oneOf":[
			{"type":"object","properties":{"obj_field":{"type":"string"}},"required":["obj_field"]},
			{"type":"array","items":{"type":"string"}}
		]
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)

	// First (object) variant wins on type + brings its properties.
	if !strings.Contains(gotStr, `"obj_field"`) {
		t.Errorf("first variant properties dropped: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"type":"object"`) {
		t.Errorf("first variant type lost: %s", gotStr)
	}
	// Second (array) variant's structural keys must NOT leak through —
	// a schema with `type: object` + `items: {...}` is malformed.
	if strings.Contains(gotStr, `"items"`) {
		t.Errorf("conflicting `items` field leaked across type boundary: %s", gotStr)
	}
	// And no oneOf residue.
	if strings.Contains(gotStr, "oneOf") {
		t.Errorf("oneOf not collapsed: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_MergeAllOf — `allOf` merges every
// variant's properties + required into the parent (intersection
// semantic — the value must satisfy all variants).
func TestSanitizeGeminiSchema_MergeAllOf(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"allOf":[
			{"properties":{"a":{"type":"string"}},"required":["a"]},
			{"properties":{"b":{"type":"number"}},"required":["b"]}
		]
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)
	if strings.Contains(gotStr, "allOf") {
		t.Errorf("allOf not merged: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"a"`) || !strings.Contains(gotStr, `"b"`) {
		t.Errorf("merged properties missing a or b: %s", gotStr)
	}
	// Both `a` and `b` should end up in required.
	if !strings.Contains(gotStr, `"a"`) || !strings.Contains(gotStr, `"b"`) {
		t.Errorf("merged required missing: %s", gotStr)
	}
}

// TestSanitizeGeminiSchema_RealisticMcpSchema — regression
// fixture mirroring a real jobs-search-agent MCP tool input
// schema (Zod's discriminatedUnion → JSON-Schema oneOf+$ref;
// nested objects with their own $defs; additionalProperties at
// multiple levels). Without the v0.8.x sanitizer, Gemini's
// function-calling API rejects this with `400 INVALID_ARGUMENT`
// at deeply nested paths
// (tools[0].function_declarations[*].parameters.properties[*].value...).
// The post-sanitize output must contain NO `$ref` / `$defs` /
// `oneOf` / `additionalProperties` and remain valid JSON.
func TestSanitizeGeminiSchema_RealisticMcpSchema(t *testing.T) {
	in := json.RawMessage(`{
		"$schema":"http://json-schema.org/draft-07/schema#",
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"applicationId":{"type":"string"},
			"patch":{
				"type":"object",
				"additionalProperties":false,
				"properties":{
					"status":{"$ref":"#/$defs/Status"},
					"answers":{
						"type":"array",
						"items":{"$ref":"#/$defs/Answer"}
					},
					"feedback":{
						"oneOf":[
							{"$ref":"#/$defs/PositiveFeedback"},
							{"$ref":"#/$defs/NegativeFeedback"}
						]
					}
				}
			}
		},
		"required":["applicationId","patch"],
		"$defs":{
			"Status":{
				"type":"string",
				"enum":["draft","submitted","won","lost"]
			},
			"Answer":{
				"type":"object",
				"additionalProperties":false,
				"properties":{
					"questionId":{"type":"string"},
					"value":{"type":"string"}
				},
				"required":["questionId","value"]
			},
			"PositiveFeedback":{
				"type":"object",
				"additionalProperties":false,
				"properties":{
					"kind":{"type":"string","enum":["positive"]},
					"highlights":{"type":"array","items":{"type":"string"}}
				},
				"required":["kind"]
			},
			"NegativeFeedback":{
				"type":"object",
				"additionalProperties":false,
				"properties":{
					"kind":{"type":"string","enum":["negative"]},
					"concerns":{"type":"array","items":{"type":"string"}}
				},
				"required":["kind"]
			}
		}
	}`)
	got := sanitizeGeminiSchema(in)
	gotStr := string(got)

	for _, banned := range []string{"$ref", "$defs", "definitions", "oneOf", "anyOf", "allOf", "additionalProperties", "$schema", "$id"} {
		if strings.Contains(gotStr, banned) {
			t.Errorf("banned key %q leaked into Gemini-bound schema: %s", banned, gotStr)
		}
	}
	// Output must still be valid JSON.
	var roundtrip any
	if err := json.Unmarshal(got, &roundtrip); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw: %s", err, gotStr)
	}
	// And it must still describe the surface — `applicationId`
	// is the load-bearing field for the tool call to make sense.
	if !strings.Contains(gotStr, `"applicationId"`) {
		t.Errorf("applicationId lost from sanitized schema: %s", gotStr)
	}
	// Both discriminated-union variants merge: PositiveFeedback's
	// `highlights` AND NegativeFeedback's `concerns` survive.
	// (Pre-fix this test would have caught the first-variant-wins
	// regression — second variant's payload was dropped entirely.)
	if !strings.Contains(gotStr, `"highlights"`) {
		t.Errorf("PositiveFeedback.highlights missing — first oneOf variant dropped: %s", gotStr)
	}
	if !strings.Contains(gotStr, `"concerns"`) {
		t.Errorf("NegativeFeedback.concerns missing — second oneOf variant dropped (discriminated-union footgun): %s", gotStr)
	}
}
