// Package openaiaudio is the speech-to-text engine that speaks the OpenAI
// audio transcription endpoint (POST {base_url}/audio/transcriptions): OpenAI
// itself, and the self-hosted whisper servers that copy its shape (speaches,
// whisper.cpp's server, LocalAI).
//
// The engine makes one request per unit and nothing else: the File tool cuts
// the recording, reserves its seconds, records the ledger row and writes the
// transcript. The request is made on the tool call's own context through the
// provider's driver, so a tenant's own key and the operator-key restriction
// apply to it exactly as to the run's chat calls.
package openaiaudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// MaxUploadBytes is the endpoint's per-request cap: OpenAI refuses a file
// over 25 MB, and the self-hosted servers are held to the same so a
// deployment can move between them.
const MaxUploadBytes = 25 << 20

// Resolver names the provider and concrete model speech-to-text calls use.
type Resolver func(ctx context.Context) (providerID, model string, err error)

// Options build an Engine.
type Options struct {
	// Config is the operator's resolved transcription.stt block.
	Config config.STTConfig
	// Resolve and Provider are the server's model resolution and its
	// provider registry.
	Resolve  Resolver
	Provider func(id string) (providers.Provider, error)
}

// Engine is the openai-audio engine.
type Engine struct {
	resolve  Resolver
	provider func(id string) (providers.Provider, error)
	timeout  time.Duration
}

// New builds the engine.
func New(o Options) (*Engine, error) {
	if o.Resolve == nil || o.Provider == nil {
		return nil, errors.New("openai-audio: a resolver and a provider registry are required")
	}
	if o.Config.Engine != config.STTEngineOpenAIAudio {
		return nil, fmt.Errorf("openai-audio: transcription.stt.engine is %q, not %s", o.Config.Engine, config.STTEngineOpenAIAudio)
	}
	timeout := time.Duration(o.Config.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = config.TranscriptionDefaultSTTTimeoutSecs * time.Second
	}
	return &Engine{resolve: o.Resolve, provider: o.Provider, timeout: timeout}, nil
}

func (e *Engine) Name() string { return config.STTEngineOpenAIAudio }

// uploadExt is the file extension each accepted type is uploaded under: the
// endpoint reads the format from the name.
var uploadExt = map[string]string{
	"audio/wav": ".wav", "audio/wave": ".wav", "audio/x-wav": ".wav", "audio/vnd.wave": ".wav",
	"audio/mpeg": ".mp3", "audio/mp3": ".mp3",
	"audio/mp4": ".m4a", "audio/m4a": ".m4a", "audio/x-m4a": ".m4a",
	"video/mp4":  ".mp4",
	"audio/flac": ".flac", "audio/x-flac": ".flac",
	"audio/ogg": ".ogg", "application/ogg": ".ogg", "audio/opus": ".ogg",
	"audio/webm": ".webm", "video/webm": ".webm",
	"video/mpeg": ".mpeg",
}

// Accepts is the formats the endpoint reads whole (OpenAI's list: flac, mp3,
// mp4, mpeg, mpga, m4a, ogg, wav, webm).
func (e *Engine) Accepts(mediaType string) bool {
	_, ok := uploadExt[mediaType]
	return ok
}

// Plan resolves the provider and model and checks the provider serves the
// endpoint.
func (e *Engine) Plan(ctx context.Context) (transcribe.STTPlan, error) {
	providerID, model, err := e.resolve(ctx)
	if err != nil {
		return transcribe.STTPlan{}, fmt.Errorf("the speech-to-text model could not be resolved: %w", err)
	}
	prov, err := e.provider(providerID)
	if err != nil {
		return transcribe.STTPlan{}, fmt.Errorf("the speech-to-text provider %q is not available: %w", providerID, err)
	}
	if _, ok := providers.As[providers.AudioTranscriber](prov); !ok {
		return transcribe.STTPlan{}, fmt.Errorf("the provider %q does not serve speech-to-text; point transcription.stt.provider at a provider on the openai driver", providerID)
	}
	return transcribe.STTPlan{Engine: e.Name(), Provider: providerID, Model: model, MaxBytes: MaxUploadBytes, Diarizes: diarizeModel(model)}, nil
}

// diarizeModel reports a model that labels speakers (gpt-4o-transcribe-diarize).
func diarizeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "diarize")
}

// whisperModel reports a whisper-family model, which returns timed segments
// in verbose_json (whisper-1, large-v3, faster-whisper-…, ggml-…).
func whisperModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "whisper")
}

