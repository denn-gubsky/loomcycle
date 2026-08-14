// Package api embeds machine-readable API contracts into the binary so the
// server can serve them without a filesystem dependency.
//
// The canonical source lives at api/openapi.yaml for external / GitHub
// consumption (and codegen). go:embed cannot reach "..", so this tiny package
// sits alongside the file; the HTTP server imports it to serve the spec at
// /v1/openapi.{yaml,json}. (RFC CD Part A.)
package api

import _ "embed"

// OpenAPISpecYAML is the raw OpenAPI 3.1 document for the loomcycle data API
// (memory + document/path). Served verbatim at /v1/openapi.yaml; rendered to
// JSON at /v1/openapi.json.
//
//go:embed openapi.yaml
var OpenAPISpecYAML []byte
