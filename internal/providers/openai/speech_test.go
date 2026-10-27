package openai

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

var speechReq = providers.SpeechRequest{Model: "phonon", Voice: "Hadley", Input: "Good morning.", Format: "wav"}

// speechServer answers every request with audio, recording the last
// request's path, method, headers and JSON body.
type speechSeen struct {
	path, method string
	header       http.Header
	body         map[string]any
}

func speechServer(t *testing.T, audio []byte) (*httptest.Server, *speechSeen, *atomic.Int32) {
	t.Helper()
	seen := &speechSeen{}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seen.path, seen.method, seen.header = r.URL.Path, r.Method, r.Header.Clone()
		seen.body = nil
		if err := json.NewDecoder(r.Body).Decode(&seen.body); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(audio)
	}))
	t.Cleanup(srv.Close)
	return srv, seen, &calls
}

// TestSynthesizeSpeech_PostsJSONToTheSpeechEndpoint — one POST to
// {base_url}/audio/speech with model, input, voice and response_format, the
// operator's key, and no speed or instructions when they are unset (Ollama and
// Phonon's server answer 400 to either). The audio comes back as sent.
func TestSynthesizeSpeech_PostsJSONToTheSpeechEndpoint(t *testing.T) {
	srv, seen, _ := speechServer(t, []byte("RIFFaudio"))
	d := New("host-key", srv.URL+"/v1", streamhttp.Options{}, srv.Client())
	res, err := d.SynthesizeSpeech(context.Background(), speechReq)
	if err != nil {
		t.Fatal(err)
	}
	if seen.method != http.MethodPost || seen.path != "/v1/audio/speech" {
		t.Errorf("%s %s", seen.method, seen.path)
	}
	if seen.header.Get("Authorization") != "Bearer host-key" || seen.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v", seen.header)
	}
	want := map[string]any{"model": "phonon", "input": "Good morning.", "voice": "Hadley", "response_format": "wav"}
	if len(seen.body) != len(want) {
		t.Errorf("body fields = %v, want exactly %v", seen.body, want)
	}
	for k, v := range want {
		if seen.body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, seen.body[k], v)
		}
	}
	if string(res.Audio) != "RIFFaudio" || res.ContentType != "audio/wav" || res.CredentialSource != "operator" {
		t.Errorf("response = %q %q from %q", res.Audio, res.ContentType, res.CredentialSource)
	}
}

// TestSynthesizeSpeech_SendsSpeedAndInstructionsWhenSet — the optional
// fields go out when the runtime sets them.
func TestSynthesizeSpeech_SendsSpeedAndInstructionsWhenSet(t *testing.T) {
	srv, seen, _ := speechServer(t, []byte("pcm"))
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	req := speechReq
	req.Format, req.Speed, req.Instructions = "pcm", 1.25, "Speak calmly."
	if _, err := d.SynthesizeSpeech(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if seen.body["speed"] != 1.25 || seen.body["instructions"] != "Speak calmly." || seen.body["response_format"] != "pcm" {
		t.Errorf("body = %v", seen.body)
	}
}

// TestOpenAI_SynthesizeSpeechUsesTenantKey — the key is resolved as for a
// chat call: a tenant's own key is sent and named as the payer.
func TestOpenAI_SynthesizeSpeechUsesTenantKey(t *testing.T) {
	srv, seen, _ := speechServer(t, []byte("RIFF"))
	d := New("host-key", srv.URL, streamhttp.Options{}, srv.Client())
	tenant := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
		return providers.CredentialResolution{Value: "tenant-key", Scope: "tenant", ScopeID: "t1"}, name == "OPENAI_API_KEY", nil
	})
	res, err := d.SynthesizeSpeech(tenant, speechReq)
	if err != nil {
		t.Fatal(err)
	}
	if seen.header.Get("Authorization") != "Bearer tenant-key" || res.CredentialSource != "tenant" || res.CredentialScopeID != "t1" {
		t.Errorf("auth %q source %q/%q", seen.header.Get("Authorization"), res.CredentialSource, res.CredentialScopeID)
	}
}

