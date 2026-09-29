package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A run records the AgentDef version its agent name resolved to when it
// started, so a resume can continue on that version. These tests check the
// write side: what a real run start, and a real spawn, put in the record.

// putAgentDef writes one version of name in the shared tenant, active or not.
func putAgentDef(t *testing.T, st store.Store, defID, name string, version int, definition string, active bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: defID, Name: name, Version: version,
		Definition: []byte(definition), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AgentDefCreate %s: %v", defID, err)
	}
	if active {
		if err := st.AgentDefSetActive(ctx, "", name, defID, ""); err != nil {
			t.Fatalf("AgentDefSetActive %s: %v", defID, err)
		}
	}
}

// postAgentRun starts one run of agent and returns its row once it is done.
func postAgentRun(t *testing.T, st store.Store, ts *httptest.Server, agent string) store.Run {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"`+agent+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session frame in SSE body:\n%s", body)
	}
	return onlyRun(t, st, sessionID)
}

// recordedVersion is the run's recorded agent version, failing when there is
// none — "no record" and "a record naming no version" mean different things.
func recordedVersion(t *testing.T, run store.Run) string {
	t.Helper()
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok {
		t.Fatalf("run %s carries no run_config", run.ID)
	}
	if rec.AgentVersion == nil {
		t.Fatalf("run %s recorded no agent version; a resume could only re-resolve its agent by name", run.ID)
	}
	return rec.AgentVersion.DefID
}

func TestRunStart_RecordsTheAgentVersionItResolvedTo(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Agents = map[string]config.AgentDef{
		"yaml-agent": {Model: "stub-model", Tools: []string{}, SystemPrompt: "static"},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	srv, st, ts := spawnCeilingServer(t, cfg, prov, nil)
	putAgentDef(t, st, "def_worker_v1", "worker", 1,
		`{"provider":"scripted","model":"stub-model","system_prompt":"v1","tools":[]}`, true)

	first := postAgentRun(t, st, ts, "worker")
	if got := recordedVersion(t, first); got != "def_worker_v1" {
		t.Errorf("POST /v1/runs recorded version %q, want def_worker_v1 (the active version it ran on)", got)
	}

	// Each run records the version active when IT started. RunOnce is the
	// start path of gRPC, MCP spawn_run, webhooks, schedules and A2A.
	putAgentDef(t, st, "def_worker_v2", "worker", 2,
		`{"provider":"scripted","model":"stub-model","system_prompt":"v2","tools":[]}`, true)
	if err := srv.RunOnce(context.Background(), runner.RunInput{
		Agent: "worker", AgentID: "a_once",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
	}, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	once, err := st.GetRunByAgentID(context.Background(), "a_once")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordedVersion(t, once); got != "def_worker_v2" {
		t.Errorf("RunOnce recorded version %q, want def_worker_v2", got)
	}

	// A continuation of the first chat is a run start too.
	if code, body := do(t, "POST", ts.URL+"/v1/sessions/"+first.SessionID+"/messages", `{"prompt":"more","agent_id":"a_cont"}`); code != http.StatusOK {
		t.Fatalf("continuation: %d %s", code, body)
	}
	cont, err := st.GetRunByAgentID(context.Background(), "a_cont")
	if err != nil {
		t.Fatal(err)
	}
	if got := recordedVersion(t, cont); got != "def_worker_v2" {
		t.Errorf("a continuation recorded version %q, want def_worker_v2", got)
	}
	// The operator's yaml has no versions: the record is there, naming none.
	if got := recordedVersion(t, postAgentRun(t, st, ts, "yaml-agent")); got != "" {
		t.Errorf("a yaml agent recorded version %q, want none", got)
	}
}

// A child records the version its NAME resolved to; under a def_id pin that is
// the base the pinned version was laid over, and the pin is the row's column.
func TestSubRunStart_RecordsTheChildsAgentVersion(t *testing.T) {
	for _, tc := range []struct {
		name, spawn, wantVersion, wantPin string
	}{
		{"by name", `{"name":"child","prompt":"hi"}`, "def_child_v1", ""},
		{"pinned by def_id", `{"name":"child","prompt":"hi","def_id":"def_child_v2"}`, "def_child_v1", "def_child_v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := makeBaseConfig()
			cfg.Defaults.Provider = "scripted"
			cfg.Agents = map[string]config.AgentDef{
				"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:parent."},
			}
			prov := &roleProvider{
				roles:   []string{"parent", "child"},
				scripts: map[string][][]providers.Event{"parent": {toolCallTurn("tu_spawn", "Agent", tc.spawn)}},
				calls:   map[string]int{},
			}
			_, st, ts := spawnCeilingServer(t, cfg, prov, nil)
			putAgentDef(t, st, "def_child_v1", "child", 1,
				`{"provider":"scripted","model":"stub-model","system_prompt":"role:child. v1","tools":[]}`, true)
			putAgentDef(t, st, "def_child_v2", "child", 2,
				`{"provider":"scripted","model":"stub-model","system_prompt":"role:child. v2","tools":[]}`, false)

			child := spawnLiveChild(t, st, ts, "a_parent_"+strings.ReplaceAll(tc.name, " ", "_"), "")
			if got := recordedVersion(t, child); got != tc.wantVersion {
				t.Errorf("child recorded version %q, want %q", got, tc.wantVersion)
			}
			if child.AgentDefID != tc.wantPin {
				t.Errorf("child agent_def_id = %q, want %q", child.AgentDefID, tc.wantPin)
			}
		})
	}
}
