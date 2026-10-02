package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// structuredMemberServer is outputFormatServer with its agent held to an
// output_format, so a member run ends with a structured result.
func structuredMemberServer(t *testing.T) *Server {
	t.Helper()
	srv, _, _, _ := outputFormatServer(t)
	cfg := srv.cfg()
	ag := cfg.Agents["agent"]
	ag.OutputFormat = &config.OutputFormat{Type: config.OutputFormatJSONSchema, Name: "verdict",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string"}}}}
	cfg.Agents["agent"] = ag
	return srv
}

// A team member reports its bare answer and its structured result beside the
// headered Output the walk threads, which is unchanged.
func TestTeamMember_ReportsItsBareAnswerAndStructuredResult(t *testing.T) {
	srv := structuredMemberServer(t)
	res, err := srv.runTeamMember(context.Background(), "agent", teamrun.Prompt{Input: "go"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != `{"verdict":"ok"}` {
		t.Errorf("FinalText = %q, want the bare answer", res.FinalText)
	}
	if res.Structured["verdict"] != "ok" {
		t.Errorf("Structured = %v, want verdict=ok", res.Structured)
	}
	if !strings.HasPrefix(res.Output, "[sub-agent agent_id=") || !strings.HasSuffix(res.Output, "]\n"+`{"verdict":"ok"}`) {
		t.Errorf("Output = %q, want the answer under the attribution header", res.Output)
	}
}

// sinkCapture is a ChannelIO holding one inbound message and recording what
// the Starter publishes.
type sinkCapture struct {
	mu        sync.Mutex
	published []json.RawMessage
}

func (c *sinkCapture) Read(context.Context, string, int, int, int) ([]teamrun.ChannelMessage, string, error) {
	return []teamrun.ChannelMessage{{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)}}, "cur", nil
}
func (c *sinkCapture) Ack(context.Context, string, string) error { return nil }
func (c *sinkCapture) Publish(_ context.Context, _ string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.published = append(c.published, payload)
	return nil
}

// Through the real member runner: a Starter's sink message carries the
// member's bare answer and its structured result, while the Starter's own
// output — the envelope a consolidator reads — keeps the header.
func TestTeamWalk_StarterSinkCarriesTheMembersBareAnswerAndStructured(t *testing.T) {
	srv := structuredMemberServer(t)
	ch := &sinkCapture{}
	r := teamrun.NewAgentRunner(srv.runTeamMember, teamrun.WithChannels(ch))
	st := teamgraph.State{ID: "wave", Handler: teamgraph.Handler{
		Kind:   teamgraph.HandlerStarter,
		Source: &teamgraph.StarterSource{Channel: "in"},
		Fanout: &teamgraph.StarterFanout{Agent: "agent", Per: teamgraph.FanoutPerMessage, Max: 1},
		Prompt: &teamgraph.StarterPrompt{Input: teamrun.StarterMessageSlot},
		Sink:   &teamgraph.StarterSink{Channel: "out"},
	}}
	out, err := r.RunHandler(context.Background(), st, &teamrun.Task{Input: "go", WalkID: "wlk_sink"})
	if err != nil {
		t.Fatalf("starter: %v", err)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.published) != 1 {
		t.Fatalf("published %d sink messages, want 1", len(ch.published))
	}
	var msg teamrun.SinkMessage
	if err := json.Unmarshal(ch.published[0], &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Status != teamrun.SinkOK || msg.Output != `{"verdict":"ok"}` {
		t.Errorf("sink message = %s, want status ok and the bare answer", ch.published[0])
	}
	if msg.Structured["verdict"] != "ok" {
		t.Errorf("sink structured = %v, want verdict=ok", msg.Structured)
	}
	if !strings.Contains(out.Output, `"output":"[sub-agent agent_id=`) {
		t.Errorf("envelope = %s, want the member's headered output", out.Output)
	}
}

// Through the real member runner and the real review verb: a member a person
// rejected publishes the answer they turned down bare, as a successful one
// does, so a downstream reader binds it with the same `$.output`.
func TestTeamWalk_RejectedMemberSinkCarriesTheBareAnswer(t *testing.T) {
	h := newReviewHarness(t)
	runID, msg := rejectHeldMember(t, h)
	if msg.Status != teamrun.SinkRejected || msg.RunID != runID || msg.Error != "rejected by the reviewer" {
		t.Errorf("sink message = %+v, want status rejected for run %s", msg, runID)
	}
	if msg.Output != "answer 1" {
		t.Errorf("sink output = %q, want the rejected answer bare", msg.Output)
	}
}

// holdWriterToFormat gives the review harness's writer an output_format.
func holdWriterToFormat(h *reviewHarness) {
	cfg := h.srv.cfg()
	ag := cfg.Agents["writer"]
	ag.OutputFormat = &config.OutputFormat{Type: config.OutputFormatJSONSchema, Name: "verdict",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string"}}}}
	cfg.Agents["writer"] = ag
}

// A rejected member held to an output_format carries its structured result
// on the sink message and on its run row, as a successful one does.
func TestTeamWalk_RejectedMemberSinkCarriesItsStructuredResult(t *testing.T) {
	h := newReviewHarness(t)
	holdWriterToFormat(h)
	h.prov.answer = `{"verdict":"no"}`
	runID, msg := rejectHeldMember(t, h)
	if msg.Status != teamrun.SinkRejected || msg.Output != `{"verdict":"no"}` {
		t.Errorf("sink message = %+v, want status rejected and the bare answer", msg)
	}
	if msg.Structured["verdict"] != "no" {
		t.Errorf("sink structured = %v, want verdict=no", msg.Structured)
	}
	run, err := h.st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var rec runResultRecord
	if err := json.Unmarshal(run.Result, &rec); err != nil {
		t.Fatalf("run result %s: %v", run.Result, err)
	}
	if run.Status != store.RunRejected || rec.Structured["verdict"] != "no" {
		t.Errorf("run row = %s with result %s, want rejected with verdict=no", run.Status, run.Result)
	}
}

// A rejected member whose answer does not parse against its output_format
// stays rejected, with no structured result.
func TestTeamWalk_AnUnparseableRejectedMemberStaysRejected(t *testing.T) {
	h := newReviewHarness(t)
	holdWriterToFormat(h)
	runID, msg := rejectHeldMember(t, h)
	if msg.Status != teamrun.SinkRejected || msg.Output != "answer 1" || msg.Structured != nil {
		t.Errorf("sink message = %+v, want status rejected, the bare answer, no structured", msg)
	}
	if run, _ := h.st.GetRun(context.Background(), runID); run.Status != store.RunRejected {
		t.Errorf("run status = %s, want rejected", run.Status)
	}
}

// rejectHeldMember runs a one-member Starter whose member is armed for review,
// rejects the held member through the real review verb, and returns its run id
// and the one sink message the Starter published.
func rejectHeldMember(t *testing.T, h *reviewHarness) (string, teamrun.SinkMessage) {
	t.Helper()
	ch := &sinkCapture{}
	// The arming a walk's member_review would put on the member's ctx; the
	// user is what lets the test find the held run.
	spawn := func(ctx context.Context, name string, p teamrun.Prompt, defID string) (teamrun.SpawnResult, error) {
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "u1"})
		ctx = teamrun.WithReviewArming(ctx, func(context.Context) bool { return true })
		return h.srv.runTeamMember(ctx, name, p, defID)
	}
	r := teamrun.NewAgentRunner(spawn, teamrun.WithChannels(ch))
	st := teamgraph.State{ID: "wave", Handler: teamgraph.Handler{
		Kind:   teamgraph.HandlerStarter,
		Source: &teamgraph.StarterSource{Channel: "in"},
		Fanout: &teamgraph.StarterFanout{Agent: "writer", Per: teamgraph.FanoutPerMessage, Max: 1},
		Prompt: &teamgraph.StarterPrompt{Input: teamrun.StarterMessageSlot},
		Sink:   &teamgraph.StarterSink{Channel: "out"},
	}}
	done := make(chan error, 1)
	go func() {
		_, err := r.RunHandler(context.Background(), st, &teamrun.Task{Input: "go", WalkID: "wlk_reject"})
		done <- err
	}()

	var runID string
	for deadline := time.Now().Add(3 * time.Second); runID == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		runs, _ := h.st.ListActiveRunsByUser(context.Background(), "", "u1", store.RunRunning)
		for _, run := range runs {
			if heldForReview(context.Background(), h.st, run.ID) {
				runID = run.ID
			}
		}
	}
	if runID == "" {
		t.Fatal("the armed member was never held")
	}
	if code, body := h.review(runID, `{"decision":"reject"}`); code != http.StatusOK {
		t.Fatalf("reject = %d %s", code, body)
	}
	select {
	case err := <-done:
		// A rejected member does not count toward wait=all.
		if err == nil {
			t.Error("a wave whose only member was rejected must fail the state")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the starter never returned")
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.published) != 1 {
		t.Fatalf("published %d sink messages, want 1", len(ch.published))
	}
	var msg teamrun.SinkMessage
	if err := json.Unmarshal(ch.published[0], &msg); err != nil {
		t.Fatal(err)
	}
	return runID, msg
}
