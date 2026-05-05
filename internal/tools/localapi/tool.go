package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// EndpointTool is one tool generated from an OpenAPI operation. The
// model invokes it with a JSON body conforming to the synthesised
// input schema; the tool forwards the call to BaseURL + path with the
// agent-supplied bearer token.
//
// One tool per (path, method) — see (a) in package doc for design
// rationale. The model sees a clear menu of named operations rather
// than one generic "call" tool with an `endpoint` parameter; renamed
// or removed endpoints become missing tools rather than silent 404s.
//
// Network policy:
//   - The forward request goes to BaseURL via the standard net/http
//     client; SSRF/private-IP defences from the HTTP built-in tool
//     do NOT apply here. The operator must point this at a trusted
//     host (their own application server, typically localhost or a
//     known internal address).
//   - HTTPClient may be overridden for tests; nil means use
//     http.DefaultClient.
//
// Errors visible to the model:
//   - Invalid JSON input → IsError tool_result, model can self-correct.
//   - Missing required parameters → IsError, names the missing field.
//   - 4xx/5xx from the upstream → IsError, body forwarded as text so
//     the model sees the application's error shape (e.g. validation
//     errors).
//   - Transport failures (connection refused, DNS) → Go error from
//     Execute (rare; surfaces as the loop's tool_result with the
//     wrapper text).
type EndpointTool struct {
	ToolName    string                  // "<prefix>__<operationId>"
	BaseURL     string                  // e.g. "http://localhost:3000"
	Path        string                  // e.g. "/api/applications/{id}"
	Method      string                  // "GET" | "POST" | ...
	OpSummary   string
	OpDescription string
	Parameters  []parameter
	HasBody     bool
	BodySchema  *yaml.Node // raw JSON Schema (passed through to model)

	HTTPClient *http.Client // optional; defaults to http.DefaultClient
}

// Name implements tools.Tool. Returns the assembled
// "<prefix>__<operationId>" string the model sees.
func (t *EndpointTool) Name() string { return t.ToolName }

// Description implements tools.Tool. Combines the OpenAPI summary +
// description + an explicit auth note so the model always knows it
// must include `bearer`.
func (t *EndpointTool) Description() string {
	parts := []string{}
	if t.OpSummary != "" {
		parts = append(parts, t.OpSummary)
	}
	if t.OpDescription != "" {
		parts = append(parts, t.OpDescription)
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%s %s", t.Method, t.Path))
	}
	parts = append(parts,
		"Authentication: include the Bearer token from your prompt's auth preamble in the `bearer` field of every call.")
	return strings.Join(parts, "\n\n")
}

// InputSchema implements tools.Tool. Synthesises a JSON Schema from
// the OpenAPI parameters + requestBody + a required `bearer` field.
//
// Schema shape:
//
//	{
//	  "type": "object",
//	  "properties": {
//	    "bearer":   {"type": "string", "description": "..."},
//	    "<path-param>":  <param's schema>,
//	    "<query-param>": <param's schema>,
//	    "body":     <requestBody schema, if any>
//	  },
//	  "required": ["bearer", ...required-params, "body" if body required]
//	}
func (t *EndpointTool) InputSchema() json.RawMessage {
	props := map[string]any{
		"bearer": map[string]any{
			"type":        "string",
			"description": "Bearer token to forward as `Authorization: Bearer <value>`. Sourced from the Bearer token printed in your prompt's auth preamble.",
		},
	}
	required := []string{"bearer"}

	for _, p := range t.Parameters {
		if p.In == "header" || p.In == "cookie" {
			// Skip header/cookie parameters in the input schema —
			// the only header we forward is Authorization (set from
			// `bearer`). Cookies are not supported.
			continue
		}
		schema := yamlNodeToJSON(p.Schema)
		if schema == nil {
			schema = map[string]any{"type": "string"}
		}
		// Augment the parameter schema with a description so the model
		// knows where the param goes (path vs query) — useful when the
		// OpenAPI desc was sparse.
		if m, ok := schema.(map[string]any); ok {
			d := p.Description
			if d == "" {
				d = fmt.Sprintf("%s parameter", p.In)
			}
			if _, exists := m["description"]; !exists {
				m["description"] = d
			}
		}
		props[p.Name] = schema
		if p.Required {
			required = append(required, p.Name)
		}
	}

	if t.HasBody {
		bodySchema := yamlNodeToJSON(t.BodySchema)
		if bodySchema == nil {
			bodySchema = map[string]any{"type": "object"}
		}
		props["body"] = bodySchema
		// We treat the body as required only if the OpenAPI spec
		// explicitly marked it so. Many specs default to false.
		// (See Endpoint construction in loader.go for where the flag
		// is propagated.)
		// Required-ness is recorded on the EndpointTool by the loader.
	}

	full := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	out, err := json.Marshal(full)
	if err != nil {
		// Schema construction is purely from operator-controlled
		// input; failure is a programmer error, not runtime.
		return json.RawMessage(`{"type":"object"}`)
	}
	return json.RawMessage(out)
}

