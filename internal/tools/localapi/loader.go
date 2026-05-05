package localapi

import (
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Config is the operator-supplied configuration for the local-api
// gateway, populated from the loomcycle.yaml `local_api` block.
type Config struct {
	// SpecPath is the OpenAPI 3.0 file. Relative paths are resolved
	// against the loomcycle.yaml directory.
	SpecPath string `yaml:"spec"`
	// BaseURL is where forward requests are sent. The OpenAPI spec's
	// own `servers[0].url` is intentionally NOT honoured — operator
	// is the authority on routing.
	BaseURL string `yaml:"base_url"`
	// ToolNamePrefix is prepended to every operationId. Default
	// "local"; tools become e.g. `local__patch_application`.
	ToolNamePrefix string `yaml:"tool_name_prefix"`
}

// Build reads the OpenAPI spec, walks its operations, and returns the
// generated EndpointTools as the standard tools.Tool slice. main.go
// appends these to allTools alongside the built-ins.
//
// Returns the tools, a slice of warnings (entries the loader skipped
// or normalised), and any fatal error. A fatal error means the gateway
// can't start; loomcycle continues running without it after logging.
func Build(cfg Config, configDir string) ([]tools.Tool, []string, error) {
	if cfg.SpecPath == "" {
		return nil, nil, fmt.Errorf("localapi: spec is required")
	}
	if cfg.BaseURL == "" {
		return nil, nil, fmt.Errorf("localapi: base_url is required")
	}
	prefix := cfg.ToolNamePrefix
	if prefix == "" {
		prefix = "local"
	}
	if !validPrefix(prefix) {
		return nil, nil, fmt.Errorf("localapi: invalid tool_name_prefix %q (use [a-zA-Z0-9_-]+)", prefix)
	}

	spec, err := LoadSpec(cfg.SpecPath, configDir)
	if err != nil {
		return nil, nil, err
	}

	endpoints, warns := spec.Endpoints()

	out := make([]tools.Tool, 0, len(endpoints))
	for _, ep := range endpoints {
		toolName := prefix + "__" + ep.Operation.OperationID
		// operationId values that don't survive the loomcycle tool-
		// name conventions (must be addressable from agent allow-
		// lists, which are simple strings) are normalised: replace
		// any non-[a-zA-Z0-9_] with "_". This is rare in well-formed
		// OpenAPI but cheap to enforce so we don't surprise an
		// operator whose generator emits camelCase-with-dashes.
		safe := normaliseToolName(toolName)
		if safe != toolName {
			warns = append(warns,
				fmt.Sprintf("operation %s: normalised tool name %q → %q", ep.Operation.OperationID, toolName, safe))
			toolName = safe
		}

		tool := &EndpointTool{
			ToolName:      toolName,
			BaseURL:       cfg.BaseURL,
			Path:          ep.Path,
			Method:        ep.Method,
			OpSummary:     ep.Operation.Summary,
			OpDescription: ep.Operation.Description,
			Parameters:    ep.Operation.Parameters,
		}

		if rb := ep.Operation.RequestBody; rb != nil {
			if mc, ok := rb.Content["application/json"]; ok && mc.Schema != nil {
				tool.HasBody = true
				tool.BodySchema = mc.Schema
				// Note: we don't track requestBody.required separately
				// because the input schema's `required` array is
				// driven only by the parameters loop. If a future
				// release wants to enforce body presence at schema
				// level, add a HasBodyRequired flag and append "body"
				// to required when set.
			} else if rb.Content != nil {
				warns = append(warns,
					fmt.Sprintf("%s %s: requestBody.content has no application/json entry; body skipped",
						ep.Method, ep.Path))
			}
		}
		out = append(out, tool)
	}
	return out, warns, nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			continue
		default:
			return false
		}
	}
	return true
}

// normaliseToolName replaces every char that isn't [a-zA-Z0-9_-] with
// "_". The "__" separator is preserved because each side is already
// validated/normalised independently. Result is always non-empty.
func normaliseToolName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
