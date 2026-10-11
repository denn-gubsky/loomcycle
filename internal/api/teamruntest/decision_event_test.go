package teamruntest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A decision state's answer is on the walk's own run.
//
// A client that starts a walk detached holds a run id and nothing else: the
// start returns no steps, and a decision state starts no member run. These
// drive the read that client makes, GET /v1/runs/{run_id}/stream on the walk's
// run, while the walk is still running and after it has ended.

// frame is one SSE frame of a run's stream: its event name and its data.
type frame struct {
	Event string
	Data  map[string]any
}

// streamFrames reads the walk run's stream from the start and sends each
// frame until the stream ends or ctx does.
func (e *decisionEnv) streamFrames(ctx context.Context, runID string) <-chan frame {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+"/v1/runs/"+runID+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		e.t.Fatalf("stream of %s = %d: %s", runID, resp.StatusCode, raw)
	}
	out := make(chan frame, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var f frame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f.Data)
			case line == "" && f.Event != "":
				out <- f
				f = frame{}
			}
		}
	}()
	return out
}

// decisionsOf drains a stream and returns its team_decision payloads.
func decisionsOf(frames <-chan frame) []map[string]any {
	var out []map[string]any
	for f := range frames {
		if f.Event == "team_decision" {
			d, _ := f.Data["team_decision"].(map[string]any)
			out = append(out, d)
		}
	}
	return out
}

// wantTriageDecision checks one decision payload against the triage team's
// walk: the second state visited, routed to the billing desk, with the
// model's whole answer.
func wantTriageDecision(t *testing.T, d map[string]any) {
	t.Helper()
	if d["state"] != "triage" || d["visit"] != float64(2) || d["edge"] != "conditional:billing" || d["next"] != "billing-desk" {
		t.Errorf("the decision = %v, want triage, visit 2, conditional:billing to billing-desk", d)
	}
	answer, _ := d["answer"].(map[string]any)
	answers, _ := answer["answers"].(map[string]any)
	route, _ := answers["route"].(map[string]any)
	probs, _ := route["probabilities"].(map[string]any)
	urgent, _ := answers["urgent"].(map[string]any)
	detail, _ := answers["detail"].(map[string]any)
	if answer["model"] != "decide" || route["choice"] != "billing" || probs["billing"] != 0.91 || urgent["noul"] != 0.4 || detail["score"] != 1.5 {
		t.Errorf("the decision's answer = %v, want the model, the option's probabilities and the unrouted answers", d["answer"])
	}
}

func (e *decisionEnv) startDetached() string {
	e.t.Helper()
	status, out, raw := e.teamdef(`{"op":"run","name":"triage","mode":"detach","input":` + jsonString(ticket) + `}`)
	runID, _ := out["run_id"].(string)
	if status != http.StatusOK || runID == "" || out["steps"] != nil {
		e.t.Fatalf("detached run = %d: %s, want a run id and no steps", status, raw)
	}
	return runID
}

func (e *decisionEnv) runStatus(runID string) store.RunStatus {
	e.t.Helper()
	run, err := e.st.GetRun(context.Background(), runID)
	if err != nil {
		e.t.Fatal(err)
	}
	return run.Status
}

// While the walk is still running, with the desk the decision routed to not
// yet answered, the walk's stream already carries the decision.
func TestDecisionEvent_IsOnTheWalksStreamWhileTheWalkRuns(t *testing.T) {
	e := newDecisionEnv(t, true)
	gate := make(chan struct{})
	e.desks.gate = gate
	if status, raw := e.create(triageOverlay); status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, raw)
	}
	runID := e.startDetached()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	frames := e.streamFrames(ctx, runID)
	var seen map[string]any
	for f := range frames {
		if f.Event == "team_decision" {
			seen, _ = f.Data["team_decision"].(map[string]any)
			break
		}
	}
	if seen == nil {
		t.Fatal("the walk's stream ended, or timed out, with no team_decision")
	}
	if st := e.runStatus(runID); st != store.RunRunning {
		t.Fatalf("the walk was %s when its decision arrived, want it still running", st)
	}
	wantTriageDecision(t, seen)

	close(gate)
	for range frames { // the stream ends when the walk does
	}
	if st := e.runStatus(runID); st != store.RunCompleted {
		t.Errorf("the walk ended %s, want completed", st)
	}
}

