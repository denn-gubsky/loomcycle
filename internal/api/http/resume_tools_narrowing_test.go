package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// offeredToolNames is the tool set a provider request offered the model.
func offeredToolNames(req providers.Request) []string {
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// startAndResumeWithTools starts a run of an agent holding Alpha and Beta with
// the request's own `tools` field (toolsJSON, "" = none sent), parks it, resumes
// it, and returns the tools offered on the original call and the resumed one.
func startAndResumeWithTools(t *testing.T, toolsJSON string) (orig, resumed []string) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{"Alpha", "Beta"}, SystemPrompt: "you work"},
	}
	prov := &recordingScriptedProvider{defaultS: endTurn()}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "runcfg_tools.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov},
		[]tools.Tool{namedStubTool{name: "Alpha"}, namedStubTool{name: "Beta"}},
		concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	field := ""
	if toolsJSON != "" {
		field = `"tools":` + toolsJSON + `,`
	}
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"worker",`+field+`"segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`,
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

	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	reqs := prov.waitForRequests(t, 2)
	return offeredToolNames(reqs[0]), offeredToolNames(reqs[1])
}

// A run whose caller narrowed its tools resumes with that narrowing: a pause,
// a snapshot restore or a boot recovery must not hand it the agent's full set.
func TestResumedRun_KeepsItsCallerToolsNarrowing(t *testing.T) {
	orig, resumed := startAndResumeWithTools(t, `["Alpha"]`)
	if want := []string{"Alpha"}; !reflect.DeepEqual(orig, want) {
		t.Fatalf("original run offered %v, want %v (fixture)", orig, want)
	}
	if !reflect.DeepEqual(resumed, orig) {
		t.Errorf("resumed run offered %v, want the caller's narrowing %v", resumed, orig)
	}
}

// An explicit empty list resumes as it started. Over HTTP an empty `tools`
// reads as no list at all (the request's omitempty drops it), so the run starts
// on the agent's set and must resume on it — the record must not turn the empty
// list into a narrowing to nothing, nor the reverse (the round-trip test below
// pins the record's own three states).
func TestResumedRun_KeepsAnEmptyCallerToolsListAsItStarted(t *testing.T) {
	orig, resumed := startAndResumeWithTools(t, `[]`)
	if !reflect.DeepEqual(resumed, orig) {
		t.Errorf("resumed run offered %v, want what it started with %v", resumed, orig)
	}
}

// No caller list means the agent's own set, before and after the resume.
func TestResumedRun_WithNoCallerToolsKeepsTheAgentsSet(t *testing.T) {
	orig, resumed := startAndResumeWithTools(t, "")
	if want := []string{"Alpha", "Beta"}; !reflect.DeepEqual(orig, want) || !reflect.DeepEqual(resumed, want) {
		t.Errorf("offered %v then %v, want %v both times", orig, resumed, want)
	}
}

// The record must keep "no list", "empty list" and "these names" apart through
// the JSON round trip the runs.run_config column puts it through.
func TestRunConfigRecord_CallerToolsSurviveTheRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"absent", nil, nil},
		{"empty", []string{}, []string{}},
		{"names", []string{"Alpha"}, []string{"Alpha"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := runConfigRecord{Tools: toolsRecordOf(tc.in)}.marshal()
			var back runConfigRecord
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			got := back.callerTools()
			if (got == nil) != (tc.want == nil) || !reflect.DeepEqual(append([]string{}, got...), append([]string{}, tc.want...)) {
				t.Errorf("callerTools() = %#v after %s, want %#v", got, raw, tc.want)
			}
		})
	}
}
