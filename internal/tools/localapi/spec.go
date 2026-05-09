// Package localapi implements the Local API MCP gateway: it reads an
// OpenAPI 3.0 spec at boot and exposes each declared endpoint as a
// typed loomcycle tool. Replaces the curl-shaped HTTP-tool pattern
// jobs-search-agent's Phase B agents currently use to call back into
// the host application's `/api/agent/*` endpoints.
//
// Trust + scope:
//   - Tools are named `<prefix>__<operationId>` (default prefix
//     "local"); each carries the operation's input schema so typos
//     fail at the model layer instead of becoming a 4xx mid-run.
//   - Auth is carried by the agent on each call (the `bearer` field
//     in every tool's input schema). Agents already see the Bearer
//     token in their prompt's auth preamble; this is the typed
//     replacement for `headers: {Authorization: ...}` on the
//     existing HTTP tool.
//   - SSRF / private-IP defences DO NOT apply: this gateway is
//     deliberately pointed at a trusted host (the operator's own
//     application server). Operators MUST set base_url to a known
//     internal address; do not point this at attacker-controlled
//     URLs.
//
// Constraints (MVP, v0.4.0):
//   - Inline schemas only — `$ref` is NOT resolved. Operators must
//     bundle their spec before pointing loomcycle at it (e.g. via
//     swagger-cli bundle, redocly bundle).
//   - JSON request/response bodies only (`application/json` content
//     type). Form-data, multipart, and binary payloads are not
//     wrapped.
//   - The supported HTTP methods are GET/POST/PUT/PATCH/DELETE.
//     OPTIONS / HEAD / TRACE / WebSocket are skipped.
//   - Top-level `servers[0].url` is ignored — base_url is always
//     operator-supplied via loomcycle.yaml. Mixing the two would
//     create surprise routing.
package localapi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is the parsed subset of an OpenAPI 3.0 document. Fields not
// listed here are ignored.
type Spec struct {
	OpenAPI    string              `yaml:"openapi"`
	Info       infoBlock           `yaml:"info"`
	Paths      map[string]pathItem `yaml:"paths"`
	Components componentsBlock     `yaml:"components"`
	rawBytes   []byte              // kept for diagnostics
}

type infoBlock struct {
	Title   string `yaml:"title"`
	Version string `yaml:"version"`
}

// pathItem is one entry under `paths`. We accept the standard HTTP
// verbs and ignore parameters declared at the path level (operators
// should declare them per-operation for clarity in the rendered tool
// names).
type pathItem struct {
	Get    *operation `yaml:"get"`
	Post   *operation `yaml:"post"`
	Put    *operation `yaml:"put"`
	Patch  *operation `yaml:"patch"`
	Delete *operation `yaml:"delete"`
}

// operation is the parsed shape of one (path, method) tuple. Schemas
// are kept as `*yaml.Node` so we can re-serialise them as JSON Schema
// without deep-parsing every type variant the OpenAPI spec allows.
type operation struct {
	OperationID string       `yaml:"operationId"`
	Summary     string       `yaml:"summary"`
	Description string       `yaml:"description"`
	Parameters  []parameter  `yaml:"parameters"`
	RequestBody *requestBody `yaml:"requestBody"`
	Tags        []string     `yaml:"tags"`
}

type parameter struct {
	Name        string     `yaml:"name"`
	In          string     `yaml:"in"` // "path" | "query" | "header" | "cookie"
	Description string     `yaml:"description"`
	Required    bool       `yaml:"required"`
	Schema      *yaml.Node `yaml:"schema"`
}

type requestBody struct {
	Description string                  `yaml:"description"`
	Required    bool                    `yaml:"required"`
	Content     map[string]mediaContent `yaml:"content"`
}

type mediaContent struct {
	Schema *yaml.Node `yaml:"schema"`
}

type componentsBlock struct {
	// We don't resolve $ref in the MVP (see package doc). The block is
	// parsed only so a future enhancement that DOES resolve refs has
	// the data to work with — kept for forward compatibility.
	Schemas map[string]*yaml.Node `yaml:"schemas"`
}

// LoadSpec reads an OpenAPI YAML/JSON file from disk and parses it into
// the supported subset.
//
// Path resolution: relative paths are resolved against the operator's
// configured config directory (passed via configDir). An empty
// configDir treats the path as-is. Absolute paths bypass the join.
func LoadSpec(specPath, configDir string) (*Spec, error) {
	if specPath == "" {
		return nil, fmt.Errorf("localapi: empty spec path")
	}
	resolved := specPath
	if !filepath.IsAbs(resolved) && configDir != "" {
		resolved = filepath.Join(configDir, resolved)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("localapi: read %s: %w", resolved, err)
	}
	var sp Spec
	if err := yaml.Unmarshal(raw, &sp); err != nil {
		return nil, fmt.Errorf("localapi: parse %s: %w", resolved, err)
	}
	if sp.OpenAPI == "" {
		return nil, fmt.Errorf("localapi: %s missing required `openapi:` field (must be 3.0.x)", resolved)
	}
	if !strings.HasPrefix(sp.OpenAPI, "3.") {
		return nil, fmt.Errorf("localapi: %s declares OpenAPI %q; only 3.x supported", resolved, sp.OpenAPI)
	}
	if len(sp.Paths) == 0 {
		return nil, fmt.Errorf("localapi: %s has no paths", resolved)
	}
	sp.rawBytes = raw
	return &sp, nil
}

// Endpoint is one resolved (path, method, operation) tuple ready to
// become a tool. Builders walk the spec via Endpoints() and call
// BuildTool for each.
type Endpoint struct {
	Path      string
	Method    string // "GET" | "POST" | "PUT" | "PATCH" | "DELETE"
	Operation *operation
}

// Endpoints flattens the Spec into a list of (path, method, operation)
// tuples sorted deterministically by (path, method) so tool registration
// order is stable across restarts.
//
// Operations without an operationId are SKIPPED with a warning line —
// without one we have nothing meaningful to name the tool. The caller
// receives the warnings via the returned warns slice; main.go logs
// them so operators see exactly which entries got dropped.
func (s *Spec) Endpoints() (eps []Endpoint, warns []string) {
	type entry struct {
		path, method string
		op           *operation
	}
	var raw []entry
	for path, item := range s.Paths {
		if item.Get != nil {
			raw = append(raw, entry{path, "GET", item.Get})
		}
		if item.Post != nil {
			raw = append(raw, entry{path, "POST", item.Post})
		}
		if item.Put != nil {
			raw = append(raw, entry{path, "PUT", item.Put})
		}
		if item.Patch != nil {
			raw = append(raw, entry{path, "PATCH", item.Patch})
		}
		if item.Delete != nil {
			raw = append(raw, entry{path, "DELETE", item.Delete})
		}
	}
	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].path != raw[j].path {
			return raw[i].path < raw[j].path
		}
		return raw[i].method < raw[j].method
	})
	for _, r := range raw {
		if r.op.OperationID == "" {
			warns = append(warns,
				fmt.Sprintf("%s %s: missing operationId; skipped (loomcycle needs operationId to name the tool)",
					r.method, r.path))
			continue
		}
		eps = append(eps, Endpoint{Path: r.path, Method: r.method, Operation: r.op})
	}
	return eps, warns
}
