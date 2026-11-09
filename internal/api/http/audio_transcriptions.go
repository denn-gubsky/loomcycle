package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/sttpass"
)

const (
	transcriptionCodeUnreadable     = "unreadable_audio"
	transcriptionCodeUnsupported    = "unsupported_media_type"
	transcriptionCodeTooLarge       = "audio_too_large"
	transcriptionCodeTooLong        = "audio_too_long"
	transcriptionCodeUnavailable    = "transcription_unavailable"
	transcriptionCodeFailed         = "transcription_failed"
	transcriptionCodeBadFormat      = "unsupported_response_format"
	transcriptionCodeBadLanguage    = "unsupported_language"
	transcriptionCodeBadPrompt      = "bad_prompt"
	transcriptionFieldMaxBytes      = 4 << 10
	transcriptionFormatJSON         = "json"
	transcriptionFormatVerboseJSON  = "verbose_json"
	transcriptionFormatText         = "text"
	transcriptionMediaHelperSetting = "transcription.stt.media.command"
)

// transcriptionForm is POST /v1/audio/transcriptions' multipart form, read
// into a scratch folder: the recording as a file, the rest as short fields.
type transcriptionForm struct {
	path, filename, declared string
	size                     int64
	model, language, prompt  string
	format                   string
}

// handleAudioTranscriptions serves POST /v1/audio/transcriptions: a recording
// in, its transcript out, outside any run (RFC FK P3). It runs the pass the
// File tool's op=transcribe runs (internal/transcribe/sttpass) on the same
// engine and ledger:
//
//   - a PCM WAV is cut to the engine's per-request size;
//   - with transcription.stt.media.command set, any audio or video is
//     converted to WAV first;
//   - without it, a type the engine reads whole (webm, mp4/m4a, mp3, ogg,
//     flac) is sent whole when it fits one request (P-D4).
//
// One request may carry up to transcription.stt.max_minutes_per_run of audio
// and LOOMCYCLE_MAX_REQUEST_BYTES of upload (P-D2). Each engine request takes
// a slot of the transcription queue. It is all or nothing: a pass that stops
// part-way returns no transcript, and the requests it made stay charged.
func (s *Server) handleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	stt := s.stt
	if stt == nil || stt.Engine == nil || !stt.Config.STTEnabled() {
		writeJSONError(w, http.StatusServiceUnavailable, transcriptionCodeNotConfigured,
			"speech-to-text is not configured on this deployment (transcription.stt)")
		return
	}
	caps := transcribe.Effective(stt.Config, config.AgentTranscription{})
	// Admitted (rate, budget, per-user cap) before the upload is read: a
	// caller over its rate or budget must not get the body spooled to disk,
	// nor hold a connection while it uploads.
	ctx, release, err := s.admitOffRunCall(r.Context(), "a transcription", auth.SurfaceTranscription)
	if err != nil {
		writeOffRunAdmissionError(w, err)
		return
	}
	defer release()
	scratch, err := os.MkdirTemp("", "loomcycle-stt-")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "a scratch folder for the upload could not be made")
		return
	}
	defer os.RemoveAll(scratch)

	form, err := readTranscriptionForm(http.MaxBytesReader(w, r.Body, s.maxRequestBytes()), r.Header.Get("Content-Type"), scratch)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeBodyReadError(w, err)
			return
		}
		writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	opts, format, ok := s.transcriptionOptions(w, stt.Config.STT, form)
	if !ok {
		return
	}

	src, err := os.Open(form.path)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", "the upload could not be reopened")
		return
	}
	defer src.Close()
	head := make([]byte, 512)
	n, _ := src.ReadAt(head, 0)
	sniffed := inbox.Sniff(form.declared, head[:n], form.filename)

	_, queue := s.audioQueues()
	deps := sttpass.Deps{
		Engine:  stt.Engine,
		Config:  stt.Config.STT,
		Ledger:  stt.Ledger,
		Scratch: func(context.Context) (string, error) { return scratch, nil },
		Slot:    queue.take,
	}
	plan, err := sttpass.Prepare(ctx, deps, caps, src, form.size, sniffed)
	if err != nil {
		writeTranscriptionPrepareError(ctx, w, stt.Engine, err)
		return
	}
	p := sttpass.Run(ctx, deps, plan, src, opts, caps, []*transcribe.Budget{transcribe.NewBudget(caps)})
	if fail := p.Failure(); fail != nil {
		if r.Context().Err() != nil {
			return
		}
		switch {
		case p.CapErr != nil:
			writeJSONErrorBody(w, http.StatusRequestEntityTooLarge, map[string]any{
				"code": transcriptionCodeTooLong,
				"error": fmt.Sprintf("the recording is too long for one request: transcription.stt.%s is %g %s, and this needs %g more; send it in parts",
					p.CapErr.Cap, p.CapErr.Limit, p.CapErr.Unit, p.CapErr.Requested),
				"cap": p.CapErr.Cap, "unit": p.CapErr.Unit, "limit": p.CapErr.Limit, "requested": p.CapErr.Requested,
			})
		case errors.Is(fail, providers.ErrOperatorKeyForbidden):
			writeJSONError(w, http.StatusForbidden, "operator_key_restricted", operatorKeyRestrictedMsg)
		default:
			// As for read-aloud: the engine's error stays in the log.
			slog.WarnContext(ctx, "audio: speech-to-text failed", "err", fail, "status", p.Status(), "pieces", len(p.Pieces), "done", p.Done)
			writeJSONError(w, http.StatusBadGateway, transcriptionCodeFailed, "the speech engine failed; try again later")
		}
		return
	}
	writeTranscript(w, format, p, opts.Language)
}