// TestSynthesizeSpeech_RestrictedRunRefusedBeforeRequest — a run restricted
// from the operator's key, with none of its own, never reaches the endpoint.
func TestSynthesizeSpeech_RestrictedRunRefusedBeforeRequest(t *testing.T) {
	srv, _, calls := speechServer(t, []byte("RIFF"))
	d := New("host-key", srv.URL, streamhttp.Options{}, srv.Client())
	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)
	if _, err := d.SynthesizeSpeech(restricted, speechReq); !errors.Is(err, providers.ErrOperatorKeyForbidden) {
		t.Fatalf("err = %v, want ErrOperatorKeyForbidden", err)
	}
	if calls.Load() != 0 {
		t.Error("a refused call reached the endpoint")
	}
}

// TestSynthesizeSpeech_OperatorCallIsAuthenticatedByRequestAuth — with a
// RequestAuth (Bedrock), the operator's call is signed over the JSON body and
// carries no key; a tenant key bypasses it.
func TestSynthesizeSpeech_OperatorCallIsAuthenticatedByRequestAuth(t *testing.T) {
	srv, seen, _ := speechServer(t, []byte("RIFF"))
	a := &fakeAuth{}
	d := authDriver(srv.URL, a)
	if _, err := d.SynthesizeSpeech(context.Background(), speechReq); err != nil {
		t.Fatal(err)
	}
	if a.applied.Load() != 1 || !strings.HasPrefix(seen.header.Get("Authorization"), "AWS4-HMAC-SHA256 signed-1") {
		t.Errorf("applied=%d Authorization=%q", a.applied.Load(), seen.header.Get("Authorization"))
	}
	tenant := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
		return providers.CredentialResolution{Value: "tenant-bedrock-key", Scope: "tenant"}, name == "AWS_BEARER_TOKEN_BEDROCK", nil
	})
	if _, err := d.SynthesizeSpeech(tenant, speechReq); err != nil {
		t.Fatal(err)
	}
	if a.applied.Load() != 1 || seen.header.Get("Authorization") != "Bearer tenant-bedrock-key" {
		t.Errorf("tenant call: applied=%d Authorization=%q", a.applied.Load(), seen.header.Get("Authorization"))
	}
}

// TestSynthesizeSpeech_InvalidRequestRefusedBeforeCall — the runtime always
// sends a model, a voice, the text and wav or pcm; anything else is a bug
// upstream and never reaches the endpoint.
func TestSynthesizeSpeech_InvalidRequestRefusedBeforeCall(t *testing.T) {
	srv, _, calls := speechServer(t, []byte("RIFF"))
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	for name, mut := range map[string]func(*providers.SpeechRequest){
		"no model":       func(r *providers.SpeechRequest) { r.Model = "" },
		"no voice":       func(r *providers.SpeechRequest) { r.Voice = "" },
		"no input":       func(r *providers.SpeechRequest) { r.Input = "" },
		"no format":      func(r *providers.SpeechRequest) { r.Format = "" },
		"mp3":            func(r *providers.SpeechRequest) { r.Format = "mp3" },
		"negative speed": func(r *providers.SpeechRequest) { r.Speed = -1 },
	} {
		req := speechReq
		mut(&req)
		if _, err := d.SynthesizeSpeech(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d invalid requests reached the endpoint", calls.Load())
	}
}

// TestSynthesizeSpeech_ErrorNamesTheStatus — a refusal names the status and
// the endpoint's own reason, bounded, and is classified with whose key it was.
func TestSynthesizeSpeech_ErrorNamesTheStatus(t *testing.T) {
	reason := `{"error":"unknown voice"}` + strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, reason, http.StatusBadRequest)
	}))
	defer srv.Close()
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	_, err := d.SynthesizeSpeech(context.Background(), speechReq)
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "unknown voice") {
		t.Fatalf("err = %v", err)
	}
	if len(err.Error()) > 1200 {
		t.Errorf("the error carries %d bytes of the body", len(err.Error()))
	}
	if providers.ClassifyError(err) != providers.ErrorClassPermanent {
		t.Errorf("a 400 classified %v", providers.ClassifyError(err))
	}
}

