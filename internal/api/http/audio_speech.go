package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/speech"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/synth"
)

// The codes the audio routes answer with, besides the shared ones
// (request_too_large, bad_request, the 429 bodies, operator_key_restricted).
const (
	speechCodeNotConfigured        = "speech_not_configured"
	speechCodeEncodeNotConfigured  = "speech_encode_not_configured"
	speechCodeEmptyInput           = "empty_input"
	speechCodeInputTooLong         = "input_too_long"
	speechCodeUnsupportedLanguage  = "unsupported_language"
	speechCodeUnsupportedVoice     = "unsupported_voice"
	speechCodeUnsupportedFormat    = "unsupported_format"
	audioCodeModelNotAllowed       = "model_not_allowed"
	speechCodeFailed               = "speech_failed"
	transcriptionCodeNotConfigured = "transcription_not_configured"
)

// speechRequest is POST /v1/audio/speech's body: the fields of OpenAI's
// speech endpoint the runtime honours, plus language. Other fields an OpenAI
// client sends (speed, instructions) are ignored, not refused.
type speechRequest struct {
	Input          string `json:"input"`
	Model          string `json:"model,omitempty"`
	Voice          string `json:"voice,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Language       string `json:"language,omitempty"`
}

// handleAudioSpeech serves POST /v1/audio/speech: text in, audio out, outside
// any run (RFC FK P3). The text is normalised from Markdown, cut into pieces
// the engine takes, spoken piece by piece through the engine queue, joined
// and, for ogg or mp3, encoded by the operator's speech.encode. The audio is
// returned whole with a Content-Length: nothing is streamed (P-D6), so a
// failure part-way returns no audio.
//
// One request may carry up to speech.max_chars_per_run characters and
// speech.max_seconds_per_run of estimated audio (P-D2): the per-run caps are
// the per-request caps here, as a request is the unit of work.
func (s *Server) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg().Speech
	if !cfg.Enabled() {
		writeJSONError(w, http.StatusServiceUnavailable, speechCodeNotConfigured,
			"text-to-speech is not configured on this deployment (speech.engine)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxRequestBytes()))
	if err != nil {
		writeBodyReadError(w, err)
		return
	}
	var req speechRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "body: "+err.Error())
		return
	}
	caps := synth.OperatorCaps(cfg)
	job, err := synth.Prepare(cfg, caps, synth.Request{
		Text: req.Input, Language: req.Language, Voice: req.Voice, Format: req.ResponseFormat,
	})
	if err != nil {
		writeSpeechPrepareError(w, err)
		return
	}
	if req.Model != "" {
		ref, ok := s.cfg().Models[req.Model]
		if !ok || ref.Kind != config.ModelKindSynthesizer {
			writeJSONError(w, http.StatusBadRequest, audioCodeModelNotAllowed,
				fmt.Sprintf("model %q is not a models: alias of kind synthesizer; omit model to use the language's voice", req.Model))
			return
		}
		job.Model = req.Model
	}
	// Checked here as well as by the reservation in synth.Run, so a request
	// that can never fit is refused before it takes the caller's run rate.
	if err := checkSpeechCaps(caps, job); err != nil {
		writeSpeechCapError(w, err)
		return
	}

	ctx, release, err := s.admitOffRunCall(r.Context(), "a read-aloud")
	if err != nil {
		writeOffRunAdmissionError(w, err)
		return
	}
	defer release()

	scratch := ""
	if job.Format != config.SpeechFormatWAV {
		dir, err := os.MkdirTemp("", "loomcycle-tts-")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal", "a scratch folder for encoding could not be made")
			return
		}
		defer os.RemoveAll(dir)
		scratch = dir
	}
	queue, _ := s.audioQueues()
	res, err := synth.Run(ctx, synth.Deps{
		Config:   cfg,
		Resolve:  s.resolveSynthesizer,
		Provider: func(id string) (providers.Provider, error) { return s.providers.Get(id) },
		Ledger:   transcriptionLedger{s: s},
		Scratch:  func(context.Context) (string, error) { return scratch, nil },
		Slot:     queue.take,
	}, job, []*speech.Budget{speech.NewBudget(caps)})
	if err != nil {
		if r.Context().Err() != nil {
			// The caller hung up: there is nobody to answer.
			return
		}
		var capErr *speech.CapError
		switch {
		case errors.As(err, &capErr):
			writeSpeechCapError(w, capErr)
		case errors.Is(err, providers.ErrOperatorKeyForbidden):
			writeJSONError(w, http.StatusForbidden, "operator_key_restricted", operatorKeyRestrictedMsg)
		default:
			writeJSONError(w, http.StatusBadGateway, speechCodeFailed, "text-to-speech failed: "+err.Error())
		}
		return
	}
	h := w.Header()
	h.Set("Content-Type", res.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(res.Audio)))
	h.Set("X-Loomcycle-Speech-Language", res.Language)
	h.Set("X-Loomcycle-Speech-Voice", res.Voice)
	h.Set("X-Loomcycle-Speech-Seconds", strconv.FormatFloat(res.Seconds, 'f', 2, 64))
	h.Set("X-Loomcycle-Speech-Chars", strconv.Itoa(res.Chars))
	h.Set("X-Loomcycle-Speech-Pieces", strconv.Itoa(res.Pieces))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Audio)
}

// resolveSynthesizer names the provider and concrete model of a synthesizer
// alias, read from the live configuration. Config load holds every alias in
// speech.voices to kind: synthesizer with its own provider and model; a
// caller-named alias is checked for the kind before it gets here.
func (s *Server) resolveSynthesizer(_ context.Context, alias string) (string, string, error) {
	ref, ok := s.cfg().Models[alias]
	if !ok || ref.Provider == "" || ref.Model == "" {
		return "", "", fmt.Errorf("%q is not a models: alias naming a provider and a model", alias)
	}
	return ref.Provider, ref.Model, nil
}

// checkSpeechCaps refuses a job no fresh request budget could hold.
func checkSpeechCaps(caps speech.Caps, job synth.Job) error {
	if caps.MaxCharsPerRun > 0 && job.Chars > caps.MaxCharsPerRun {
		return &speech.CapError{Cap: "max_chars_per_run", Unit: "chars", Limit: float64(caps.MaxCharsPerRun), Requested: float64(job.Chars)}
	}
	if caps.MaxSecondsPerRun > 0 && job.EstSeconds > caps.MaxSecondsPerRun {
		return &speech.CapError{Cap: "max_seconds_per_run", Unit: "seconds", Limit: caps.MaxSecondsPerRun, Requested: job.EstSeconds}
	}
	return nil
}

func writeSpeechCapError(w http.ResponseWriter, err error) {
	var capErr *speech.CapError
	if !errors.As(err, &capErr) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, speechCodeInputTooLong, err.Error())
		return
	}
	writeJSONErrorBody(w, http.StatusRequestEntityTooLarge, map[string]any{
		"code": speechCodeInputTooLong,
		"error": fmt.Sprintf("the text is too long for one request: %s is %g %s, and this needs %g; send it in parts",
			"speech."+capErr.Cap, capErr.Limit, capErr.Unit, capErr.Requested),
		"cap": capErr.Cap, "unit": capErr.Unit, "limit": capErr.Limit, "requested": capErr.Requested,
	})
}

func writeSpeechPrepareError(w http.ResponseWriter, err error) {
	code := ""
	switch {
	case errors.Is(err, synth.ErrDisabled):
		writeJSONError(w, http.StatusServiceUnavailable, speechCodeNotConfigured, err.Error())
		return
	case errors.Is(err, synth.ErrEmpty):
		code = speechCodeEmptyInput
	case errors.Is(err, synth.ErrLanguage):
		code = speechCodeUnsupportedLanguage
	case errors.Is(err, synth.ErrVoice):
		code = speechCodeUnsupportedVoice
	case errors.Is(err, synth.ErrEncodeNotConfigured):
		code = speechCodeEncodeNotConfigured
	case errors.Is(err, synth.ErrFormat):
		code = speechCodeUnsupportedFormat
	default:
		code = "bad_request"
	}
	writeJSONError(w, http.StatusBadRequest, code, err.Error())
}

// writeBodyReadError answers a request body that could not be read: 413 when
// it ran past LOOMCYCLE_MAX_REQUEST_BYTES, else 400.
func writeBodyReadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("request body exceeds the %d-byte limit (LOOMCYCLE_MAX_REQUEST_BYTES)", maxErr.Limit))
		return
	}
	writeJSONError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
}

// writeOffRunAdmissionError answers a refusal from admitOffRunCall, with the
// statuses POST /v1/_decide gives the same refusals.
func writeOffRunAdmissionError(w http.ResponseWriter, err error) {
	var over *connector.TokenLimitError
	switch {
	case errors.As(err, &over):
		writeTokenLimitError(w, &over.Info)
	case errors.Is(err, runner.ErrRateLimited):
		writeRateLimited(w, err)
	case concurrency.IsPerUserQuotaExhausted(err):
		writeQuotaError(w, err)
	case errors.Is(err, runner.ErrAgentNotAllowed):
		writeAgentNotAllowed(w, err)
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal", strings.TrimSpace(err.Error()))
	}
}