// readTranscriptionForm streams the multipart body: the file part into a file
// in dir, the known fields into short strings. Unknown fields (an OpenAI
// client's temperature, timestamp_granularities[]) are skipped. The body's
// size bound is the caller's MaxBytesReader.
func readTranscriptionForm(body io.Reader, contentType, dir string) (transcriptionForm, error) {
	var f transcriptionForm
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return f, errors.New("the body must be multipart/form-data with a file part")
	}
	mr := multipart.NewReader(body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return f, err
		}
		name := part.FormName()
		switch name {
		case "file":
			if f.path != "" {
				return f, errors.New("the form carries more than one file")
			}
			out, err := os.CreateTemp(dir, "upload-*")
			if err != nil {
				return f, err
			}
			n, err := io.Copy(out, part)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return f, err
			}
			f.path, f.size, f.filename, f.declared = out.Name(), n, part.FileName(), part.Header.Get("Content-Type")
		case "model", "language", "prompt", "response_format":
			v, err := io.ReadAll(io.LimitReader(part, transcriptionFieldMaxBytes+1))
			if err != nil {
				return f, err
			}
			if len(v) > transcriptionFieldMaxBytes {
				return f, fmt.Errorf("the %s field is longer than %d bytes", name, transcriptionFieldMaxBytes)
			}
			val := strings.TrimSpace(string(v))
			switch name {
			case "model":
				f.model = val
			case "language":
				f.language = val
			case "prompt":
				f.prompt = val
			case "response_format":
				f.format = val
			}
		default:
			if _, err := io.Copy(io.Discard, part); err != nil {
				return f, err
			}
		}
	}
	if f.path == "" {
		return f, errors.New("the form has no file part")
	}
	if f.size == 0 {
		return f, errors.New("the file is empty")
	}
	return f, nil
}

// transcriptionOptions checks the form's fields against the operator's
// configuration and writes the refusal itself when one is wrong.
func (s *Server) transcriptionOptions(w http.ResponseWriter, op *config.STTConfig, f transcriptionForm) (transcribe.STTOptions, string, bool) {
	opts := transcribe.STTOptions{Language: op.Language, Vocabulary: op.Vocabulary, Diarize: transcribe.Diarize(op.Diarize)}
	format := f.format
	switch format {
	case "":
		format = transcriptionFormatJSON
	case transcriptionFormatJSON, transcriptionFormatVerboseJSON, transcriptionFormatText:
	default:
		writeJSONError(w, http.StatusBadRequest, transcriptionCodeBadFormat,
			fmt.Sprintf("response_format %q is not one of json, verbose_json, text", format))
		return opts, "", false
	}
	if f.model != "" && f.model != op.Model {
		writeJSONError(w, http.StatusBadRequest, audioCodeModelNotAllowed,
			fmt.Sprintf("model %q is not this deployment's speech-to-text model; omit model", f.model))
		return opts, "", false
	}
	if f.language != "" {
		lang, err := transcribe.CheckSTTLanguage(op, f.language)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, transcriptionCodeBadLanguage, "language: "+err.Error())
			return opts, "", false
		}
		opts.Language = lang
	}
	if f.prompt != "" {
		v, err := transcribe.SanitizeVocabulary(f.prompt)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, transcriptionCodeBadPrompt, "prompt: "+err.Error())
			return opts, "", false
		}
		opts.Vocabulary = v
	}
	return opts, format, true
}

