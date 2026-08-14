package http

import (
	"encoding/json"
	"net/http"
	"sync"

	"gopkg.in/yaml.v3"

	loomapi "github.com/denn-gubsky/loomcycle/api"
)

// openapi.go serves the machine-readable API contract (RFC CD Part A).
//
// The spec + the docs console are served UNAUTHENTICATED (registered with
// recoveryMiddleware only, no authMiddleware — the same posture as /ui): the
// contract is non-secret and must be discoverable so any language can generate
// a client, while the data behind it still goes through authMiddleware.

var (
	openapiJSONOnce sync.Once
	openapiJSON     []byte
	openapiJSONErr  error
)

// openapiSpecJSON renders the embedded YAML spec to JSON once and caches it.
// yaml.v3 decodes maps into map[string]interface{} (string keys), so the result
// marshals to JSON cleanly.
func openapiSpecJSON() ([]byte, error) {
	openapiJSONOnce.Do(func() {
		var doc any
		if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
			openapiJSONErr = err
			return
		}
		openapiJSON, openapiJSONErr = json.Marshal(doc)
	})
	return openapiJSON, openapiJSONErr
}

func (s *Server) handleOpenAPIYAML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(loomapi.OpenAPISpecYAML)
}

func (s *Server) handleOpenAPIJSON(w http.ResponseWriter, r *http.Request) {
	b, err := openapiSpecJSON()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"openapi_convert_failed","error":"could not render the OpenAPI spec as JSON"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
