package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
)

var _ providers.SpeechSynthesizer = (*Driver)(nil)

// SynthesizeSpeech sends one piece of text to the provider's text-to-speech
// endpoint, POST {base_url}/audio/speech, and returns the audio as received.
// The same code serves OpenAI and the self-hosted servers that copy its shape
// (Ollama, Phonon's own server, speaches, Kokoro-FastAPI).
//
// The audio is not parsed or repaired: Ollama and Phonon's server stream WAV
// with 0xFFFFFFFF in the RIFF and data size fields, and the joiner takes the
// length from the bytes.
//
// The caller bounds the call with ctx. Like TranscribeAudio, the request goes
// out on the client with no response-header timeout: a CPU host sends nothing
// until it has synthesised the piece.
func (d *Driver) SynthesizeSpeech(ctx context.Context, req providers.SpeechRequest) (providers.SpeechResponse, error) {
	if req.Model == "" || req.Voice == "" || req.Input == "" {
		return providers.SpeechResponse{}, errors.New("openai-speech: a model, a voice and the input are required")
	}
	if req.Format != "wav" && req.Format != "pcm" {
		return providers.SpeechResponse{}, fmt.Errorf("openai-speech: format %q is not wav or pcm", req.Format)
	}
	if req.Speed < 0 || math.IsNaN(req.Speed) || math.IsInf(req.Speed, 0) {
		return providers.SpeechResponse{}, fmt.Errorf("openai-speech: speed %v is not a rate", req.Speed)
	}
	body, err := json.Marshal(speechBody{
		Model: req.Model, Input: req.Input, Voice: req.Voice,
		ResponseFormat: req.Format, Speed: req.Speed, Instructions: req.Instructions,
	})
	if err != nil {
		return providers.SpeechResponse{}, fmt.Errorf("openai-speech: build request: %w", err)
	}
	// RFC AR/AX: the key and whose it is, once per call, as Call resolves it.
	apiKey, credSource, credScopeID, err := d.resolveKey(ctx)
	if err != nil {
		return providers.SpeechResponse{}, err
	}
	client := d.audioHTTP()
	attempt := func(c context.Context) (*http.Response, error) {
		build := func() (*http.Request, error) {
			r, err := http.NewRequestWithContext(c, http.MethodPost, d.baseURL+"/audio/speech", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			r.Header.Set("Content-Type", "application/json")
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
			return providers.SpeechResponse{}, err
		}
		return providers.SpeechResponse{}, fmt.Errorf("openai-speech: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// One word before the status: the shared classifier reads the
		// "<name> <code>:" prefix (a 400 is permanent, a 401 refuses the
		// key, a 5xx is retryable), and "openai speech 400:" would not match.
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return providers.SpeechResponse{}, providers.WithKeySource(
			fmt.Errorf("openai-speech %d: %s", resp.StatusCode, strings.TrimSpace(string(preview))), credSource)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, providers.MaxSpeechResponseBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return providers.SpeechResponse{}, ctxErr
		}
		return providers.SpeechResponse{}, fmt.Errorf("openai-speech: reading the audio: %w", err)
	}
	if len(out) > providers.MaxSpeechResponseBytes {
		return providers.SpeechResponse{}, fmt.Errorf("openai-speech: the audio is over %d bytes", providers.MaxSpeechResponseBytes)
	}
	if len(out) == 0 {
		return providers.SpeechResponse{}, errors.New("openai-speech: the endpoint answered with no audio")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = speechContentType(req.Format)
	}
	return providers.SpeechResponse{
		Audio: out, ContentType: contentType,
		CredentialSource: credSource, CredentialScopeID: credScopeID,
	}, nil
}

// speechBody is the JSON request. speed and instructions are omitted when
// unset: Ollama and Phonon's server answer 400 to either field.
type speechBody struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed,omitempty"`
	Instructions   string  `json:"instructions,omitempty"`
}

// speechContentType is the type of a format the runtime asks for, used only
// when the endpoint names none.
func speechContentType(format string) string {
	if format == "pcm" {
		return "audio/pcm"
	}
	return "audio/wav"
}
