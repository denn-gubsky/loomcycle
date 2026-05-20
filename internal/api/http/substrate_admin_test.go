package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/skills"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// substrateAdminFixture spins up an HTTP Server with the two
// substrate tools registered (AgentDef + SkillDef), an in-memory
// SQLite store, and bearer auth. Returns the test httptest.Server.
func substrateAdminFixture(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		// Leave cfg.Agents empty so AgentDef.create of a DB-only
		// name is accepted (static-name guard wouldn't fire).
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	cfg.Env.AuthToken = "test-token"

	emptySkillSet, err := skills.LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	agentDefTool := &builtin.AgentDef{Cfg: cfg, Store: st}
	skillDefTool := &builtin.SkillDef{Set: emptySkillSet, Store: st}

	srv := New(cfg, &stubResolver{}, []tools.Tool{agentDefTool, skillDefTool}, concurrency.New(1, 1, time.Second), st)
	return httptest.NewServer(srv.Mux())
}

func TestSubstrateAdmin_SkillDef_HappyPath(t *testing.T) {
	ts := substrateAdminFixture(t)
	defer ts.Close()

	body := `{"op":"create","name":"runtime-skill","overlay":{"body":"FRESH BODY"}}`
	resp := postAdmin(t, ts, "/v1/_skilldef", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["name"] != "runtime-skill" {
		t.Errorf("name = %v, want runtime-skill", out["name"])
	}
	if out["version"].(float64) != 1 {
		t.Errorf("version = %v, want 1", out["version"])
	}
	if out["promoted"].(bool) != true {
		t.Errorf("create default promote = false; want true")
	}
}

func TestSubstrateAdmin_AgentDef_HappyPath(t *testing.T) {
	ts := substrateAdminFixture(t)
	defer ts.Close()

	body := `{"op":"create","name":"runtime-agent","overlay":{"system_prompt":"hi"}}`
	resp := postAdmin(t, ts, "/v1/_agentdef", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["name"] != "runtime-agent" {
		t.Errorf("name = %v, want runtime-agent", out["name"])
	}
}

func TestSubstrateAdmin_RejectsMalformedBody(t *testing.T) {
	ts := substrateAdminFixture(t)
	defer ts.Close()

	resp := postAdmin(t, ts, "/v1/_skilldef", `not json`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSubstrateAdmin_RequiresBearer(t *testing.T) {
	ts := substrateAdminFixture(t)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/_skilldef", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestSubstrateAdmin_ToolRefusal_Returns422 — a SkillDef.create
// with an empty body is refused by the in-process tool; the HTTP
// layer maps that to 422 with a canonical error envelope.
func TestSubstrateAdmin_ToolRefusal_Returns422(t *testing.T) {
	ts := substrateAdminFixture(t)
	defer ts.Close()

	body := `{"op":"create","name":"bad","overlay":{"body":"   "}}`
	resp := postAdmin(t, ts, "/v1/_skilldef", body)
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 422; body=%s", resp.StatusCode, raw)
		return
	}
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env["code"] != "tool_refused" {
		t.Errorf("code = %v, want tool_refused", env["code"])
	}
	if env["tool"] != "SkillDef" {
		t.Errorf("tool = %v, want SkillDef", env["tool"])
	}
}

func postAdmin(t *testing.T, ts *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