// writeTranscriptionPrepareError answers a recording the pass will not take.
// What the caller can fix is spelled out; what only the operator can (the
// engine could not be resolved) is logged, since its detail names the
// provider and model.
func writeTranscriptionPrepareError(ctx context.Context, w http.ResponseWriter, engine transcribe.STTEngine, err error) {
	var pe *sttpass.PrepareError
	if !errors.As(err, &pe) {
		slog.WarnContext(ctx, "audio: speech-to-text cannot run", "err", err)
		writeJSONError(w, http.StatusBadGateway, transcriptionCodeFailed, "the speech engine failed; try again later")
		return
	}
	switch pe.Kind {
	case sttpass.Unreadable:
		writeJSONError(w, http.StatusBadRequest, transcriptionCodeUnreadable, "the file could not be read as a recording: "+pe.Detail)
	case sttpass.NotRecording:
		writeJSONError(w, http.StatusUnsupportedMediaType, transcriptionCodeUnsupported,
			"the file is not an audio or video recording ("+pe.MediaType+")")
	case sttpass.EngineUnavailable:
		slog.WarnContext(ctx, "audio: speech-to-text engine unavailable", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, transcriptionCodeUnavailable, "the speech engine is not available; try again later")
	default: // NotWhole: no converter, and the engine cannot take it whole.
		if !engine.Accepts(pe.MediaType) {
			writeJSONError(w, http.StatusUnsupportedMediaType, transcriptionCodeUnsupported,
				fmt.Sprintf("%s cannot be transcribed here: the engine does not read it whole and no converter is set up (%s); send a PCM WAV", pe.MediaType, transcriptionMediaHelperSetting))
			return
		}
		writeJSONError(w, http.StatusRequestEntityTooLarge, transcriptionCodeTooLarge,
			fmt.Sprintf("the recording cannot be sent whole: %s, and no converter is set up to cut it (%s); send a PCM WAV, which is cut", pe.Detail, transcriptionMediaHelperSetting))
	}
}

// transcriptSegment is one verbose_json segment: seconds from the start of
// the recording.
type transcriptSegment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker,omitempty"`
}

func writeTranscript(w http.ResponseWriter, format string, p *sttpass.Pass, requestedLanguage string) {
	segs := transcribe.Merge(p.Pieces, p.Results)
	texts := make([]string, 0, len(segs))
	for _, sg := range segs {
		if t := strings.TrimSpace(sg.Text); t != "" {
			texts = append(texts, t)
		}
	}
	text := strings.Join(texts, " ")
	if format == transcriptionFormatText {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, text)
		return
	}
	language := p.Language
	if language == "" {
		language = requestedLanguage
	}
	out := map[string]any{"text": text, "duration": math.Round(recordingLength(p).Seconds()*100) / 100}
	if language != "" {
		out["language"] = language
	}
	if format == transcriptionFormatVerboseJSON {
		list := make([]transcriptSegment, 0, len(segs))
		for _, sg := range segs {
			list = append(list, transcriptSegment{
				Start: math.Round(sg.Start.Seconds()*100) / 100, End: math.Round(sg.End.Seconds()*100) / 100,
				Text: strings.TrimSpace(sg.Text), Speaker: sg.Speaker,
			})
		}
		out["segments"] = list
	}
	writeJSONOK(w, out)
}

// recordingLength is how long the recording is, not how much audio was sent:
// cut pieces overlap, so the seconds a pass sent (and was charged for) run
// past the recording's end. A whole file's length is what the engine read.
func recordingLength(p *sttpass.Pass) time.Duration {
	if p.Route == sttpass.RouteWhole || len(p.Pieces) == 0 {
		return time.Duration(p.Seconds * float64(time.Second))
	}
	return p.Pieces[len(p.Pieces)-1].To
}
