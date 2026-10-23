package http

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const servedSpecAdmin = "legacy-admin-secret"

// Whose access-control rules bound a run is kept for the server (a resume, a
// restarted queued run) and never served: not in the spec of the single-run
// reads, not in GET .../config or .../effective-config, not through the
// connector the MCP tools read — and not inside a sub-run's spec, whose
// override offer carries its parent's record.
func TestServedRun_CarriesNoACLOrigin(t *testing.T) {
	e := aclResumeFixture(t, true, map[string][][]providers.Event{
		"worker": {toolCallTurn("tu_child", "Agent", `{"name":"child","prompt":"work"}`), endTurn()},
		"child":  {endTurn()},
	})
	e.srv.SetSteerRegistry(steer.NewRegistry(0)) // an interactive run parks on its steer queue
	// The stream of an interactive run stays open while it parks; the request
	// is dropped at the end (the run itself is cancelled by its agent id), or
	// the test server's Close would wait on it.
	reqCtx, dropStream := context.WithCancel(context.Background())
	streamDone := make(chan struct{})
	t.Cleanup(func() { dropStream(); <-streamDone })
	go func() {
		defer close(streamDone)
		body := `{"agent":"worker","agent_id":"a_served","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
		req, _ := http.NewRequestWithContext(reqCtx, "POST", e.ts.URL+"/v1/runs", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+workerBearer)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	var parent string
	waitFor(t, "the interactive run to park", func() bool {
		r, err := e.st.GetRunByAgentID(context.Background(), "a_served")
		if err != nil {
			return false
		}
		parent = r.ID
		code, _ := bearerCall(t, e.ts, servedSpecAdmin, "GET", "/v1/runs/"+r.ID+"/config", "")
		return code == http.StatusOK && e.prov.callsOf("worker") == 2
	})
	stored, err := e.st.GetRun(context.Background(), parent)
	if err != nil || !strings.Contains(string(stored.RunConfig), "acl_origin") {
		t.Fatalf("fixture: the stored record holds no acl_origin: %s (%v)", stored.RunConfig, err)
	}
	kids, err := e.st.ListRunsByParentAgentID(context.Background(), "a_served")
	if err != nil || len(kids) != 1 || !strings.Contains(string(kids[0].RunConfig), "acl_origin") {
		t.Fatalf("fixture: the child's stored record holds no acl_origin (%d kids, %v)", len(kids), err)
	}
	child := kids[0]

	for _, path := range []string{
		"/v1/agents/a_served", "/v1/runs/" + parent, "/v1/runs/" + parent + "/config", "/v1/runs/" + parent + "/effective-config",
		"/v1/agents/" + child.AgentID, "/v1/runs/" + child.ID,
	} {
		code, body := bearerCall(t, e.ts, servedSpecAdmin, "GET", path, "")
		if code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, code, body)
			continue
		}
		if strings.Contains(body, "acl_origin") || strings.Contains(body, "def_worker") {
			t.Errorf("GET %s serves the caller's access-control origin: %s", path, body)
		}
	}
	for _, agentID := range []string{"a_served", child.AgentID} {
		run, err := e.srv.GetRun(context.Background(), agentID)
		if err != nil {
			t.Fatal(err)
		}
		if len(run.Spec) == 0 || strings.Contains(string(run.Spec), "acl_origin") {
			t.Errorf("connector GetRun(%s) spec = %s; want it without acl_origin", agentID, run.Spec)
		}
	}
	_, _ = bearerCall(t, e.ts, servedSpecAdmin, "POST", "/v1/agents/a_served/cancel", "")
	waitRunStatus(t, e.st, parent, store.RunCancelled)
}