// Execute implements tools.Tool. Validates input, builds the forward
// HTTP request from the OpenAPI definition + supplied parameters, and
// returns the upstream response.
//
// Errors are surfaced as IsError tool_results (recoverable by the
// model) rather than Go errors (which would tear down the run). Only
// transport-level failures bubble up as Go errors.
func (t *EndpointTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	var raw map[string]any
	if err := json.Unmarshal(input, &raw); err != nil {
		return tools.Result{IsError: true, Text: fmt.Sprintf("invalid input JSON: %s", err)}, nil
	}

	bearer, _ := raw["bearer"].(string)
	bearer = strings.TrimSpace(bearer)
	if bearer == "" {
		return tools.Result{
			IsError: true,
			Text:    "missing required field: bearer (the Authorization token from your prompt's auth preamble)",
		}, nil
	}

	// Substitute path parameters into the URL template. url.PathEscape
	// escapes characters outside the unreserved set (which includes
	// '.' but NOT '/'), so '/' becomes '%2F' on the wire. The dot is
	// untouched, but it can't introduce a path-traversal step without
	// a slash; `id=../admin` becomes `..%2Fadmin`, which the upstream
	// router treats as one literal segment. OpenAPI path parameters
	// are scoped to a single segment by default (style: simple); this
	// escape matches that contract.
	urlPath := t.Path
	for _, p := range t.Parameters {
		if p.In != "path" {
			continue
		}
		v, ok := raw[p.Name]
		if !ok {
			if p.Required {
				return tools.Result{IsError: true, Text: fmt.Sprintf("missing required path parameter %q", p.Name)}, nil
			}
			continue
		}
		urlPath = strings.ReplaceAll(urlPath, "{"+p.Name+"}", url.PathEscape(fmt.Sprintf("%v", v)))
	}

	// Build query string from query parameters.
	q := url.Values{}
	for _, p := range t.Parameters {
		if p.In != "query" {
			continue
		}
		v, ok := raw[p.Name]
		if !ok {
			if p.Required {
				return tools.Result{IsError: true, Text: fmt.Sprintf("missing required query parameter %q", p.Name)}, nil
			}
			continue
		}
		q.Set(p.Name, fmt.Sprintf("%v", v))
	}

	// Build full URL.
	full := strings.TrimRight(t.BaseURL, "/") + urlPath
	if encoded := q.Encode(); encoded != "" {
		full += "?" + encoded
	}

	// Build body if present.
	var bodyReader io.Reader
	if t.HasBody {
		body, hasBody := raw["body"]
		if hasBody && body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return tools.Result{IsError: true, Text: fmt.Sprintf("could not marshal body: %s", err)}, nil
			}
			bodyReader = bytes.NewReader(b)
		}
	}

	req, err := http.NewRequestWithContext(ctx, t.Method, full, bodyReader)
	if err != nil {
		// Almost always a malformed URL; surface as IsError so the
		// model sees the bad input.
		return tools.Result{IsError: true, Text: fmt.Sprintf("build request: %s", err)}, nil
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	client := t.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level failure — return as Go error so the loop's
		// tool_result wrapping carries the message intact.
		return tools.Result{IsError: true, Text: fmt.Sprintf("upstream request failed: %s", err)}, nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap, matches HTTP tool
	if err != nil {
		return tools.Result{IsError: true, Text: fmt.Sprintf("read response: %s", err)}, nil
	}

	if resp.StatusCode >= 400 {
		// Forward the upstream body to the model so it sees the
		// application's error shape (validation errors, 401 hints,
		// etc.) and can self-correct.
		return tools.Result{
			IsError: true,
			Text:    fmt.Sprintf("upstream %s %s returned %d:\n%s", t.Method, t.Path, resp.StatusCode, string(respBody)),
		}, nil
	}

	return tools.Result{Text: string(respBody)}, nil
}

// yamlNodeToJSON converts a yaml.Node to the equivalent
// map[string]any/[]any/scalar tree that json.Marshal will turn into
// proper JSON. Nil node → nil. Returns the converted value or nil if
// the node is empty.
//
// We use yaml.Node's Decode into `interface{}` to leverage yaml.v3's
// existing scalar resolution rules (ints stay ints, floats stay
// floats, etc.) — the resulting tree round-trips through json.Marshal
// cleanly.
func yamlNodeToJSON(n *yaml.Node) any {
	if n == nil {
		return nil
	}
	var out any
	if err := n.Decode(&out); err != nil {
		return nil
	}
	return out
}

var _ tools.Tool = (*EndpointTool)(nil)
