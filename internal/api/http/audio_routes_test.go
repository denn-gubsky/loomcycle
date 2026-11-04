package http

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// ---- fixtures ----

// pcmWAV is a mono 16-bit PCM WAV of silence at rate Hz lasting d.
func pcmWAV(rate int, d time.Duration) []byte {
	data := make([]byte, int(d.Seconds()*float64(rate))*2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// speakingProvider is a chat provider that also speaks: each piece comes
// back as a tenth of a second of WAV per character. hold, when set, keeps
// every request waiting until it is closed.
type speakingProvider struct {
	scriptedProvider
	mu    sync.Mutex
	sent  []providers.SpeechRequest
	hold  chan struct{}
	fail  error
	inEng atomic.Int32
}

func (p *speakingProvider) SynthesizeSpeech(ctx context.Context, req providers.SpeechRequest) (providers.SpeechResponse, error) {
	p.mu.Lock()
	p.sent = append(p.sent, req)
	p.mu.Unlock()
	p.inEng.Add(1)
	defer p.inEng.Add(-1)
	if p.fail != nil {
		return providers.SpeechResponse{}, p.fail
	}
	if p.hold != nil {
		select {
		case <-p.hold:
		case <-ctx.Done():
			return providers.SpeechResponse{}, ctx.Err()
		}
	}
	d := time.Duration(len([]rune(req.Input))) * 100 * time.Millisecond
	return providers.SpeechResponse{Audio: pcmWAV(16000, d), ContentType: "audio/wav", CredentialSource: "operator"}, nil
}

func (p *speakingProvider) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

// hearingEngine is an STT engine that hears "word" in every piece and
// reports the piece's length as what it read.
type hearingEngine struct {
	readsWhole bool
	calls      atomic.Int32
}

func (e *hearingEngine) Name() string { return "openai-audio" }
func (e *hearingEngine) Accepts(mt string) bool {
	return e.readsWhole || strings.Contains(mt, "wav")
}
func (e *hearingEngine) Plan(context.Context) (transcribe.STTPlan, error) {
	return transcribe.STTPlan{Engine: "openai-audio", Provider: "whisper-local", Model: "large-v3", MaxBytes: 25 << 20}, nil
}
func (e *hearingEngine) Transcribe(_ context.Context, _ transcribe.STTPlan, u transcribe.Unit, _ transcribe.STTOptions) (transcribe.STTResult, error) {
	e.calls.Add(1)
	length := u.Span.End - u.Span.Start
	return transcribe.STTResult{Reached: true, Language: "en", Duration: length,
		Segments: transcribe.Segments{{End: length, Text: "word"}}}, nil
}

// speechConfig is an English voice on a speaches-style provider, and a
// transcription block cutting WAVs into one-minute pieces.
func withAudio(cfg *config.Config) {
	cfg.Models = map[string]config.ModelRef{
		"en-voice": {Provider: "speaches", Model: "kokoro", Kind: config.ModelKindSynthesizer},
		"whisper":  {Provider: "whisper-local", Model: "large-v3", Kind: config.ModelKindTranscriber},
		"chatty":   {Provider: "speaches", Model: "llama"},
	}
	cfg.Speech = &config.SpeechConfig{
		Engine: config.SpeechEngineOpenAISpeech, DefaultLanguage: "en", Format: config.SpeechFormatWAV,
		Voices:          map[string]config.SpeechVoice{"en": {Model: "en-voice", Voice: "af_heart", Alternates: []string{"am_adam"}}},
		MaxCharsPerCall: 40, MaxCharsPerRun: 400, MaxSecondsPerRun: 3600, TimeoutSeconds: 30,
	}
	cfg.Transcription = config.TranscriptionConfig{STT: &config.STTConfig{
		Engine: config.STTEngineOpenAIAudio, Provider: "whisper-local", Model: "whisper", ChunkMinutes: 1,
	}}
}

// audioServer is an open-mode server with both engines on.
func audioServer(t *testing.T, prov *speakingProvider, eng *hearingEngine) (*Server, *storesqlite.Store) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "audio.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4}}
	withAudio(cfg)
	cfg.Env.MaxRequestBytes = 16 << 20
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetSTT(&builtin.FileSTT{Config: cfg.Transcription.Resolved(), Engine: eng, Ledger: transcriptionLedger{s: srv}})
	return srv, st
}

func postAudioJSON(srv *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Mux().ServeHTTP(rec, req)
	return rec
}

// upload posts a multipart form with the file part typed as mediaType.
func upload(srv *Server, file []byte, filename, mediaType string, fields map[string]string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", mediaType)
	part, _ := mw.CreatePart(h)
	_, _ = part.Write(file)
	_ = mw.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	srv.Mux().ServeHTTP(rec, req)
	return rec
}

// ---- POST /v1/audio/speech ----

// Markdown in, WAV out: the text is cut into pieces under max_chars_per_call,
// each spoken by the language's voice, joined, described in the headers, and
// charged to the caller as one tts row per engine request with no run.
func TestAudioSpeech_SpeaksTheTextAndChargesTheCaller(t *testing.T) {
	prov := &speakingProvider{}
	srv, st := audioServer(t, prov, &hearingEngine{})
	rec := postAudioJSON(srv, "/v1/audio/speech", `{"input":"# Notes\n\nThe first sentence is here. The second one follows it.\n\nA new paragraph."}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Content-Type") != "audio/wav" || h.Get("Content-Length") != fmt.Sprint(rec.Body.Len()) ||
		h.Get("X-Loomcycle-Speech-Language") != "en" || h.Get("X-Loomcycle-Speech-Voice") != "af_heart" {
		t.Errorf("headers = %v", h)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("RIFF")) {
		t.Fatalf("body is not a WAV: % x", rec.Body.Bytes()[:min(8, rec.Body.Len())])
	}
	if n := prov.requests(); n < 2 || h.Get("X-Loomcycle-Speech-Pieces") != fmt.Sprint(n) {
		t.Errorf("engine requests = %d, pieces header %q: the text should be cut under 40 characters", n, h.Get("X-Loomcycle-Speech-Pieces"))
	}
	for _, r := range prov.sent {
		if strings.Contains(r.Input, "#") || r.Model != "kokoro" || r.Voice != "af_heart" || r.Format != "wav" {
			t.Errorf("engine request = %+v", r)
		}
	}
	rows, err := st.TokenUsageForRun(context.Background(), "")
	if err != nil || len(rows) != prov.requests() {
		t.Fatalf("ledger = %d rows (%v), want one per engine request (%d)", len(rows), err, prov.requests())
	}
	for _, r := range rows {
		if r.Purpose != "tts" || r.InputChars <= 0 || r.AudioSeconds <= 0 || r.Provider != "speaches" || r.Model != "kokoro" {
			t.Errorf("row = %+v", r)
		}
	}
}

// Every refusal the caller can fix is answered before the engine is asked,
// with a code it can branch on.
func TestAudioSpeech_RefusesBeforeTheEngine(t *testing.T) {
	for _, c := range []struct {
		name, body string
		edit       func(*config.Config)
		status     int
		code, says string
	}{
		{"an encoded format with no encoder", `{"input":"Hello.","response_format":"mp3"}`, nil, 400, "speech_encode_not_configured", "speech.encode"},
		{"the other encoded format", `{"input":"Hello.","response_format":"mp3"}`, func(c *config.Config) {
			c.Speech.Format = config.SpeechFormatOgg
			c.Speech.Encode = &config.HelperCommand{Command: "cp {in} {out}"}
		}, 400, "speech_encode_not_configured", "speech.format"},
		{"a voice outside the list", `{"input":"Hello.","voice":"nova"}`, nil, 400, "unsupported_voice", "am_adam"},
		{"a language with no voice", `{"input":"Hello.","language":"de"}`, nil, 400, "unsupported_language", ""},
		{"nothing to speak after Markdown", `{"input":"**  **"}`, nil, 400, "empty_input", ""},
		{"a model that is not a synthesizer", `{"input":"Hello.","model":"chatty"}`, nil, 400, "model_not_allowed", "synthesizer"},
		{"too long for one request", `{"input":"` + strings.Repeat("Words go here. ", 40) + `"}`, nil, 413, "input_too_long", "speech.max_chars_per_run"},
		{"text-to-speech off", `{"input":"Hello."}`, func(c *config.Config) { c.Speech.Engine = config.SpeechEngineOff }, 503, "speech_not_configured", ""},
		{"not JSON", `{"input":`, nil, 400, "bad_request", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			prov := &speakingProvider{}
			srv, _ := audioServer(t, prov, &hearingEngine{})
			if c.edit != nil {
				c.edit(srv.cfg())
			}
			rec := postAudioJSON(srv, "/v1/audio/speech", c.body)
			if rec.Code != c.status || errorCode(t, rec.Body.Bytes()) != c.code || !strings.Contains(rec.Body.String(), c.says) {
				t.Errorf("got %d %s, want %d %s naming %q", rec.Code, rec.Body.String(), c.status, c.code, c.says)
			}
			if prov.requests() != 0 {
				t.Errorf("the engine was asked %d times", prov.requests())
			}
		})
	}
}

// An engine failure is a 502 the caller can retry on; what the engine said
// (which names its host and model) goes to the log, never to the caller.
func TestAudioSpeech_EngineFailureNamesNoEngine(t *testing.T) {
	prov := &speakingProvider{fail: errors.New("openai-speech: POST http://speaches.internal:8000/v1/audio/speech kokoro: 503 busy")}
	srv, _ := audioServer(t, prov, &hearingEngine{})
	rec := postAudioJSON(srv, "/v1/audio/speech", `{"input":"Hello."}`)
	if rec.Code != 502 || errorCode(t, rec.Body.Bytes()) != "speech_failed" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{"speaches", "8000", "kokoro", "503"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the 502 body names %q: %s", leak, rec.Body.String())
		}
	}
}

// ---- POST /v1/audio/transcriptions ----

// A WAV longer than one piece is cut, each piece sent, and the text comes
// back whole with the recording's length, in each response format.
func TestAudioTranscriptions_CutsAWAVAndReturnsOneText(t *testing.T) {
	eng := &hearingEngine{}
	srv, st := audioServer(t, &speakingProvider{}, eng)
	rec := pcmWAV(8000, 150*time.Second)

	res := upload(srv, rec, "memo.wav", "audio/wav", map[string]string{"language": "en"})
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var got struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Segments []any   `json:"segments"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if eng.calls.Load() != 3 || got.Text != "word word word" || got.Language != "en" || got.Duration != 150 || got.Segments != nil {
		t.Errorf("engine calls %d, reply %+v; want 3 pieces → \"word word word\", en, 150 s, no segments", eng.calls.Load(), got)
	}
	rows, err := st.TokenUsageForRun(context.Background(), "")
	if err != nil || len(rows) != 3 || rows[0].Purpose != "stt" || rows[0].AudioSeconds <= 0 {
		t.Fatalf("ledger = %+v, %v; want three stt rows", rows, err)
	}

	res = upload(srv, rec, "memo.wav", "audio/wav", map[string]string{"response_format": "verbose_json"})
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || len(got.Segments) != 3 {
		t.Errorf("verbose_json: %d segments, %v: %s", len(got.Segments), err, res.Body.String())
	}
	res = upload(srv, rec, "memo.wav", "audio/wav", map[string]string{"response_format": "text"})
	if res.Body.String() != "word word word" || !strings.HasPrefix(res.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("text: %q %s", res.Body.String(), res.Header().Get("Content-Type"))
	}
}

// A browser's own recording (webm) on a host with no converter is sent whole
// when the engine reads that type, and refused with 415 when it does not.
func TestAudioTranscriptions_BrowserRecordingWithoutAConverter(t *testing.T) {
	webm := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 64<<10)...)

	eng := &hearingEngine{readsWhole: true}
	srv, _ := audioServer(t, &speakingProvider{}, eng)
	if res := upload(srv, webm, "rec.webm", "audio/webm;codecs=opus", nil); res.Code != 200 || eng.calls.Load() != 1 {
		t.Errorf("engine reads webm: %d %s, %d calls", res.Code, res.Body.String(), eng.calls.Load())
	}

	eng = &hearingEngine{}
	srv, _ = audioServer(t, &speakingProvider{}, eng)
	res := upload(srv, webm, "rec.webm", "audio/webm", nil)
	if res.Code != 415 || errorCode(t, res.Body.Bytes()) != "unsupported_media_type" || !strings.Contains(res.Body.String(), "transcription.stt.media.command") || eng.calls.Load() != 0 {
		t.Errorf("engine does not read webm: %d %s, %d calls", res.Code, res.Body.String(), eng.calls.Load())
	}
	if res := upload(srv, []byte("just some notes"), "notes.txt", "text/plain", nil); res.Code != 415 {
		t.Errorf("a text file: %d %s", res.Code, res.Body.String())
	}
}

func TestAudioTranscriptions_RefusesBeforeTheEngine(t *testing.T) {
	wav := pcmWAV(8000, 150*time.Second)
	for _, c := range []struct {
		name   string
		fields map[string]string
		edit   func(*Server)
		status int
		code   string
	}{
		{"longer than a request may be", nil, func(s *Server) {
			s.cfg().Transcription.STT.MaxMinutesPerRun = 2
			s.SetSTT(&builtin.FileSTT{Config: s.cfg().Transcription.Resolved(), Engine: s.stt.Engine, Ledger: s.stt.Ledger})
		}, 413, "audio_too_long"},
		{"an unknown response format", map[string]string{"response_format": "srt"}, nil, 400, "unsupported_response_format"},
		{"another model", map[string]string{"model": "whisper-1"}, nil, 400, "model_not_allowed"},
		{"not a language code", map[string]string{"language": "english please"}, nil, 400, "unsupported_language"},
		{"speech-to-text off", nil, func(s *Server) { s.SetSTT(nil) }, 503, "transcription_not_configured"},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng := &hearingEngine{}
			srv, _ := audioServer(t, &speakingProvider{}, eng)
			if c.edit != nil {
				c.edit(srv)
			}
			res := upload(srv, wav, "memo.wav", "audio/wav", c.fields)
			if res.Code != c.status || errorCode(t, res.Body.Bytes()) != c.code {
				t.Errorf("got %d %s, want %d %s", res.Code, res.Body.String(), c.status, c.code)
			}
			if eng.calls.Load() != 0 {
				t.Errorf("the engine was asked %d times", eng.calls.Load())
			}
		})
	}
}

