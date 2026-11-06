package http

import (
	"context"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/synth"
)

// The audio routes, as GET /v1/_me and GET /v1/_speech ask about them.
const (
	audioSpeechPath         = "/v1/audio/speech"
	audioTranscriptionsPath = "/v1/audio/transcriptions"
	speechInfoPath          = "/v1/_speech"
)

// speechInfo is GET /v1/_speech: what a client needs to draw its voice
// controls. It names no engine URL, provider or model; voice ids are listed
// because a caller has to pick one.
type speechInfo struct {
	Speech        speechDirectionInfo        `json:"speech"`
	Transcription transcriptionDirectionInfo `json:"transcription"`
}

type speechDirectionInfo struct {
	Enabled         bool                          `json:"enabled"`
	DefaultLanguage string                        `json:"default_language,omitempty"`
	Languages       map[string]speechLanguageInfo `json:"languages,omitempty"`
	// Formats are wav, plus speech.format when speech.encode is set.
	Formats []string `json:"formats,omitempty"`
	// MaxCharsPerCall is the size of each piece sent to the engine, for
	// information; MaxCharsPerRequest is what one request may carry.
	MaxCharsPerCall    int     `json:"max_chars_per_call,omitempty"`
	MaxCharsPerRequest int     `json:"max_chars_per_request,omitempty"`
	MaxSecondsPerReq   float64 `json:"max_seconds_per_request,omitempty"`
}

type speechLanguageInfo struct {
	Voices       []string `json:"voices"`
	DefaultVoice string   `json:"default_voice"`
}

type transcriptionDirectionInfo struct {
	Enabled         bool   `json:"enabled"`
	DefaultLanguage string `json:"default_language,omitempty"`
	// Languages is the operator's list, when one is set; absent, any
	// supported code is taken.
	Languages      []string `json:"languages,omitempty"`
	Accepts        []string `json:"accepts,omitempty"`
	MaxUploadBytes int64    `json:"max_upload_bytes,omitempty"`
	// MaxSecondsPerCall is one engine request; MaxSecondsPerRequest what one
	// upload may last.
	MaxSecondsPerCall    int `json:"max_seconds_per_call,omitempty"`
	MaxSecondsPerRequest int `json:"max_seconds_per_request,omitempty"`
	// WholeFileMaxBytes is set when no converter is configured: the largest
	// non-WAV file the engine takes whole.
	WholeFileMaxBytes int64 `json:"whole_file_max_bytes,omitempty"`
}

// wholeFileTypes are the recording types a client is likely to send, in the
// order accepts lists them; without a converter a type is listed only when
// the engine reads it whole.
var wholeFileTypes = []string{"audio/webm", "audio/mp4", "audio/mpeg", "audio/ogg", "audio/flac", "video/mp4", "video/webm"}

// handleSpeechInfo serves GET /v1/_speech. Each direction is reported enabled
// only when its engine is on AND the caller would get past its route's gates
// (scope, run plane, the ACL question), the answer GET /v1/_me gives for the
// surface; a direction the caller cannot use is {"enabled": false} and shows
// nothing more.
func (s *Server) handleSpeechInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var out speechInfo
	if s.speechEnabled() && s.callerMayUse(ctx, audioSpeechPath) {
		out.Speech = s.speechDirection()
	}
	if s.transcriptionEnabled() && s.callerMayUse(ctx, audioTranscriptionsPath) {
		out.Transcription = s.transcriptionDirection(ctx)
	}
	writeJSONOK(w, out)
}

// callerMayUse is whether the request's principal would be admitted to
// POST path; true in open mode, where there is no principal to refuse.
func (s *Server) callerMayUse(ctx context.Context, path string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return true
	}
	return routeGatesAdmit(ctx, p, http.MethodPost, path)
}

func (s *Server) speechEnabled() bool { return s.cfg().Speech.Enabled() }

func (s *Server) transcriptionEnabled() bool {
	return s.stt != nil && s.stt.Engine != nil && s.stt.Config.STTEnabled()
}

func (s *Server) speechDirection() speechDirectionInfo {
	cfg := s.cfg().Speech
	caps := synth.OperatorCaps(cfg)
	langs := make(map[string]speechLanguageInfo, len(cfg.Voices))
	for _, lang := range synth.Languages(cfg) {
		_, voice, _, _ := cfg.VoiceFor(lang)
		langs[lang] = speechLanguageInfo{Voices: cfg.AllowedVoices(lang), DefaultVoice: voice}
	}
	return speechDirectionInfo{
		Enabled: true, DefaultLanguage: cfg.DefaultLanguage, Languages: langs,
		Formats:         synth.Formats(cfg),
		MaxCharsPerCall: caps.MaxCharsPerCall, MaxCharsPerRequest: caps.MaxCharsPerRun, MaxSecondsPerReq: caps.MaxSecondsPerRun,
	}
}

func (s *Server) transcriptionDirection(ctx context.Context) transcriptionDirectionInfo {
	op := s.stt.Config.STT
	caps := transcribe.Effective(s.stt.Config, config.AgentTranscription{})
	out := transcriptionDirectionInfo{
		Enabled: true, DefaultLanguage: op.Language, Languages: op.Languages, MaxUploadBytes: s.maxRequestBytes(),
		MaxSecondsPerCall: caps.MaxMinutesPerCall * 60, MaxSecondsPerRequest: caps.MaxMinutesPerRun * 60,
	}
	if op.Media != nil {
		// With the converter, any audio or video type is taken.
		out.Accepts = []string{"audio/wav", "audio/*", "video/*"}
		return out
	}
	out.Accepts = []string{"audio/wav"}
	for _, mt := range wholeFileTypes {
		if s.stt.Engine.Accepts(mt) {
			out.Accepts = append(out.Accepts, mt)
		}
	}
	if plan, err := s.stt.Engine.Plan(ctx); err == nil {
		out.WholeFileMaxBytes = plan.MaxBytes
	}
	return out
}
