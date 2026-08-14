package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	loomapi "github.com/denn-gubsky/loomcycle/api"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOpenAPISpec_ParsesAsYAML ensures the hand-authored spec is well-formed.
func TestOpenAPISpec_ParsesAsYAML(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
		t.Fatalf("api/openapi.yaml is not valid YAML: %v", err)
	}
	if doc["openapi"] == nil {
		t.Fatalf("api/openapi.yaml missing top-level 'openapi' version key")
	}
	if doc["paths"] == nil {
		t.Fatalf("api/openapi.yaml missing 'paths'")
	}
}

// TestOpenAPISpec_RendersAsJSON ensures the yaml→json conversion served at
// /v1/openapi.json succeeds and yields valid JSON.
func TestOpenAPISpec_RendersAsJSON(t *testing.T) {
	b, err := openapiSpecJSON()
	if err != nil {
		t.Fatalf("openapiSpecJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("rendered /v1/openapi.json is invalid JSON: %v", err)
	}
}

// TestOpenAPISpec_DocumentOpsMatchTool guards drift: the DocumentToolInput.op
// enum in the hand-authored spec must equal the Document tool's live op set. A
// new op added to the tool (or removed) without updating api/openapi.yaml fails
// here, so the published contract can't silently rot.
func TestOpenAPISpec_DocumentOpsMatchTool(t *testing.T) {
	spec := specOpEnum(t, "DocumentToolInput")
	tool := toolOpEnum(t, (&builtin.Document{}).InputSchema())
	assertSameOpSet(t, "Document", tool, spec)
}

// TestOpenAPISpec_PathOpsMatchTool guards the same drift for the Path tool.
func TestOpenAPISpec_PathOpsMatchTool(t *testing.T) {
	spec := specOpEnum(t, "PathToolInput")
	tool := toolOpEnum(t, (&builtin.Path{}).InputSchema())
	assertSameOpSet(t, "Path", tool, spec)
}

// TestOpenAPIHandlers_Serve exercises the HTTP serve path (the handlers read
// only the embedded spec, not Server state, so a zero-value Server suffices).
func TestOpenAPIHandlers_Serve(t *testing.T) {
	var s Server

	t.Run("yaml", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handleOpenAPIYAML(rec, httptest.NewRequest(http.MethodGet, "/v1/openapi.yaml", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
			t.Errorf("Content-Type = %q, want application/yaml", ct)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("served YAML does not parse: %v", err)
		}
	})

	t.Run("json", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handleOpenAPIJSON(rec, httptest.NewRequest(http.MethodGet, "/v1/openapi.json", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("served JSON does not parse: %v", err)
		}
		if doc["openapi"] == nil {
			t.Errorf("served JSON missing 'openapi' key")
		}
	})
}

// TestOpenAPIDocs_Serve exercises the /v1/docs Swagger UI console: the index
// page, the two vendored assets, and the whitelist (a non-listed name 404s).
func TestOpenAPIDocs_Serve(t *testing.T) {
	var s Server
	cases := []struct {
		path    string
		wantCT  string
		wantHit []byte
	}{
		{"/v1/docs", "text/html", []byte("swagger-ui")},
		{"/v1/docs/index.html", "text/html", []byte("SwaggerUIBundle")},
		{"/v1/docs/swagger-ui.css", "text/css", []byte(".swagger-ui")},
		{"/v1/docs/swagger-ui-bundle.js", "application/javascript", []byte("SwaggerUIBundle")},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		s.handleOpenAPIDocs(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", c.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, c.wantCT) {
			t.Errorf("%s: Content-Type = %q, want %s*", c.path, ct, c.wantCT)
		}
		if !strings.Contains(rec.Body.String(), string(c.wantHit)) {
			t.Errorf("%s: body missing marker %q", c.path, c.wantHit)
		}
	}

	// Whitelist: an unlisted / traversal-ish name must 404, never serve.
	for _, bad := range []string{"/v1/docs/PROVENANCE.md", "/v1/docs/../openapi.go", "/v1/docs/nope.js"} {
		rec := httptest.NewRecorder()
		s.handleOpenAPIDocs(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (whitelist)", bad, rec.Code)
		}
	}
}

// specOpEnum extracts components.schemas.<name>.properties.op.enum from the
// embedded spec.
func specOpEnum(t *testing.T, schemaName string) []string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	sch, _ := schemas[schemaName].(map[string]any)
	props, _ := sch["properties"].(map[string]any)
	op, _ := props["op"].(map[string]any)
	rawEnum, _ := op["enum"].([]any)
	if len(rawEnum) == 0 {
		t.Fatalf("spec schema %s has no properties.op.enum", schemaName)
	}
	out := make([]string, 0, len(rawEnum))
	for _, e := range rawEnum {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("non-string op value in spec %s: %v", schemaName, e)
		}
		out = append(out, s)
	}
	return out
}

// toolOpEnum extracts properties.op.enum from a tool's InputSchema() JSON.
func toolOpEnum(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var sch struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &sch); err != nil {
		t.Fatalf("parse tool InputSchema: %v", err)
	}
	if len(sch.Properties.Op.Enum) == 0 {
		t.Fatalf("tool InputSchema has no properties.op.enum")
	}
	return sch.Properties.Op.Enum
}

func assertSameOpSet(t *testing.T, name string, tool, spec []string) {
	t.Helper()
	toolSet := map[string]bool{}
	for _, s := range tool {
		toolSet[s] = true
	}
	specSet := map[string]bool{}
	for _, s := range spec {
		specSet[s] = true
	}
	for s := range toolSet {
		if !specSet[s] {
			t.Errorf("%s op %q exists in the tool but is MISSING from the OpenAPI spec — add it to %sToolInput.op enum in api/openapi.yaml", name, s, name)
		}
	}
	for s := range specSet {
		if !toolSet[s] {
			t.Errorf("%s op %q is in the OpenAPI spec but NOT in the tool — stale entry in api/openapi.yaml", name, s)
		}
	}
}
