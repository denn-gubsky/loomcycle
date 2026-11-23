package http

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A tool's pictures are kept on the stored tool_result (never on the live
// event), a rebuilt conversation carries them back to the model, and a
// no-trace result keeps none.
func TestToolImages_StoredBesideTheEventAndReplayed(t *testing.T) {
	srv, _ := makeServer(t, &childRouter{}, noTraceConfig())
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "parent", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_p", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var forwarded []providers.Event
	emit := srv.makeRecordingEmit(ctx, run.ID, tools.RunIdentityValue{AgentID: "a_p", UserID: "alice"}, sess.ID, runStateMeta{},
		func(ev providers.Event) { forwarded = append(forwarded, ev) })
	const data = "UElDVFVSRS1CWVRFUw=="
	pics := []providers.ImagePart{{MediaType: "image/png", Data: data}}
	for _, c := range []struct {
		id          string
		notRetained bool
	}{{"tu_pic", false}, {"tu_secret", true}} {
		emit(providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: c.id, Name: "Pic", Input: json.RawMessage(`{}`)}})
		emit(providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: c.id, Name: "Pic"},
			Text: "a chart", ToolImages: pics, ContentNotRetained: c.notRetained})
	}

	// The live frame is the event as SSE marshals it: no picture bytes.
	for _, ev := range forwarded {
		frame, _ := json.Marshal(ev)
		if bytes.Contains(frame, []byte(data)) {
			t.Fatalf("the live %s frame carries the picture: %s", ev.Type, frame)
		}
	}
	events, err := srv.store.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]providers.ContentBlock{}
	for _, m := range replayTranscript(events) {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				got[b.ToolUseID] = b
			}
		}
	}
	if b := got["tu_pic"]; len(b.Images) != 1 || b.Images[0] != pics[0] || b.Text != "a chart" {
		t.Errorf("replayed result = %+v, want the text and the picture back", b)
	}
	if b := got["tu_secret"]; len(b.Images) != 0 {
		t.Errorf("a no-trace result kept its picture: %+v", b)
	}
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), "tu_secret") && bytes.Contains(ev.Payload, []byte(data)) {
			t.Errorf("the no-trace result's picture is stored: %s", ev.Payload)
		}
	}
}

// bigPictureTool returns a picture over the edge.
type bigPictureTool struct{ data []byte }

func (bigPictureTool) Name() string                 { return "BigPic" }
func (bigPictureTool) Description() string          { return "" }
func (bigPictureTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (b bigPictureTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Images: []tools.Image{{MediaType: "image/png", Data: b.data}}}, nil
}

// A run's dispatcher scales a tool's picture with the server's re-encoder; it
// is not refused for want of one.
func TestToolImages_RunDispatcherScalesWithTheServersReencoder(t *testing.T) {
	srv, _ := makeServer(t, &childRouter{}, noTraceConfig())
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2000, 500))); err != nil {
		t.Fatal(err)
	}
	tool := bigPictureTool{data: b.Bytes()}
	r := srv.newDispatcher([]tools.Tool{tool}, nil).Execute(context.Background(), "BigPic", json.RawMessage(`{}`))
	if r.IsError || len(r.Images) != 1 {
		t.Fatalf("result = %q (error %v)", r.Text, r.IsError)
	}
	if im := r.Images[0]; im.Width != tools.MaxImageEdge || im.Height != 392 {
		t.Errorf("sent at %d×%d, want %d×392", im.Width, im.Height, tools.MaxImageEdge)
	}
}