// format is the response_format a request asks for: segments where the model
// gives them, speaker-labelled segments where it labels speakers and labels
// are wanted, plain text otherwise.
func format(plan transcribe.STTPlan, opts transcribe.STTOptions) string {
	switch {
	case plan.Diarizes && opts.Diarize != transcribe.DiarizeOff:
		return "diarized_json"
	case whisperModel(plan.Model):
		return "verbose_json"
	}
	return "json"
}

// Transcribe makes one request for unit.
func (e *Engine) Transcribe(ctx context.Context, plan transcribe.STTPlan, unit transcribe.Unit, opts transcribe.STTOptions) (transcribe.STTResult, error) {
	res := transcribe.STTResult{Provider: plan.Provider, Model: plan.Model}
	ext, ok := uploadExt[unit.MediaType]
	if !ok {
		return res, fmt.Errorf("the speech-to-text endpoint does not read %s", unit.MediaType)
	}
	if unit.Size() > plan.MaxBytes {
		return res, fmt.Errorf("one speech-to-text request may carry at most %d bytes; this one is %d", plan.MaxBytes, unit.Size())
	}
	prov, err := e.provider(plan.Provider)
	if err != nil {
		return res, fmt.Errorf("the speech-to-text provider %q is not available: %w", plan.Provider, err)
	}
	at, ok := providers.As[providers.AudioTranscriber](prov)
	if !ok {
		return res, fmt.Errorf("the provider %q does not serve speech-to-text", plan.Provider)
	}
	data, err := io.ReadAll(io.NewSectionReader(unit, 0, unit.Size()))
	if err != nil {
		return res, fmt.Errorf("reading the audio: %w", err)
	}
	f := format(plan, opts)
	fields := map[string]string{"response_format": f, "language": opts.Language}
	if f == "diarized_json" {
		// The diarizing model takes no prompt, and needs a chunking strategy
		// for anything over 30 seconds.
		fields["chunking_strategy"] = "auto"
	} else {
		fields["prompt"] = opts.Vocabulary
	}

	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	// From here the request may have reached the endpoint: what it carried
	// may be billed, also when the call fails.
	res.Reached = true
	resp, err := at.TranscribeAudio(callCtx, providers.AudioRequest{Model: plan.Model, File: data, Filename: "audio" + ext, Fields: fields})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return res, fmt.Errorf("the speech-to-text request took longer than %s (transcription.stt.timeout_s)", e.timeout)
		}
		return res, err
	}
	res.CredentialSource, res.CredentialScopeID = resp.CredentialSource, resp.CredentialScopeID
	if err := parse(resp.Body, unit.Span.End-unit.Span.Start, &res); err != nil {
		return res, err
	}
	return res, nil
}

// wireResponse is the union of the json, verbose_json and diarized_json
// answers.
type wireResponse struct {
	Text     string   `json:"text"`
	Language string   `json:"language"`
	Duration *float64 `json:"duration"`
	Segments []struct {
		Start   float64 `json:"start"`
		End     float64 `json:"end"`
		Text    string  `json:"text"`
		Speaker string  `json:"speaker"`
	} `json:"segments"`
	Usage *struct {
		Type         string  `json:"type"`
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		Seconds      float64 `json:"seconds"`
	} `json:"usage"`
}

// maxSegmentSeconds bounds a timestamp the runtime believes: a day.
const maxSegmentSeconds = 24 * 3600

// parse reads an answer into res. Times the engine reports outside [0, a day]
// or not finite are clamped, so a malformed answer cannot push a segment
// before the unit's start or far past its end. An answer without segments is
// one segment spanning the unit.
func parse(body []byte, span time.Duration, res *transcribe.STTResult) error {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return fmt.Errorf("the speech-to-text answer is not the expected JSON: %w", err)
	}
	res.Language = w.Language
	if w.Duration != nil {
		res.Duration = seconds(*w.Duration)
	}
	if u := w.Usage; u != nil && u.Type == "tokens" && u.InputTokens >= 0 && u.OutputTokens >= 0 {
		res.InputTokens, res.OutputTokens = u.InputTokens, u.OutputTokens
	}
	for _, s := range w.Segments {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		start, end := seconds(s.Start), seconds(s.End)
		if end < start {
			end = start
		}
		res.Segments = append(res.Segments, transcribe.Segment{Start: start, End: end, Speaker: strings.TrimSpace(s.Speaker), Text: text})
	}
	if len(res.Segments) == 0 {
		if text := strings.TrimSpace(w.Text); text != "" {
			end := span
			if res.Duration > 0 {
				end = res.Duration
			}
			res.Segments = transcribe.Segments{{Start: 0, End: end, Text: text}}
		}
	}
	return nil
}

func seconds(f float64) time.Duration {
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	if f > maxSegmentSeconds {
		f = maxSegmentSeconds
	}
	return time.Duration(f * float64(time.Second))
}