// TestSynthesizeSpeech_OperatorKeyRefusalIsAttributed — a refused key is
// reported with whose it was, so a refused operator key can take the provider
// out while a tenant's own refused key never does.
func TestSynthesizeSpeech_OperatorKeyRefusalIsAttributed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"invalid_api_key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	d := New("host-key", srv.URL, streamhttp.Options{}, srv.Client())
	_, err := d.SynthesizeSpeech(context.Background(), speechReq)
	if !providers.OperatorKeyRefused(err) {
		t.Errorf("operator key refusal not attributed: %v", err)
	}
	tenant := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
		return providers.CredentialResolution{Value: "tenant-key", Scope: "tenant"}, name == "OPENAI_API_KEY", nil
	})
	_, err = d.SynthesizeSpeech(tenant, speechReq)
	if err == nil || providers.OperatorKeyRefused(err) {
		t.Errorf("a tenant key refusal was blamed on the operator: %v", err)
	}
}

// TestSynthesizeSpeech_RetriesARateLimit — a 429 is retried as a chat call's
// is, and the second answer's audio comes back.
func TestSynthesizeSpeech_RetriesARateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "RIFFsecond")
	}))
	defer srv.Close()
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	res, err := d.SynthesizeSpeech(context.Background(), speechReq)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || string(res.Audio) != "RIFFsecond" {
		t.Errorf("calls=%d audio=%q", calls.Load(), res.Audio)
	}
}

// TestSynthesizeSpeech_AudioOverTheBoundRefused — an answer of exactly
// MaxSpeechResponseBytes is taken; one byte more is an error, never a
// truncated file.
func TestSynthesizeSpeech_AudioOverTheBoundRefused(t *testing.T) {
	var size atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.CopyN(w, zeroReader{}, size.Load())
	}))
	defer srv.Close()
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())

	size.Store(providers.MaxSpeechResponseBytes)
	if res, err := d.SynthesizeSpeech(context.Background(), speechReq); err != nil || len(res.Audio) != providers.MaxSpeechResponseBytes {
		t.Fatalf("at the bound: %d bytes, err %v", len(res.Audio), err)
	}

	size.Store(providers.MaxSpeechResponseBytes + 1)
	if _, err := d.SynthesizeSpeech(context.Background(), speechReq); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("over the bound: err = %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestSynthesizeSpeech_StreamedWAVReturnedByteIdentical — Ollama and Phonon's
// server stream WAV chunked, with 0xFFFFFFFF in the RIFF and data sizes. The
// driver returns the bytes as received: it does not read, fix or cut at the
// header's sizes.
func TestSynthesizeSpeech_StreamedWAVReturnedByteIdentical(t *testing.T) {
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(0xFFFFFFFF))
	wav.WriteString("WAVEfmt ")
	_ = binary.Write(&wav, binary.LittleEndian, []any{uint32(16), uint16(1), uint16(1), uint32(24000), uint32(48000), uint16(2), uint16(16)})
	wav.WriteString("data")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(0xFFFFFFFF))
	for i := range 48000 {
		wav.WriteByte(byte(i))
	}
	want := wav.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "audio/wav")
		f := w.(http.Flusher)
		for rest := want; len(rest) > 0; {
			n := min(len(rest), 4093)
			_, _ = w.Write(rest[:n])
			f.Flush()
			rest = rest[n:]
		}
	}))
	defer srv.Close()
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	res, err := d.SynthesizeSpeech(context.Background(), speechReq)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Audio, want) {
		t.Errorf("audio differs: got %d bytes, want %d", len(res.Audio), len(want))
	}
}

