package localapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalSpec = `
openapi: 3.0.3
info:
  title: Test API
  version: 1.0.0
paths:
  /api/applications/{id}:
    get:
      operationId: get_application
      summary: Fetch one application
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
    patch:
      operationId: patch_application
      summary: Update an application
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                cvText: {type: string}
                coverLetterText: {type: string}
  /api/research:
    get:
      operationId: list_research
      summary: List research entries
      parameters:
        - name: date
          in: query
          schema:
            type: string
`

func writeSpec(t *testing.T, body string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "openapi.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func TestLoadSpec_Basic(t *testing.T) {
	path, _ := writeSpec(t, minimalSpec)
	sp, err := LoadSpec(path, "")
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if sp.OpenAPI != "3.0.3" {
		t.Errorf("OpenAPI = %q", sp.OpenAPI)
	}
	if len(sp.Paths) != 2 {
		t.Errorf("Paths len = %d, want 2", len(sp.Paths))
	}
	if sp.Paths["/api/applications/{id}"].Get == nil {
		t.Error("missing GET /api/applications/{id}")
	}
	if sp.Paths["/api/applications/{id}"].Patch == nil {
		t.Error("missing PATCH /api/applications/{id}")
	}
	if sp.Paths["/api/research"].Get == nil {
		t.Error("missing GET /api/research")
	}
}

// Endpoints must be sorted deterministically (path then method) so
// tool registration order is stable across restarts.
func TestSpec_Endpoints_StableOrder(t *testing.T) {
	path, _ := writeSpec(t, minimalSpec)
	sp, _ := LoadSpec(path, "")
	eps, warns := sp.Endpoints()
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
	// Expected order: /api/applications/{id} GET, /api/applications/{id} PATCH, /api/research GET
	want := []struct{ path, method string }{
		{"/api/applications/{id}", "GET"},
		{"/api/applications/{id}", "PATCH"},
		{"/api/research", "GET"},
	}
	if len(eps) != len(want) {
		t.Fatalf("endpoints len = %d, want %d", len(eps), len(want))
	}
	for i, w := range want {
		if eps[i].Path != w.path || eps[i].Method != w.method {
			t.Errorf("ep[%d] = %s %s, want %s %s",
				i, eps[i].Method, eps[i].Path, w.method, w.path)
		}
	}
}

// Operation without operationId surfaces a warning and is dropped.
// Without the warning the operator wouldn't know why their endpoint
// isn't reachable; without the drop we'd be unable to name the tool.
func TestSpec_MissingOperationId(t *testing.T) {
	body := `
openapi: 3.0.0
info: {title: x, version: x}
paths:
  /unnamed:
    get:
      summary: no operationId
  /named:
    get:
      operationId: ok
      summary: has it
`
	path, _ := writeSpec(t, body)
	sp, err := LoadSpec(path, "")
	if err != nil {
		t.Fatal(err)
	}
	eps, warns := sp.Endpoints()
	if len(eps) != 1 || eps[0].Operation.OperationID != "ok" {
		t.Errorf("expected only the named endpoint to survive, got %v", eps)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "/unnamed") {
		t.Errorf("expected one warning naming /unnamed: %v", warns)
	}
}

// Missing required `openapi:` field is a fatal load error.
func TestLoadSpec_MissingVersion(t *testing.T) {
	path, _ := writeSpec(t, "info: {title: x, version: x}\npaths: {}")
	if _, err := LoadSpec(path, ""); err == nil {
		t.Fatal("expected error on missing openapi field")
	}
}

// Non-3.x OpenAPI version is rejected — we don't support 2.0 (Swagger)
// or hypothetical 4.0 in this MVP.
func TestLoadSpec_RejectsNon3x(t *testing.T) {
	path, _ := writeSpec(t, "openapi: 2.0\ninfo: {title: x, version: x}\npaths: {}")
	_, err := LoadSpec(path, "")
	if err == nil || !strings.Contains(err.Error(), "3.x") {
		t.Errorf("expected 3.x-only error, got %v", err)
	}
}

// Empty paths map: the spec is technically valid OpenAPI but useless
// for our gateway; refuse loudly so the operator notices.
func TestLoadSpec_RejectsEmptyPaths(t *testing.T) {
	path, _ := writeSpec(t, "openapi: 3.0.0\ninfo: {title: x, version: x}\npaths: {}")
	_, err := LoadSpec(path, "")
	if err == nil || !strings.Contains(err.Error(), "no paths") {
		t.Errorf("expected no-paths error, got %v", err)
	}
}

// Missing file: error should name the resolved path so the operator
// sees exactly where loomcycle looked.
func TestLoadSpec_FileMissing(t *testing.T) {
	_, err := LoadSpec(filepath.Join(t.TempDir(), "no-such-file.yaml"), "")
	if err == nil || !strings.Contains(err.Error(), "no-such-file") {
		t.Errorf("error should name the path: %v", err)
	}
}

// Relative paths are resolved against configDir — same behaviour as
// system_prompt_file. Operator's loomcycle.yaml can carry a relative
// reference next to it.
func TestLoadSpec_RelativePathResolution(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(minimalSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := LoadSpec("spec.yaml", dir)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if len(sp.Paths) != 2 {
		t.Errorf("paths missing after relative-path load: %d", len(sp.Paths))
	}
}