// A body over LOOMCYCLE_MAX_REQUEST_BYTES is a 413 naming the knob.
func TestAudioTranscriptions_BodyOverTheRequestCapIs413(t *testing.T) {
	srv, _ := audioServer(t, &speakingProvider{}, &hearingEngine{})
	srv.cfg().Env.MaxRequestBytes = 64 << 10
	res := upload(srv, pcmWAV(8000, 30*time.Second), "memo.wav", "audio/wav", nil)
	if res.Code != 413 || !strings.Contains(res.Body.String(), "LOOMCYCLE_MAX_REQUEST_BYTES") {
		t.Errorf("got %d %s", res.Code, res.Body.String())
	}
}

// ---- the engine queue ----

// With one slot, a second read-aloud waits for the first instead of being
// refused, and both are answered.
func TestAudioRoutes_WaitForEngineSlot(t *testing.T) {
	prov := &speakingProvider{hold: make(chan struct{})}
	srv, _ := audioServer(t, prov, &hearingEngine{})
	srv.cfg().Env.SpeechMaxInflight = 1
	queue, _ := srv.audioQueues()

	codes := make(chan int, 2)
	for range 2 {
		go func() { codes <- postAudioJSON(srv, "/v1/audio/speech", `{"input":"Hello."}`).Code }()
	}
	waitFor(t, "one request at the engine and one waiting", func() bool {
		return prov.inEng.Load() == 1 && queue.queued() == 1
	})
	close(prov.hold)
	for range 2 {
		if code := <-codes; code != 200 {
			t.Errorf("status %d, want 200 for both", code)
		}
	}
	if queue.inflight() != 0 || queue.queued() != 0 {
		t.Errorf("after both: inflight %d queued %d", queue.inflight(), queue.queued())
	}
}