// TestSynthesizeSpeech_ContentTypeFallsBackToTheFormat — an endpoint that
// names no type gets the format's own.
func TestSynthesizeSpeech_ContentTypeFallsBackToTheFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header()["Content-Type"] = nil // no sniffing either
		_, _ = w.Write([]byte{1, 2, 3})
	}))
	defer srv.Close()
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	for format, want := range map[string]string{"wav": "audio/wav", "pcm": "audio/pcm"} {
		req := speechReq
		req.Format = format
		res, err := d.SynthesizeSpeech(context.Background(), req)
		if err != nil || res.ContentType != want {
			t.Errorf("%s: type %q err %v", format, res.ContentType, err)
		}
	}
}

// TestSynthesizeSpeech_EmptyAnswerIsAnError — a 200 with no audio is a
// failed piece, not a silent gap in the joined file.
func TestSynthesizeSpeech_EmptyAnswerIsAnError(t *testing.T) {
	srv, _, _ := speechServer(t, nil)
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	if _, err := d.SynthesizeSpeech(context.Background(), speechReq); err == nil {
		t.Fatal("an empty answer was accepted")
	}
}

// TestSynthesizeSpeech_CancelEndsTheCall — the caller's ctx ends the call
// both while the endpoint is still synthesising (no headers yet) and midway
// through the audio, and the error is the ctx's.
func TestSynthesizeSpeech_CancelEndsTheCall(t *testing.T) {
	for _, midBody := range []bool{false, true} {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if midBody {
				_, _ = w.Write([]byte("RIFF"))
				w.(http.Flusher).Flush()
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		done := make(chan error, 1)
		go func() {
			_, err := d.SynthesizeSpeech(ctx, speechReq)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("midBody=%v: err = %v, want context.Canceled", midBody, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("midBody=%v: the call outlived its ctx", midBody)
		}
		close(release)
		cancel()
		srv.Close()
	}
}

// TestSynthesizeSpeech_NoHeaderTimeout — a CPU host answers only when the
// piece is synthesised. The driver's chat client gives up on a first byte
// after its header timeout; the speech call must not.
func TestSynthesizeSpeech_NoHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "RIFFslow")
	}))
	defer srv.Close()
	d := New("", srv.URL, streamhttp.Options{HeaderTimeout: 50 * time.Millisecond}, nil)
	res, err := d.SynthesizeSpeech(context.Background(), speechReq)
	if err != nil {
		t.Fatalf("a slow answer was cut off: %v", err)
	}
	if string(res.Audio) != "RIFFslow" {
		t.Errorf("audio = %q", res.Audio)
	}
	if tr, ok := d.http.Transport.(*http.Transport); !ok || tr.ResponseHeaderTimeout != 50*time.Millisecond {
		t.Error("the chat client lost its header timeout")
	}
}

// wrapped stands in for the registry's lane wrapper: it hides everything the
// driver implements beyond Provider.
type wrapped struct{ providers.Provider }

func (w wrapped) Unwrap() providers.Provider { return w.Provider }

// TestSynthesizeSpeech_ReachedThroughTheWrapperByAs — a registry provider
// does not satisfy SpeechSynthesizer itself; As finds the driver inside.
func TestSynthesizeSpeech_ReachedThroughTheWrapperByAs(t *testing.T) {
	srv, _, calls := speechServer(t, []byte("RIFF"))
	var p providers.Provider = wrapped{New("k", srv.URL, streamhttp.Options{}, srv.Client())}
	if _, ok := p.(providers.SpeechSynthesizer); ok {
		t.Fatal("the wrapper exposes the driver's methods; this test no longer models the registry")
	}
	ss, ok := providers.As[providers.SpeechSynthesizer](p)
	if !ok {
		t.Fatal("As did not find the synthesizer")
	}
	res, err := ss.SynthesizeSpeech(context.Background(), speechReq)
	if err != nil || string(res.Audio) != "RIFF" || calls.Load() != 1 {
		t.Errorf("audio %q err %v calls %d", res.Audio, err, calls.Load())
	}
}
