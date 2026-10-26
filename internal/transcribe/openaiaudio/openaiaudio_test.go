package openaiaudio

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// fakeEndpoint is an httptest double of /v1/audio/transcriptions: it records
// the form of each request and answers with reply(form).
type fakeEndpoint struct {
	srv   *httptest.Server
	mu    sync.Mutex
	forms []map[string]string
}

func newEndpoint(t *testing.T, reply func(form map[string]string) string) *fakeEndpoint {
	t.Helper()
	f := &fakeEndpoint{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		form := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			form[k] = v[0]
		}
		if fh, h, err := r.FormFile("file"); err == nil {
			b, _ := io.ReadAll(fh)
			form["file"] = string(b)
			form["filename"] = h.Filename
		}
		f.mu.Lock()
		f.forms = append(f.forms, form)
		f.mu.Unlock()
		_, _ = io.WriteString(w, reply(form))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// wrapped stands for the registry's lane wrapper: the driver is reachable
// only through Unwrap.
type wrapped struct{ providers.Provider }

func (w wrapped) Unwrap() providers.Provider { return w.Provider }

func engineFor(t *testing.T, ep *fakeEndpoint, model string, cfg config.STTConfig) *Engine {
	t.Helper()
	d := openai.New("", ep.srv.URL+"/v1", streamhttp.Options{}, ep.srv.Client())
	cfg.Engine = config.STTEngineOpenAIAudio
	e, err := New(Options{
		Config:   cfg,
		Resolve:  func(context.Context) (string, string, error) { return "whisper-local", model, nil },
		Provider: func(string) (providers.Provider, error) { return wrapped{d}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func unit(t *testing.T, data string, mt string, span time.Duration) transcribe.Unit {
	t.Helper()
	u, err := transcribe.NewUnit(bytes.NewReader([]byte(data)), 0, int64(len(data)), mt)
	if err != nil {
		t.Fatal(err)
	}
	u.Span = transcribe.TimeRange{End: span}
	return u
}

// TestOpenAIAudio_WhisperGetsVerboseSegments — a whisper model is asked for
// verbose_json with the language and the vocabulary as its prompt, and its
// timed segments come back in order.
func TestOpenAIAudio_WhisperGetsVerboseSegments(t *testing.T) {
	ep := newEndpoint(t, func(map[string]string) string {
		return `{"text":"добрий день. почнемо","language":"ukrainian","duration":12.5,
			"segments":[{"start":0.0,"end":4.2,"text":" Добрий день."},{"start":4.2,"end":12.5,"text":" Почнемо засідання."},{"start":12.5,"end":12.5,"text":"  "}]}`
	})
	e := engineFor(t, ep, "Systran/faster-whisper-large-v3", config.STTConfig{})
	plan, err := e.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Diarizes || plan.MaxBytes != MaxUploadBytes || plan.Provider != "whisper-local" {
		t.Errorf("plan = %+v", plan)
	}
	res, err := e.Transcribe(context.Background(), plan, unit(t, "RIFF....", "audio/wav", 13*time.Second),
		transcribe.STTOptions{Language: "uk", Vocabulary: "Обленерго, Петренко"})
	if err != nil {
		t.Fatal(err)
	}
	f := ep.forms[0]
	if f["response_format"] != "verbose_json" || f["language"] != "uk" || f["prompt"] != "Обленерго, Петренко" ||
		f["filename"] != "audio.wav" || f["file"] != "RIFF...." || f["model"] != "Systran/faster-whisper-large-v3" {
		t.Errorf("form = %v", f)
	}
	if _, ok := f["chunking_strategy"]; ok {
		t.Error("a whisper request carried chunking_strategy")
	}
	if len(res.Segments) != 2 || res.Segments[1].Start != 4200*time.Millisecond || res.Segments[1].Text != "Почнемо засідання." {
		t.Errorf("segments = %+v", res.Segments)
	}
	if res.Duration != 12500*time.Millisecond || !res.Reached || res.Language != "ukrainian" {
		t.Errorf("result = %+v", res)
	}
}

// TestOpenAIAudio_DiarizeModelGetsSpeakerSegments — a diarizing model is asked
// for diarized_json with automatic chunking and no prompt (it takes none), and
// the speaker labels come back on the segments; off asks for plain json.
func TestOpenAIAudio_DiarizeModelGetsSpeakerSegments(t *testing.T) {
	ep := newEndpoint(t, func(form map[string]string) string {
		if form["response_format"] == "json" {
			return `{"text":"все разом","usage":{"type":"tokens","input_tokens":120,"output_tokens":9}}`
		}
		return `{"text":"…","segments":[{"type":"transcript.text.segment","start":0.5,"end":3,"text":"Доповідаю.","speaker":"A"},
			{"start":3.1,"end":5,"text":"Дякую.","speaker":"B"}],"usage":{"type":"tokens","input_tokens":300,"output_tokens":20}}`
	})
	e := engineFor(t, ep, "gpt-4o-transcribe-diarize", config.STTConfig{})
	plan, _ := e.Plan(context.Background())
	if !plan.Diarizes {
		t.Fatal("a -diarize model must plan as diarizing")
	}
	res, err := e.Transcribe(context.Background(), plan, unit(t, "ID3x", "audio/mpeg", 5*time.Second),
		transcribe.STTOptions{Language: "uk", Vocabulary: "names", Diarize: transcribe.DiarizeAuto})
	if err != nil {
		t.Fatal(err)
	}
	f := ep.forms[0]
	if f["response_format"] != "diarized_json" || f["chunking_strategy"] != "auto" || f["filename"] != "audio.mp3" {
		t.Errorf("form = %v", f)
	}
	if _, ok := f["prompt"]; ok {
		t.Error("the diarizing model was sent a prompt")
	}
	if len(res.Segments) != 2 || res.Segments[0].Speaker != "A" || res.Segments[1].Speaker != "B" || res.InputTokens != 300 {
		t.Errorf("result = %+v", res)
	}

	res, err = e.Transcribe(context.Background(), plan, unit(t, "ID3x", "audio/mpeg", 5*time.Second),
		transcribe.STTOptions{Language: "uk", Diarize: transcribe.DiarizeOff})
	if err != nil {
		t.Fatal(err)
	}
	if ep.forms[1]["response_format"] != "json" {
		t.Errorf("diarize off: format %q", ep.forms[1]["response_format"])
	}
	// No segments: the text is one segment spanning the unit.
	if len(res.Segments) != 1 || res.Segments[0].End != 5*time.Second || res.Segments[0].Text != "все разом" || res.OutputTokens != 9 {
		t.Errorf("json result = %+v", res)
	}
}

// TestOpenAIAudio_ClampsMalformedTimes — negative, inverted and absurd times in
// an answer do not escape the unit's range.
func TestOpenAIAudio_ClampsMalformedTimes(t *testing.T) {
	var res transcribe.STTResult
	err := parse([]byte(`{"segments":[{"start":-5,"end":-1,"text":"a"},{"start":9,"end":3,"text":"b"},{"start":1e12,"end":1e13,"text":"c"}]}`), time.Minute, &res)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Segments
	if len(s) != 3 || s[0].Start != 0 || s[0].End != 0 || s[1].End != s[1].Start || s[2].Start != maxSegmentSeconds*time.Second {
		t.Errorf("segments = %+v", s)
	}
	var billed transcribe.STTResult
	if err := parse([]byte(`{"text":"x","usage":{"type":"duration","seconds":42}}`), time.Minute, &billed); err != nil || billed.Duration != 42*time.Second {
		t.Errorf("usage seconds: %v %v", billed.Duration, err)
	}
	if err := parse([]byte(`<html>`), time.Minute, &res); err == nil {
		t.Error("a non-JSON answer must be an error")
	}
}

// TestOpenAIAudio_RefusesWhatTheEndpointCannotTake — a type the endpoint does
// not read, a unit over the cap and a provider without the endpoint are
// refused before any request.
func TestOpenAIAudio_RefusesWhatTheEndpointCannotTake(t *testing.T) {
	ep := newEndpoint(t, func(map[string]string) string { return `{"text":"x"}` })
	e := engineFor(t, ep, "whisper-1", config.STTConfig{})
	plan, _ := e.Plan(context.Background())
	if _, err := e.Transcribe(context.Background(), plan, unit(t, "x", "audio/aiff", time.Second), transcribe.STTOptions{}); err == nil {
		t.Error("audio/aiff was sent")
	}
	small := plan
	small.MaxBytes = 2
	if res, err := e.Transcribe(context.Background(), small, unit(t, "xyz", "audio/wav", time.Second), transcribe.STTOptions{}); err == nil || res.Reached {
		t.Errorf("an over-cap unit: err %v reached %v", err, res.Reached)
	}
	if len(ep.forms) != 0 {
		t.Errorf("%d requests were made", len(ep.forms))
	}

	chatOnly := &Engine{
		resolve:  func(context.Context) (string, string, error) { return "anthropic", "claude-x", nil },
		provider: func(string) (providers.Provider, error) { return noAudio{}, nil },
		timeout:  time.Minute,
	}
	if _, err := chatOnly.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "transcription.stt.provider") {
		t.Errorf("plan on a chat-only provider: %v", err)
	}
}

// TestOpenAIAudio_TimeoutNamesTheSetting — a request over timeout_s says which
// setting bounds it.
func TestOpenAIAudio_TimeoutNamesTheSetting(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	d := openai.New("", srv.URL, streamhttp.Options{}, srv.Client())
	e := &Engine{
		resolve:  func(context.Context) (string, string, error) { return "w", "whisper-1", nil },
		provider: func(string) (providers.Provider, error) { return d, nil },
		timeout:  100 * time.Millisecond,
	}
	plan, _ := e.Plan(context.Background())
	res, err := e.Transcribe(context.Background(), plan, unit(t, "x", "audio/wav", time.Second), transcribe.STTOptions{})
	if err == nil || !strings.Contains(err.Error(), "transcription.stt.timeout_s") || !res.Reached {
		t.Errorf("err = %v reached %v", err, res.Reached)
	}
}

type noAudio struct{}

func (noAudio) ID() string                           { return "anthropic" }
func (noAudio) Capabilities() providers.Capabilities { return providers.Capabilities{} }
func (noAudio) Probe(context.Context) error          { return nil }
func (noAudio) ListModels(context.Context) ([]string, error) {
	return nil, nil
}
func (noAudio) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	return nil, nil
}
