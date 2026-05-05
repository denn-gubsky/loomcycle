package localapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServerHandler captures the last request for assertion and returns
// a fixed response. status defaults to 200; pass non-zero to override.
type stubServerHandler struct {
	gotMethod  string
	gotPath    string
	gotBearer  string
	gotBody    []byte
	gotQuery   string
	respStatus int
	respBody   string
}

func (s *stubServerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.gotMethod = r.Method
	// Prefer the raw (still-encoded) path so tests can assert on the
	// wire-level escaping. Go decodes URL-encoded paths into URL.Path
	// for handler convenience; URL.RawPath preserves the original form
	// only when it differs from Path.
	if r.URL.RawPath != "" {
		s.gotPath = r.URL.RawPath
	} else {
		s.gotPath = r.URL.Path
	}
	s.gotQuery = r.URL.RawQuery
	if v := r.Header.Get("Authorization"); v != "" {
		s.gotBearer = v
	}
	s.gotBody, _ = io.ReadAll(r.Body)
	status := s.respStatus
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(s.respBody))
}

// End-to-end via a stub HTTP server: GET-by-path-param round-trip
// includes the path substitution + the Authorization header.
func TestEndpointTool_GetWithPathParam(t *testing.T) {
	stub := &stubServerHandler{respBody: `{"id":"abc","cv":"x"}`}
	ts := httptest.NewServer(stub)
	defer ts.Close()

	tool, _, _ := buildOneTool(t, minimalSpec, ts.URL, "get_application")
	res, err := tool.Execute(context.Background(),
		json.RawMessage(`{"bearer":"sk-test","id":"abc"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got IsError: %s", res.Text)
	}
	if stub.gotMethod != "GET" {
		t.Errorf("upstream method = %s", stub.gotMethod)
	}
	if stub.gotPath != "/api/applications/abc" {
		t.Errorf("upstream path = %q", stub.gotPath)
	}
	if stub.gotBearer != "Bearer sk-test" {
		t.Errorf("upstream Authorization = %q", stub.gotBearer)
	}
	if !strings.Contains(res.Text, `"cv":"x"`) {
		t.Errorf("response body lost: %q", res.Text)
	}
}

// PATCH with a body: input.body is JSON-marshalled and sent. Bearer
// goes into Authorization, NOT into the body.
func TestEndpointTool_PatchWithBody(t *testing.T) {
	stub := &stubServerHandler{respBody: `{"ok":true}`}
	ts := httptest.NewServer(stub)
	defer ts.Close()

	tool, _, _ := buildOneTool(t, minimalSpec, ts.URL, "patch_application")
	res, err := tool.Execute(context.Background(),
		json.RawMessage(`{"bearer":"tok","id":"app-1","body":{"cvText":"hello"}}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got IsError: %s", res.Text)
	}
	if stub.gotMethod != "PATCH" {
		t.Errorf("method = %s", stub.gotMethod)
	}
	if stub.gotPath != "/api/applications/app-1" {
		t.Errorf("path = %q", stub.gotPath)
	}
	gotBody := string(stub.gotBody)
	if !strings.Contains(gotBody, `"cvText":"hello"`) {
		t.Errorf("body not forwarded: %q", gotBody)
	}
	if strings.Contains(gotBody, "bearer") || strings.Contains(gotBody, "tok") {
		t.Errorf("bearer leaked into body: %q", gotBody)
	}
}

// Query parameters are URL-encoded and appended.
func TestEndpointTool_QueryParam(t *testing.T) {
	stub := &stubServerHandler{respBody: `[]`}
	ts := httptest.NewServer(stub)
	defer ts.Close()

	tool, _, _ := buildOneTool(t, minimalSpec, ts.URL, "list_research")
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"bearer":"x","date":"2026-05-05"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stub.gotPath != "/api/research" {
		t.Errorf("path = %q", stub.gotPath)
	}
	if !strings.Contains(stub.gotQuery, "date=2026-05-05") {
		t.Errorf("query missing date param: %q", stub.gotQuery)
	}
}

// Missing bearer is a recoverable error — surfaces as IsError so the
// model can read the message and try again. Critically, NO request
// goes upstream.
func TestEndpointTool_MissingBearer(t *testing.T) {
	stub := &stubServerHandler{}
	ts := httptest.NewServer(stub)
	defer ts.Close()

	tool, _, _ := buildOneTool(t, minimalSpec, ts.URL, "get_application")
	res, _ := tool.Execute(context.Background(),
		json.RawMessage(`{"id":"abc"}`))
	if !res.IsError || !strings.Contains(res.Text, "bearer") {
		t.Errorf("expected bearer-missing IsError, got %+v", res)
	}
	if stub.gotMethod != "" {
		t.Error("no upstream call should be made when bearer is missing")
	}
}

// Missing required path parameter is recoverable IsError; no upstream
// call (would result in malformed URL).
func TestEndpointTool_MissingRequiredPathParam(t *testing.T) {
	stub := &stubServerHandler{}
	ts := httptest.NewServer(stub)
	defer ts.Close()

	tool, _, _ := buildOneTool(t, minimalSpec, ts.URL, "get_application")
	res, _ := tool.Execute(context.Background(),
		json.RawMessage(`{"bearer":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "id") {
		t.Errorf("expected missing-id IsError, got %+v", res)
	}
	if stub.gotMethod != "" {
		t.Error("no upstream call should happen with missing path param")
	}
}

// Upstream 4xx/5xx is forwarded as IsError WITH the response body so
// the model can see the application's validation/error shape and
// self-correct.
func TestEndpointTool_Upstream4xxForwarded(t *testing.T) {
	stub := &stubServerHandler{respStatus: 422, respBody: `{"error":"missing field cvText"}`}
	ts := httptest.NewServer(stub)
	defer ts.Close()

	tool, _, _ := buildOneTool(t, minimalSpec, ts.URL, "patch_application")
	res, _ := tool.Execute(context.Background(),
		json.RawMessage(`{"bearer":"x","id":"a","body":{}}`))
	if !res.IsError {
		t.Error("expected IsError for upstream 4xx")
	}
	if !strings.Contains(res.Text, "422") || !strings.Contains(res.Text, "missing field cvText") {
		t.Errorf("upstream body not forwarded: %q", res.Text)
	}
}

// Path param with a slash gets percent-encoded so it stays inside the
// path segment (defends against path injection from a hostile param
// value, even though our agents are operator-trusted).
func TestEndpointTool_PathParamEscaping(t *testing.T) {
	stub := &stubServerHandler{respBody: "ok"}
	ts := httptest.NewServer(stub)
	defer ts.Close()

	tool, _, _ := buildOneTool(t, minimalSpec, ts.URL, "get_application")
	_, err := tool.Execute(context.Background(),
		json.RawMessage(`{"bearer":"x","id":"a/b/../etc/passwd"}`))
	if err != nil {
		t.Fatal(err)
	}
	// %2F encoding for the slash means the path segment stays intact.
	if !strings.Contains(stub.gotPath, "a%2Fb%2F..%2Fetc%2Fpasswd") {
		t.Errorf("path not escaped: %q", stub.gotPath)
	}
}

// Loader tests: the Build entry point produces the right number of
// tools, applies the tool name prefix, and propagates warnings.
func TestBuild_GeneratesToolsWithPrefix(t *testing.T) {
	path, _ := writeSpec(t, minimalSpec)
	tools, warns, err := Build(Config{
		SpecPath:       path,
		BaseURL:        "http://localhost:3000",
		ToolNamePrefix: "myapp",
	}, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
	want := map[string]bool{
		"myapp__get_application":   true,
		"myapp__patch_application": true,
		"myapp__list_research":     true,
	}
	for _, tl := range tools {
		if !want[tl.Name()] {
			t.Errorf("unexpected tool: %s", tl.Name())
		}
		delete(want, tl.Name())
	}
	if len(want) > 0 {
		t.Errorf("missing tools: %v", want)
	}
}

// Default prefix is "local" when ToolNamePrefix is empty.
func TestBuild_DefaultPrefix(t *testing.T) {
	path, _ := writeSpec(t, minimalSpec)
	tools, _, err := Build(Config{
		SpecPath: path,
		BaseURL:  "http://localhost",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if !strings.HasPrefix(tl.Name(), "local__") {
			t.Errorf("default prefix not applied: %q", tl.Name())
		}
	}
}

// Invalid prefix (slash, space, etc.) is rejected — the resulting tool
// name would be unaddressable from agent allow-lists.
func TestBuild_InvalidPrefixRejected(t *testing.T) {
	path, _ := writeSpec(t, minimalSpec)
	for _, bad := range []string{"my app", "my/app", "my::app", ""} {
		if bad == "" {
			continue // empty defaults to "local"; covered separately
		}
		_, _, err := Build(Config{
			SpecPath:       path,
			BaseURL:        "http://localhost",
			ToolNamePrefix: bad,
		}, "")
		if err == nil {
			t.Errorf("prefix %q should be rejected", bad)
		}
	}
}

// Build returns a fatal error when SpecPath or BaseURL is missing.
func TestBuild_RequiresSpecAndBaseURL(t *testing.T) {
	if _, _, err := Build(Config{BaseURL: "x"}, ""); err == nil {
		t.Error("expected error when SpecPath empty")
	}
	path, _ := writeSpec(t, minimalSpec)
	if _, _, err := Build(Config{SpecPath: path}, ""); err == nil {
		t.Error("expected error when BaseURL empty")
	}
}

// Input-schema sanity: the synthesised JSON Schema requires `bearer`
// plus all `required: true` parameters. The model sees a clear menu
// of fields with descriptions.
func TestEndpointTool_InputSchemaShape(t *testing.T) {
	tool, _, _ := buildOneTool(t, minimalSpec, "http://x", "patch_application")
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["bearer"]; !ok {
		t.Error("schema missing `bearer` field")
	}
	if _, ok := props["id"]; !ok {
		t.Error("schema missing path-param `id` field")
	}
	if _, ok := props["body"]; !ok {
		t.Error("schema missing `body` field")
	}
	required, _ := schema["required"].([]any)
	hasBearer, hasID := false, false
	for _, r := range required {
		if r == "bearer" {
			hasBearer = true
		}
		if r == "id" {
			hasID = true
		}
	}
	if !hasBearer || !hasID {
		t.Errorf("required missing bearer or id: %v", required)
	}
}

// Two operationIds that normalise to the same string MUST NOT
// silently collide in the dispatcher map. Without the collision
// guard, both tools register but only one dispatches (whichever
// the map iteration overwrote last) — the model sees both names in
// its menu but invocation is non-deterministic.
//
// EMPIRICAL: removing the `if firstOpID, dup := seen[toolName]; dup`
// branch from loader.go makes this test fail.
func TestBuild_DetectsCollisionAfterNormalisation(t *testing.T) {
	// Both '@' and '.' get normalised to '_' (only [A-Za-z0-9_-] survives).
	// `op@user` and `op.user` both become `op_user` after normalisation.
	body := `
openapi: 3.0.0
info: {title: x, version: x}
paths:
  /a:
    get:
      operationId: op@user
      summary: at flavour
  /b:
    get:
      operationId: op.user
      summary: dot flavour
`
	path, _ := writeSpec(t, body)
	out, warns, err := Build(Config{
		SpecPath:       path,
		BaseURL:        "http://x",
		ToolNamePrefix: "local",
	}, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Only ONE tool should survive the collision; the other becomes a
	// warning. Stable order: spec.Endpoints() sorts by (path, method),
	// so /a's op@user wins, /b's op.user is dropped.
	if len(out) != 1 {
		names := make([]string, len(out))
		for i, tl := range out {
			names[i] = tl.Name()
		}
		t.Fatalf("expected 1 tool after collision, got %d: %v", len(out), names)
	}
	if got := out[0].Name(); got != "local__op_user" {
		t.Errorf("survivor tool name = %q, want local__op_user", got)
	}
	collided := false
	for _, w := range warns {
		if strings.Contains(w, "collides") && strings.Contains(w, "op.user") {
			collided = true
		}
	}
	if !collided {
		t.Errorf("expected a collision warning naming op.user, got: %v", warns)
	}
}

// Helper: parse spec, build tools, return the named one. Aborts the
// test if the spec doesn't parse or the tool isn't found.
func buildOneTool(t *testing.T, body, baseURL, opID string) (*EndpointTool, []string, error) {
	t.Helper()
	path, _ := writeSpec(t, body)
	out, warns, err := Build(Config{
		SpecPath:       path,
		BaseURL:        baseURL,
		ToolNamePrefix: "local",
	}, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, tl := range out {
		et, ok := tl.(*EndpointTool)
		if !ok {
			continue
		}
		if et.ToolName == "local__"+opID {
			return et, warns, nil
		}
	}
	t.Fatalf("tool local__%s not found in %d generated tools", opID, len(out))
	return nil, nil, nil
}