// A caller who hangs up while waiting leaves the queue holding nothing.
func TestAudioRoutes_CancelledWaiterLeavesQueue(t *testing.T) {
	q := newEngineQueue(1)
	release, err := q.take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := q.take(ctx)
		got <- err
	}()
	waitFor(t, "the second caller waiting", func() bool { return q.queued() == 1 })
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	if q.queued() != 0 || q.inflight() != 1 {
		t.Fatalf("queued %d inflight %d, want 0 and 1", q.queued(), q.inflight())
	}
	release()
	release() // a second release gives back nothing more
	if q.inflight() != 0 {
		t.Fatalf("inflight %d after release", q.inflight())
	}
	if r, err := q.take(context.Background()); err != nil {
		t.Fatalf("the freed slot: %v", err)
	} else {
		r()
	}
}

// ---- GET /v1/_speech ----

// The info route lists what a client may pick, with no engine URL, provider
// or model; an engine that is off is {"enabled": false} and nothing else.
func TestSpeechInfo_ListsWhatAClientMayPick(t *testing.T) {
	srv, _ := audioServer(t, &speakingProvider{}, &hearingEngine{readsWhole: true})
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/_speech", nil))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"speaches", "kokoro", "whisper-local", "large-v3", "en-voice"} {
		if strings.Contains(body, secret) {
			t.Errorf("the info route names %q: %s", secret, body)
		}
	}
	var info speechInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	sp, tr := info.Speech, info.Transcription
	if !sp.Enabled || sp.DefaultLanguage != "en" || strings.Join(sp.Formats, ",") != "wav" ||
		strings.Join(sp.Languages["en"].Voices, ",") != "af_heart,am_adam" || sp.Languages["en"].DefaultVoice != "af_heart" ||
		sp.MaxCharsPerCall != 40 || sp.MaxCharsPerRequest != 400 {
		t.Errorf("speech = %+v", sp)
	}
	if !tr.Enabled || tr.Accepts[0] != "audio/wav" || !strings.Contains(strings.Join(tr.Accepts, ","), "audio/webm") ||
		tr.WholeFileMaxBytes != 25<<20 || tr.MaxUploadBytes != 16<<20 || tr.MaxSecondsPerRequest == 0 {
		t.Errorf("transcription = %+v", tr)
	}

	srv.cfg().Speech.Engine = config.SpeechEngineOff
	srv.SetSTT(nil)
	rec = httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/_speech", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"speech":{"enabled":false},"transcription":{"enabled":false}}` {
		t.Errorf("both off: %s", got)
	}
}

