package http

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
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