// After the walk has ended, a client that opens the walk's run reads the
// decision from the same stream, and from the run's transcript.
func TestDecisionEvent_IsReadFromTheWalksRunAfterItHasEnded(t *testing.T) {
	e := newDecisionEnv(t, true)
	if status, raw := e.create(triageOverlay); status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, raw)
	}
	runID := e.startDetached()
	deadline := time.Now().Add(20 * time.Second)
	for e.runStatus(runID) == store.RunRunning {
		if time.Now().After(deadline) {
			t.Fatal("the walk did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := decisionsOf(e.streamFrames(ctx, runID))
	if len(got) != 1 {
		t.Fatalf("the ended walk's stream carries %d decisions, want 1", len(got))
	}
	wantTriageDecision(t, got[0])

	run, err := e.st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(e.ts.URL + "/v1/sessions/" + run.SessionID + "/transcript")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var transcript struct {
		Events []struct {
			Type  string `json:"type"`
			Event struct {
				TeamDecision map[string]any `json:"team_decision"`
			} `json:"event"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &transcript); err != nil {
		t.Fatalf("transcript = %d: %s", resp.StatusCode, raw)
	}
	var inTranscript []map[string]any
	for _, ev := range transcript.Events {
		if ev.Type == "team_decision" {
			inTranscript = append(inTranscript, ev.Event.TeamDecision)
		}
	}
	if len(inTranscript) != 1 {
		t.Fatalf("the walk's transcript carries %d decisions, want 1: %s", len(inTranscript), raw)
	}
	wantTriageDecision(t, inTranscript[0])

	// The desk's own run names the visit after the decision's, so the two
	// sort in the order the walk made them.
	resp, err = http.Get(e.ts.URL + "/v1/runs?walk_id=" + runID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var members struct {
		Agents []struct {
			ParentContext *store.ParentContext `json:"parent_context"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("the walk's runs = %d: %s", resp.StatusCode, raw)
	}
	var deskVisit int
	for _, m := range members.Agents {
		if m.ParentContext != nil && m.ParentContext.State == "billing-desk" {
			deskVisit = m.ParentContext.StateVisit
		}
	}
	if deskVisit != 3 {
		t.Errorf("the billing desk's run carries state_visit %d, want 3, the visit after the decision's 2", deskVisit)
	}
}

// A walk the caller waited for records the decision too: what is on the
// run does not depend on how the walk was started.
func TestDecisionEvent_AWaitedForWalkRecordsItToo(t *testing.T) {
	e := newDecisionEnv(t, true)
	if status, raw := e.create(triageOverlay); status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, raw)
	}
	status, out, raw := e.teamdef(`{"op":"run","name":"triage","input":` + jsonString(ticket) + `}`)
	runID, _ := out["run_id"].(string)
	if status != http.StatusOK || runID == "" {
		t.Fatalf("run = %d: %s", status, raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := decisionsOf(e.streamFrames(ctx, runID))
	if len(got) != 1 {
		t.Fatalf("the walk's stream carries %d decisions, want 1", len(got))
	}
	wantTriageDecision(t, got[0])
}

// A decision whose call failed is not on the record as a decision: the walk's
// failure names the state and the code.
func TestDecisionEvent_AFailedCallRecordsNoDecision(t *testing.T) {
	e := newDecisionEnv(t, true)
	if status, raw := e.create(triageOverlay); status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, raw)
	}
	e.model.mu.Lock()
	e.model.status = http.StatusInternalServerError
	e.model.mu.Unlock()
	runID := e.startDetached()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if got := decisionsOf(e.streamFrames(ctx, runID)); len(got) != 0 {
		t.Errorf("a failed decision left %d decisions on the walk's run: %v", len(got), got)
	}
	run, err := e.st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunFailed || !strings.Contains(run.ErrorMsg, `state "triage"`) || !strings.Contains(run.ErrorMsg, "call_failed") {
		t.Errorf("the walk's run = %s %q, want failed at triage with call_failed", run.Status, run.ErrorMsg)
	}
}
