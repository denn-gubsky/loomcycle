package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// transcribeProbe stands in for a File tool call that ran OCR: it reports the
// pass through the run's event emitter, the way the transcribe path does, and
// returns a result that does not mention it.
type transcribeProbe struct{ info providers.TranscriptionInfo }

func (p *transcribeProbe) Name() string                 { return "transcribe_probe" }
func (p *transcribeProbe) Description() string          { return "" }
func (p *transcribeProbe) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (p *transcribeProbe) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	info := p.info
	tools.EventEmitter(ctx)(providers.Event{Type: providers.EventTranscription, Transcription: &info})
	return tools.Result{Text: "ok"}, nil
}

// GUARD, not a fail-before: the run path persists, forwards and skips on replay
// by event type generically, so a transcription event needed no server change.
// This pins that it stays so — a store-only filter, a replay case or an SSE
// allowlist added later must keep the transcription record reaching the
// operator and never reaching the model.
//
// One tool call reports one pass through the real POST /v1/runs path: the SSE
// stream carries it with its payload, GET /v1/sessions/{id}/transcript returns
// it typed, and rebuilding the conversation for a resume yields exactly what it
// yields without the row.
func TestRunPath_TranscriptionEventIsRecordedStreamedAndNeverReplayed(t *testing.T) {
	cost := 0.04
	info := providers.TranscriptionInfo{
		Ref: "volume:inbox:documents/scan.pdf", Out: "volume:inbox:documents/scan.ocr.md",
		Purpose: "ocr", Engine: "vision", Provider: "anthropic", Model: "claude-haiku-5-5",
		Pages: 12, InputTokens: 9000, OutputTokens: 4000, Cost: &cost, Currency: "USD", Status: "ok", RunID: "run-x",
	}
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model", Tools: []string{"transcribe_probe"}, SystemPrompt: "read the scan"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	prov := &scriptedProvider{scripts: [][]providers.Event{
		{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu1", Name: "transcribe_probe", Input: []byte(`{}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
		},
		{
			{Type: providers.EventText, Text: "read it"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "transcription.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{&transcribeProbe{info: info}}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}

	// SSE: one transcription frame, carrying the record as the Go struct has it.
	var streamed []map[string]any
	for _, f := range readFrames(t, strings.NewReader(string(body))) {
		if f.Event == "transcription" {
			streamed = append(streamed, f.Data)
		}
	}
	if len(streamed) != 1 {
		t.Fatalf("SSE carried %d transcription frames, want 1:\n%s", len(streamed), body)
	}
	raw, _ := json.Marshal(streamed[0]["transcription"])
	var onWire providers.TranscriptionInfo
	if err := json.Unmarshal(raw, &onWire); err != nil || !reflect.DeepEqual(onWire, info) {
		t.Errorf("SSE transcription = %s (%v), want %+v", raw, err, info)
	}

	// Transcript read: the row is there, typed.
	sessionID := extractSessionID(string(body))
	tResp, err := http.Get(ts.URL + "/v1/sessions/" + sessionID + "/transcript")
	if err != nil {
		t.Fatal(err)
	}
	var tr transcriptResponse
	err = json.NewDecoder(tResp.Body).Decode(&tr)
	_ = tResp.Body.Close()
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	var rows int
	for _, ev := range tr.Events {
		if ev.Type != "transcription" {
			continue
		}
		rows++
		if ev.Event.Transcription == nil || !reflect.DeepEqual(*ev.Event.Transcription, info) {
			t.Errorf("transcript row = %+v, want %+v", ev.Event.Transcription, info)
		}
	}
	if rows != 1 {
		t.Errorf("transcript has %d transcription rows, want 1", rows)
	}

	// Resume: the rebuilt conversation is the one without the row. The model
	// saw the tool's own result, never the record.
	stored, err := st.GetTranscript(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var without []store.Event
	for _, ev := range stored {
		if ev.Type != "transcription" {
			without = append(without, ev)
		}
	}
	if len(without) == len(stored) {
		t.Fatal("no transcription row persisted — the replay check below would compare nothing")
	}
	got, want := replayTranscript(stored), replayTranscript(without)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replay changed with the transcription row:\n got %+v\nwant %+v", got, want)
	}
	if msgs, _ := json.Marshal(got); strings.Contains(string(msgs), "scan.ocr.md") {
		t.Errorf("the replayed conversation carries the transcription record: %s", msgs)
	}
}