// ---- the gate and discovery ----

// The routes are a chat user's call: an isolated user and a member reach
// them and see them in /v1/_me; an ACL deny on `use gateway:speech` hides the
// surface, refuses the route with 403 and drops the direction from the info
// route; a token limited to certain agents is refused; an engine that is off
// is a false surface.
func TestAudioRoutes_GatedAsAChatUsersCall(t *testing.T) {
	srv, url, st, _ := navFixture(t, authz.ModeCompat)
	withAudio(srv.cfg())
	srv.SetSTT(&builtin.FileSTT{Config: srv.cfg().Transcription.Resolved(), Engine: &hearingEngine{}, Ledger: transcriptionLedger{s: srv}})
	ts := httptestServerAt(url)

	for _, c := range []struct{ name, bearer string }{{"member", freeBearer}, {"isolated user", "tok-nav-iso"}} {
		got := navSurfacesOf(t, url, c.bearer)
		if !got["speech"] || !got["transcription"] {
			t.Errorf("%s: speech=%v transcription=%v, want both true", c.name, got["speech"], got["transcription"])
		}
		if code, body := bearerCall(t, ts, c.bearer, "POST", "/v1/audio/speech", `{"input":"Hello."}`); code == 401 || code == 403 {
			t.Errorf("%s refused at the gate: %d %s", c.name, code, body)
		}
	}
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/audio/speech", `{"input":"Hello."}`); code != 403 || !strings.Contains(body, "restricted_token") {
		t.Errorf("a token limited to certain agents: %d %s", code, body)
	}

	putACL(t, st, "no-voice", `[{"effect":"deny","actions":["use"],"resources":["gateway:speech"]}]`)
	bind(t, st, "no-voice", "user", "dev")
	if got := navSurfacesOf(t, url, freeBearer); got["speech"] || !got["transcription"] {
		t.Errorf("denied speech: speech=%v transcription=%v", got["speech"], got["transcription"])
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/audio/speech", `{"input":"Hello."}`); code != 403 {
		t.Errorf("denied speech: %d %s", code, body)
	}
	code, body := bearerCall(t, ts, freeBearer, "GET", "/v1/_speech", "")
	var info speechInfo
	if code != 200 || json.Unmarshal([]byte(body), &info) != nil || info.Speech.Enabled || !info.Transcription.Enabled {
		t.Errorf("denied speech, info: %d %s", code, body)
	}

	srv.cfg().Speech.Engine = config.SpeechEngineOff
	if got := navSurfacesOf(t, url, "tok-nav-iso"); got["speech"] {
		t.Error("speech engine off, but the isolated user's speech surface is true")
	}
}

// requiredScopeFor puts the audio routes at runs:create, not llm:gateway.
func TestRequiredScopeFor_AudioRoutesAreRunsCreate(t *testing.T) {
	for _, p := range []string{"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/_speech"} {
		method := http.MethodPost
		if p == "/v1/_speech" {
			method = http.MethodGet
		}
		if got := requiredScopeFor(method, p); got != "runs:create" {
			t.Errorf("%s %s: %q", method, p, got)
		}
		if isGatewayRoute(p) || runPlaneRoute(method, p) {
			t.Errorf("%s is on the raw model gateway or the run plane", p)
		}
	}
}
