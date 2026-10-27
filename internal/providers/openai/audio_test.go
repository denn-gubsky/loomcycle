package openai

import (
	"context"
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

// TestTranscribeAudio_SendsMultipartToTheAudioEndpoint — one POST to
// {base_url}/audio/transcriptions carrying the model, the non-empty fields and
// the file under its name, with the operator's key; the body comes back as it
// was sent.
func TestTranscribeAudio_SendsMultipartToTheAudioEndpoint(t *testing.T) {
	var got struct {
		path, auth, model, language, prompt, filename string
		file                                          []byte
		hasEmpty                                      bool
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse: %v", err)
		}
		got.model, got.language, got.prompt = r.FormValue("model"), r.FormValue("language"), r.FormValue("prompt")
		_, got.hasEmpty = r.MultipartForm.Value["chunking_strategy"]
		f, h, err := r.FormFile("file")
		if err == nil {
			got.filename = h.Filename
			got.file, _ = io.ReadAll(f)
		}
		_, _ = io.WriteString(w, `{"text":"добрий день"}`)
	}))
	defer srv.Close()
	d := New("host-key", srv.URL+"/v1", streamhttp.Options{}, srv.Client())
	res, err := d.TranscribeAudio(context.Background(), providers.AudioRequest{
		Model: "whisper-large-v3", File: []byte("RIFFdata"), Filename: "audio.wav",
		Fields: map[string]string{"language": "uk", "prompt": "Oblenergo", "chunking_strategy": ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/v1/audio/transcriptions" || got.auth != "Bearer host-key" {
		t.Errorf("path %q auth %q", got.path, got.auth)
	}
	if got.model != "whisper-large-v3" || got.language != "uk" || got.prompt != "Oblenergo" || got.hasEmpty {
		t.Errorf("fields: %+v", got)
	}
	if got.filename != "audio.wav" || string(got.file) != "RIFFdata" {
		t.Errorf("file %q = %q", got.filename, got.file)
	}
	if string(res.Body) != `{"text":"добрий день"}` || res.CredentialSource != "operator" {
		t.Errorf("response = %q from %q", res.Body, res.CredentialSource)
	}
}

// TestTranscribeAudio_TenantKeyOverridesAndRestrictionRefuses — the key is
// resolved as for a chat call: a tenant's own key is sent and named as the
// payer, and a run restricted from the operator's key with none of its own is
// refused before any request.
func TestTranscribeAudio_TenantKeyOverridesAndRestrictionRefuses(t *testing.T) {
	var calls atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"text":"x"}`)
	}))
	defer srv.Close()
	d := New("host-key", srv.URL, streamhttp.Options{}, srv.Client())
	req := providers.AudioRequest{Model: "whisper-1", File: []byte("x"), Filename: "a.mp3"}

	tenant := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
		return providers.CredentialResolution{Value: "tenant-key", Scope: "tenant", ScopeID: "t1"}, name == "OPENAI_API_KEY", nil
	})
	res, err := d.TranscribeAudio(tenant, req)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Load() != "Bearer tenant-key" || res.CredentialSource != "tenant" || res.CredentialScopeID != "t1" {
		t.Errorf("auth %v source %q/%q", auth.Load(), res.CredentialSource, res.CredentialScopeID)
	}

	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)
	before := calls.Load()
	if _, err := d.TranscribeAudio(restricted, req); !errors.Is(err, providers.ErrOperatorKeyForbidden) {
		t.Fatalf("restricted run: err = %v, want ErrOperatorKeyForbidden", err)
	}
	if calls.Load() != before {
		t.Error("a refused call reached the endpoint")
	}
}

// TestTranscribeAudio_NoHeaderTimeout — a whisper server answers only when the
// piece is transcribed. The driver's chat client gives up on a first byte
// after its header timeout; the audio call must not, and ends only with ctx.
func TestTranscribeAudio_NoHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"text":"slow"}`)
	}))
	defer srv.Close()
	// The production client: a header timeout well under the server's delay.
	d := New("", srv.URL, streamhttp.Options{HeaderTimeout: 50 * time.Millisecond}, nil)
	res, err := d.TranscribeAudio(context.Background(), providers.AudioRequest{Model: "m", File: []byte("x"), Filename: "a.wav"})
	if err != nil {
		t.Fatalf("a slow answer was cut off: %v", err)
	}
	if !strings.Contains(string(res.Body), "slow") {
		t.Errorf("body = %q", res.Body)
	}
	// The chat client keeps its bound.
	if tr, ok := d.http.Transport.(*http.Transport); !ok || tr.ResponseHeaderTimeout != 50*time.Millisecond {
		t.Error("the chat client lost its header timeout")
	}
}

// TestTranscribeAudio_ErrorNamesTheStatus — a refusal names the status and
// the endpoint's own reason.
func TestTranscribeAudio_ErrorNamesTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"file too large"}`, http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()
	d := New("k", srv.URL, streamhttp.Options{}, srv.Client())
	_, err := d.TranscribeAudio(context.Background(), providers.AudioRequest{Model: "m", File: []byte("x"), Filename: "a.wav"})
	if err == nil || !strings.Contains(err.Error(), "413") || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("err = %v", err)
	}
}
