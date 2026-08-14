package http

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	loomapi "github.com/denn-gubsky/loomcycle/api"
)

// openapiDocsAssets holds the self-contained Swagger UI console served at
// /v1/docs (index.html + the vendored swagger-ui-dist files; see
// openapi_assets/PROVENANCE.md). No CDN — works air-gapped.
//
//go:embed openapi_assets/index.html openapi_assets/swagger-ui.css openapi_assets/swagger-ui-bundle.js
var openapiDocsAssets embed.FS

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

// docsAsset maps the whitelisted /v1/docs asset name to its content type. Only
// these three files are ever served — a whitelist (not a filesystem walk) so
// there is no traversal surface.
var docsAsset = map[string]string{
	"index.html":           "text/html; charset=utf-8",
	"swagger-ui.css":       "text/css; charset=utf-8",
	"swagger-ui-bundle.js": "application/javascript; charset=utf-8",
}

// handleOpenAPIDocs serves the self-contained Swagger UI console at /v1/docs
// (and its two vendored assets under /v1/docs/). Unauthenticated, like the spec
// itself — the console authenticates its "Try it out" calls with a bearer the
// operator pastes into Swagger UI's Authorize dialog.
func (s *Server) handleOpenAPIDocs(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/docs")
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "index.html"
	}
	ct, ok := docsAsset[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := openapiDocsAssets.ReadFile("openapi_assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		// Version-pinned vendored assets — safe to cache.
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	_, _ = w.Write(data)
}
