package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The presets/env-template handlers serve only embedded content (no store/config
// deps), so a zero Server suffices. Scope-gating is exercised by the
// requiredScopeFor tests; these assert the handler bodies.

func TestHandleListPresets(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleListPresets(rec, httptest.NewRequest(http.MethodGet, "/v1/_presets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Units []struct {
			Name, Kind, Description string
		} `json:"units"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]string{}
	for _, u := range resp.Units {
		byName[u.Name] = u.Kind
	}
	if byName["base"] != "preset" || byName["document-agent"] != "bundle" {
		t.Errorf("units missing base(preset)/document-agent(bundle): %v", byName)
	}
}

func TestHandleShowPreset(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/_presets/base", nil)
	req.SetPathValue("name", "base")
	s.handleShowPreset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "provider_priority") {
		t.Errorf("base YAML should contain provider_priority, got: %s", rec.Body.String())
	}

	// Unknown name → 404.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/_presets/nope", nil)
	req.SetPathValue("name", "nope")
	s.handleShowPreset(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown preset status = %d, want 404", rec.Code)
	}
}

func TestHandleEnvTemplate(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleEnvTemplate(rec, httptest.NewRequest(http.MethodGet, "/v1/_env_template", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "LOOMCYCLE_") {
		t.Errorf("env template should contain LOOMCYCLE_ vars")
	}
}
