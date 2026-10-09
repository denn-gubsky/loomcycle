package http

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	loomapi "github.com/denn-gubsky/loomcycle/api"
	"github.com/denn-gubsky/loomcycle/internal/connector"
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

// TestOpenAPISpec_DecisionRequestMatchesTool guards drift between the
// POST /v1/_decide request in the spec and the Decision tool's own input
// schema, which is what the route forwards the body to: the top-level fields
// and which are required, each question's fields, and the question types. The
// spec models a question as one schema per type (the shape of criteria differs
// by type), so every variant must carry the tool's fields and together they
// must name exactly the tool's types.
func TestOpenAPISpec_DecisionRequestMatchesTool(t *testing.T) {
	var tool struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal((&builtin.Decision{}).InputSchema(), &tool); err != nil {
		t.Fatalf("parse the Decision tool's InputSchema: %v", err)
	}
	var questions struct {
		AdditionalProperties struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"additionalProperties"`
	}
	if err := json.Unmarshal(tool.Properties["questions"], &questions); err != nil {
		t.Fatalf("parse the tool's questions schema: %v", err)
	}
	toolQuestion := questions.AdditionalProperties
	toolTypes := toolQuestion.Properties["type"].Enum
	if len(tool.Properties) == 0 || len(toolQuestion.Properties) == 0 || len(toolTypes) == 0 {
		t.Fatalf("the tool's InputSchema no longer has the shape this test reads: %s", (&builtin.Decision{}).InputSchema())
	}

	schemas := specSchemas(t)
	req := schemas["DecisionRequest"]
	assertSameSet(t, "DecisionRequest properties", specKeys(tool.Properties), specKeys(specMap(req["properties"])))
	assertSameSet(t, "DecisionRequest required", tool.Required, specStrings(req["required"]))

	var specTypes []string
	for _, ref := range specSlice(schemas["DecisionQuestion"]["oneOf"]) {
		name := strings.TrimPrefix(specString(specMap(ref)["$ref"]), "#/components/schemas/")
		variant := schemas[name]
		if variant == nil {
			t.Fatalf("DecisionQuestion.oneOf names %q, which is not a schema", name)
		}
		props := specMap(variant["properties"])
		assertSameSet(t, name+" properties", specKeys(toolQuestion.Properties), specKeys(props))
		for _, field := range toolQuestion.Required {
			if !containsStr(specStrings(variant["required"]), field) {
				t.Errorf("%s does not require %q, which the tool requires of every question", name, field)
			}
		}
		specTypes = append(specTypes, specStrings(specMap(props["type"])["enum"])...)
	}
	assertSameSet(t, "question types (DecisionQuestion variants)", toolTypes, specTypes)
	assertSameSet(t, "answer types (DecisionAnswer.type)", toolTypes,
		specStrings(specMap(specMap(schemas["DecisionAnswer"]["properties"])["type"])["enum"]))
}

// TestOpenAPISpec_DecisionFailureCodesSitUnderTheirStatus guards drift between
// the handler's status mapping and the spec: every failure code a decision
// call can carry must have an example, under the status decideFailureStatus
// gives that code. The codes are read from the source that declares them, so a
// new one fails here until the contract documents it.
func TestOpenAPISpec_DecisionFailureCodesSitUnderTheirStatus(t *testing.T) {
	codes := append(
		stringConsts(t, "../../decisionq/decisionq.go", "Code"),
		stringConsts(t, "../../tools/builtin/decision.go", "DecisionCode")...)
	// A floor on what was read, so a rename of the constants cannot leave this
	// test checking nothing.
	if len(codes) < 10 {
		t.Fatalf("read only %d decision failure codes from the source (%v); the constants moved", len(codes), codes)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	responses := specMap(specMap(specMap(specMap(doc["paths"])["/v1/_decide"])["post"])["responses"])
	if len(responses) == 0 {
		t.Fatal("the spec has no POST /v1/_decide responses")
	}
	for _, code := range codes {
		// The handler prepares every call it dispatches (who is charged), so the
		// tool's refusal of an unprepared one never reaches an HTTP caller.
		if code == builtin.DecisionCodeNoRun {
			continue
		}
		status, got := decideFailureStatus(connector.ToolResult{IsError: true, Text: "Decision: " + code + ": x"})
		if got != code {
			t.Errorf("decideFailureStatus rewrote code %q to %q", code, got)
			continue
		}
		examples := specMap(specMap(specMap(specMap(responses[strconv.Itoa(status)])["content"])["application/json"])["examples"])
		found := false
		for _, ex := range examples {
			if specString(specMap(specMap(ex)["value"])["code"]) == code {
				found = true
			}
		}
		if !found {
			t.Errorf("decision failure code %q maps to HTTP %d, but POST /v1/_decide has no %d example with that code in api/openapi.yaml", code, status, status)
		}
	}
}

// TestOpenAPIHandlers_ServedJSONCarriesTheDecisionRoutes fetches the JSON the
// docs console and client generators read and checks the decision routes
// survive the YAML-to-JSON rendering: both operations, and the noul criteria
// keys "true" and "false", which YAML reads as booleans unless quoted and
// which would then not render as JSON object keys at all.
func TestOpenAPIHandlers_ServedJSONCarriesTheDecisionRoutes(t *testing.T) {
	var s Server
	rec := httptest.NewRecorder()
	s.handleOpenAPIJSON(rec, httptest.NewRequest(http.MethodGet, "/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served JSON does not parse: %v", err)
	}
	paths := specMap(doc["paths"])
	for path, method := range map[string]string{"/v1/_decide": "post", "/v1/_decide/models": "get"} {
		if specMap(specMap(paths[path])[method])["operationId"] == nil {
			t.Errorf("served JSON has no %s %s operation", method, path)
		}
	}
	schemas := specMap(specMap(doc["components"])["schemas"])
	noul := specMap(specMap(specMap(specMap(schemas["DecisionNoulQuestion"])["properties"])["criteria"])["properties"])
	if noul["true"] == nil || noul["false"] == nil || len(noul) != 2 {
		t.Errorf("served JSON's noul criteria properties = %v, want exactly \"true\" and \"false\"", specKeys(noul))
	}
}

// stringConsts returns the values of the string constants in a Go source file
// whose names start with prefix. A constant declared as another constant of
// the same file (an exported alias of an unexported code) resolves to that
// one's value.
func stringConsts(t *testing.T, path, prefix string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	values := map[string]ast.Expr{}
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				values[name.Name] = vs.Values[i]
				if strings.HasPrefix(name.Name, prefix) {
					names = append(names, name.Name)
				}
			}
		}
	}
	var out []string
	for _, name := range names {
		expr := values[name]
		if ident, ok := expr.(*ast.Ident); ok {
			expr = values[ident.Name]
		}
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("%s: constant %s is not a string literal or an alias of one", path, name)
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("%s: constant %s: %v", path, name, err)
		}
		out = append(out, v)
	}
	return out
}

// specSchemas returns components.schemas from the embedded spec.
func specSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	out := map[string]map[string]any{}
	for name, sch := range specMap(specMap(doc["components"])["schemas"]) {
		out[name] = specMap(sch)
	}
	return out
}

func specMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func specSlice(v any) []any        { s, _ := v.([]any); return s }
func specString(v any) string      { s, _ := v.(string); return s }

func specStrings(v any) []string {
	var out []string
	for _, e := range specSlice(v) {
		out = append(out, specString(e))
	}
	return out
}

func specKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// assertSameSet fails when the tool and the spec do not name the same things.
func assertSameSet(t *testing.T, what string, tool, spec []string) {
	t.Helper()
	sort.Strings(tool)
	sort.Strings(spec)
	if len(tool) == 0 || strings.Join(tool, ",") != strings.Join(spec, ",") {
		t.Errorf("%s: the tool has %v, api/openapi.yaml has %v", what, tool, spec)
	}
}
