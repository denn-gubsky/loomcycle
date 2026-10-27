package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
)

var _ providers.AudioTranscriber = (*Driver)(nil)

// TranscribeAudio sends one recording to the provider's speech-to-text
// endpoint, POST {base_url}/audio/transcriptions. The same code serves OpenAI
// and the self-hosted whisper servers that copy its shape (speaches,
// whisper.cpp's server, LocalAI).
//
// The caller bounds the call with ctx. The request goes out on a client with
// no response-header timeout: a transcription server sends nothing until it has
// transcribed the whole piece, which on a CPU host takes minutes, where a chat
// stream's first byte comes in seconds.
func (d *Driver) TranscribeAudio(ctx context.Context, req providers.AudioRequest) (providers.AudioResponse, error) {
	if req.Model == "" || len(req.File) == 0 || req.Filename == "" {
		return providers.AudioResponse{}, errors.New("openai audio: a model, a file and its name are required")
	}
	body, contentType, err := audioForm(req)
	if err != nil {
		return providers.AudioResponse{}, fmt.Errorf("openai audio: build request: %w", err)
	}
	// RFC AR/AX: the key and whose it is, once per call, as Call resolves it.
	apiKey, credSource, credScopeID, err := d.resolveKey(ctx)
	if err != nil {
		return providers.AudioResponse{}, err
	}
	client := d.audioHTTP()
	attempt := func(c context.Context) (*http.Response, error) {
		build := func() (*http.Request, error) {
			r, err := http.NewRequestWithContext(c, http.MethodPost, d.baseURL+"/audio/transcriptions", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			r.Header.Set("Content-Type", contentType)
			r.Header.Set("Accept", "application/json")
			return r, nil
		}
		if d.auth != nil && credSource == "operator" {
			return providers.DoWithAuth(client, d.auth, body, build, setBearer)
		}
		r, err := build()
		if err != nil {
			return nil, err
		}
		if apiKey != "" {
			setBearer(r, apiKey)
		}
		return client.Do(r)
	}
	resp, err := ratelimit.Do(ctx, ratelimit.Config{Provider: "openai", ParseHeader: ratelimit.OpenAIRetryAfter}, attempt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return providers.AudioResponse{}, err
		}
		return providers.AudioResponse{}, fmt.Errorf("openai audio: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return providers.AudioResponse{}, providers.WithKeySource(
			fmt.Errorf("openai audio %d: %s", resp.StatusCode, strings.TrimSpace(string(preview))), credSource)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, providers.MaxAudioResponseBytes+1))
	if err != nil {
		return providers.AudioResponse{}, fmt.Errorf("openai audio: reading the answer: %w", err)
	}
	if len(out) > providers.MaxAudioResponseBytes {
		return providers.AudioResponse{}, fmt.Errorf("openai audio: the answer is over %d bytes", providers.MaxAudioResponseBytes)
	}
	return providers.AudioResponse{Body: out, CredentialSource: credSource, CredentialScopeID: credScopeID}, nil
}

// audioForm encodes the request as multipart/form-data: model, the other
// fields in name order (so a request is byte-for-byte reproducible), then the
// file.
func audioForm(req providers.AudioRequest) ([]byte, string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("model", req.Model); err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(req.Fields))
	for k := range req.Fields {
		if k == "model" || k == "file" {
			return nil, "", fmt.Errorf("field %q is set by the driver", k)
		}
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if v := req.Fields[k]; v != "" {
			if err := w.WriteField(k, v); err != nil {
				return nil, "", err
			}
		}
	}
	fw, err := w.CreateFormFile("file", req.Filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(req.File); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return b.Bytes(), w.FormDataContentType(), nil
}

// audioHTTP is the driver's client without its response-header timeout. Its
// own Transport is copied (keeping the proxy and TLS settings); a client whose
// Transport is not an *http.Transport (a test's) is used as it is.
func (d *Driver) audioHTTP() *http.Client {
	d.audioOnce.Do(func() {
		d.audioClient = d.http
		if t, ok := d.http.Transport.(*http.Transport); ok && t.ResponseHeaderTimeout > 0 {
			c := t.Clone()
			c.ResponseHeaderTimeout = 0
			d.audioClient = &http.Client{Transport: c, CheckRedirect: d.http.CheckRedirect, Jar: d.http.Jar}
		}
	})
	return d.audioClient
}
