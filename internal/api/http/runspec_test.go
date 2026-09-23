package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A finished run's configuration stays readable: what POST /v1/runs merged and
// persisted is what the single-run reads return as `spec` — the crossing, not
// each end — and a listing leaves it out, as it does the result.
func TestRunReads_TheSpecARunWasStartedWithIsReadableAfterItEnds(t *testing.T) {
	srv, ts, _, st := toolChoiceServer(t)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","user_id":"u1","tool_choice":{"mode":"required"},"sampling":{"temperature":0.2},`+
			`"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	run := onlyRun(t, st, extractSessionID(string(body)))
	if run.Status == store.RunRunning {
		t.Fatalf("run still running; the read under test is of a FINISHED run")
	}

	req := httptest.NewRequest("GET", "/v1/agents/"+run.AgentID, nil)
	req.SetPathValue("agent_id", run.AgentID)
	rec := httptest.NewRecorder()
	srv.handleGetAgent(rec, req)
	var got struct {
		Spec struct {
			ToolChoice struct{ Mode string } `json:"tool_choice"`
			Sampling   struct{ Temperature *float64 }
		} `json:"spec"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("GET /v1/agents/{id} = %d %s", rec.Code, rec.Body)
	}
	if got.Spec.ToolChoice.Mode != "required" || got.Spec.Sampling.Temperature == nil || *got.Spec.Sampling.Temperature != 0.2 {
		t.Errorf("spec = %s, want the run's tool_choice and sampling", rec.Body)
	}

	crun, err := srv.GetRun(context.Background(), run.AgentID)
	if err != nil || !strings.Contains(string(crun.Spec), `"required"`) {
		t.Errorf("connector GetRun spec = %q (%v), want the run's record", crun.Spec, err)
	}
	if run.UserID == "" {
		t.Fatal("the run has no user_id, so the listing check below would read nothing")
	}
	listed, err := srv.ListRuns(context.Background(), listFilter(run.UserID))
	if err != nil || len(listed) == 0 {
		t.Fatalf("ListRuns = %d rows (%v), want the run", len(listed), err)
	}
	for _, r := range listed {
		if len(r.Spec) != 0 {
			t.Errorf("ListRuns row %s carries a spec; listings must stay small", r.AgentID)
		}
	}
}

// A run that overrode nothing has no record, and the read says so by leaving
// spec out — not by sending an empty object a reader would have to special-case.
func TestRunReads_ARunThatOverrodeNothingHasNoSpec(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	run := seedTenantRun(t, srv.store, "acme", "u1", "a_nospec")
	srv.finishRun(context.Background(), run.ID, loop.RunResult{FinalText: "x"}, nil, runStateMeta{})
	req := httptest.NewRequest("GET", "/v1/agents/a_nospec", nil)
	req.SetPathValue("agent_id", "a_nospec")
	req = req.WithContext(tenantOperatorCtx("acme"))
	rec := httptest.NewRecorder()
	srv.handleGetAgent(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"spec"`) {
		t.Errorf("GET /v1/agents/{id} = %d %s, want no spec", rec.Code, rec.Body)
	}
}

// The TS RunSpec is a hand-written mirror of runConfigRecord. Every key the Go
// record persists must be declared there, or a typed TS caller reading a run's
// spec cannot name that field — the drift the overlay mirror accumulated.
func TestRunSpec_TSMirrorDeclaresEveryRecordField(t *testing.T) {
	goSrc, err := os.ReadFile("runconfig.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)type runConfigRecord struct \{(.*?)\n\}`).FindSubmatch(goSrc)
	if m == nil {
		t.Fatal("could not find runConfigRecord — this test asserts nothing until it matches again")
	}
	var goFields []string
	for _, f := range regexp.MustCompile("`json:\"([a-z0-9_]+)").FindAllSubmatch(m[1], -1) {
		goFields = append(goFields, string(f[1]))
	}
	if len(goFields) < 10 {
		t.Fatalf("only %d record fields parsed — the pattern has stopped matching", len(goFields))
	}
	tsSrc, err := os.ReadFile("../../../adapters/ts/src/types.ts")
	if err != nil {
		t.Skipf("TS adapter not present: %v", err)
	}
	tm := regexp.MustCompile(`(?s)export interface RunSpec \{(.*?)\n\}`).FindSubmatch(tsSrc)
	if tm == nil {
		t.Fatal("could not find RunSpec in types.ts")
	}
	declared := map[string]bool{}
	for _, f := range regexp.MustCompile(`(?m)^\s*([a-z0-9_]+)\??:`).FindAllSubmatch(tm[1], -1) {
		declared[string(f[1])] = true
	}
	for _, f := range goFields {
		if !declared[f] {
			t.Errorf("runConfigRecord persists %q but TS RunSpec does not declare it", f)
		}
	}
}
