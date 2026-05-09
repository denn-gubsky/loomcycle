package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// minimalServer constructs a Server with just enough wiring to exercise
// the /v1/hooks routes. No providers, no store — those aren't needed
// for the registration HTTP surface itself.
func minimalServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{}
	hookReg := hooks.NewRegistry()
	return &Server{
		cfg:            cfg,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
}

func TestHooksAPI_RegisterListDelete(t *testing.T) {
	s := minimalServer(t)

	// Register
	body := `{
		"owner": "jobs-search-web",
		"name": "scan-webfetch",
		"phase": "post",
		"agents": ["*"],
		"tools": ["WebFetch"],
		"callback_url": "https://jobs-search-web.local/hooks/scan",
		"fail_mode": "open",
		"timeout_ms": 3000
	}`
	req := httptest.NewRequest("POST", "/v1/hooks", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleRegisterHook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Register status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var regResp hookRegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if !strings.HasPrefix(regResp.ID, "hook_") {
		t.Errorf("id %q lacks hook_ prefix", regResp.ID)
	}

	// List
	rec = httptest.NewRecorder()
	s.handleListHooks(rec, httptest.NewRequest("GET", "/v1/hooks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("List status = %d", rec.Code)
	}
	var listResp struct {
		Hooks []*hooks.Hook `json:"hooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Hooks) != 1 || listResp.Hooks[0].ID != regResp.ID {
		t.Errorf("List = %+v, want one entry with id %s", listResp.Hooks, regResp.ID)
	}

	// Delete
	delReq := httptest.NewRequest("DELETE", "/v1/hooks/"+regResp.ID, nil)
	delReq.SetPathValue("id", regResp.ID)
	rec = httptest.NewRecorder()
	s.handleDeleteHook(rec, delReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("Delete status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Delete again → 404
	rec = httptest.NewRecorder()
	s.handleDeleteHook(rec, delReq)
	if rec.Code != http.StatusNotFound {
		t.Errorf("second Delete status = %d, want 404", rec.Code)
	}
}

// TestHooksAPI_ReplaceOnDuplicate is the cascading-prevention guard
// over the wire surface. Re-registering the same (owner, name) MUST
// return a fresh id and List should still show one entry.
func TestHooksAPI_ReplaceOnDuplicate(t *testing.T) {
	s := minimalServer(t)
	body := func(callback string) string {
		return `{
			"owner": "x", "name": "y", "phase": "pre",
			"tools": ["WebFetch"],
			"callback_url": "` + callback + `"
		}`
	}

	// First registration
	req := httptest.NewRequest("POST", "/v1/hooks", bytes.NewReader([]byte(body("https://a/x"))))
	rec := httptest.NewRecorder()
	s.handleRegisterHook(rec, req)
	var first hookRegisterResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)

	// Second registration, same (owner, name)
	req = httptest.NewRequest("POST", "/v1/hooks", bytes.NewReader([]byte(body("https://b/x"))))
	rec = httptest.NewRecorder()
	s.handleRegisterHook(rec, req)
	var second hookRegisterResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)

	if first.ID == second.ID {
		t.Errorf("re-register returned same id (%q) — replace must mint a fresh id", first.ID)
	}

	// List must show ONE entry, with the second's callback URL.
	rec = httptest.NewRecorder()
	s.handleListHooks(rec, httptest.NewRequest("GET", "/v1/hooks", nil))
	var listResp struct {
		Hooks []*hooks.Hook `json:"hooks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Hooks) != 1 {
		t.Fatalf("List len = %d, want 1 (replace must not append)", len(listResp.Hooks))
	}
	if listResp.Hooks[0].CallbackURL != "https://b/x" {
		t.Errorf("List[0].CallbackURL = %q, want b/x", listResp.Hooks[0].CallbackURL)
	}
}

func TestHooksAPI_RejectsInvalid(t *testing.T) {
	s := minimalServer(t)
	cases := map[string]string{
		"missing owner":    `{"name":"x","phase":"pre","callback_url":"https://e/x"}`,
		"missing callback": `{"owner":"x","name":"x","phase":"pre"}`,
		"bad phase":        `{"owner":"x","name":"x","phase":"during","callback_url":"https://e/x"}`,
		"bad scheme":       `{"owner":"x","name":"x","phase":"pre","callback_url":"ftp://e/x"}`,
	}
	for name, body := range cases {
		req := httptest.NewRequest("POST", "/v1/hooks", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		s.handleRegisterHook(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", name, rec.Code, rec.Body.String())
		}
	}
}
