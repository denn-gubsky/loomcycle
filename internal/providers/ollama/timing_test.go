package ollama

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// The fixture is a real /api/chat stream recorded from qwen3.6 on Ollama, so
// the field names and units are the server's, not a guess at them.
func TestStream_DoneFrameDurationsReachTheUsageTiming(t *testing.T) {
	raw, err := os.ReadFile("testdata/chat_stream_qwen36.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var frames []string
	for _, l := range strings.SplitAfter(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			frames = append(frames, l)
		}
	}
	srv := fakeStream(t, frames)
	defer srv.Close()

	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "qwen3.6:latest",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var usage *providers.Usage
	for ev := range ch {
		if ev.Type == providers.EventDone {
			usage = ev.Usage
		}
	}
	if usage == nil || usage.Timing == nil {
		t.Fatalf("done usage carries no timing: %+v", usage)
	}
	// total_duration 606264261, load_duration 1564251, prompt_eval_duration
	// 322120000, eval_duration 238569000 (ns) → whole milliseconds.
	want := providers.CallTiming{LoadMs: 1, PrefillMs: 322, DecodeMs: 238, ServerTotalMs: 606}
	if *usage.Timing != want {
		t.Fatalf("timing = %+v, want %+v", *usage.Timing, want)
	}
	if usage.InputTokens != 19 || usage.OutputTokens != 6 {
		t.Fatalf("tokens = %d in / %d out, want 19 / 6", usage.InputTokens, usage.OutputTokens)
	}
}

// A done frame from a server that reports no durations must not invent a
// zero-valued timing — 0 would read as "measured, and instant".
func TestStream_NoDurationsMeansNoTiming(t *testing.T) {
	srv := fakeStream(t, []string{
		`{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":1}` + "\n",
	})
	defer srv.Close()
	d := New("", "", srv.URL, streamhttp.Options{}, nil)
	ch, err := d.Call(context.Background(), providers.Request{Model: "m",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		if ev.Type == providers.EventDone && ev.Usage != nil && ev.Usage.Timing != nil {
			t.Fatalf("timing invented: %+v", ev.Usage.Timing)
		}
	}
}
