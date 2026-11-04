// Package synth turns text into one audio file through the operator's speech
// engine (RFC FK): the runtime normalises the Markdown, picks the language and
// the voice from the operator's list, cuts the text into pieces the engine
// accepts, reserves the characters and estimated seconds on the caller's
// budgets, asks the engine for WAV one piece at a time, joins the pieces with
// a pause at each paragraph end, and encodes the result when a format other
// than WAV is asked for. Each engine request is one ledger row.
//
// The pure parts (normaliser, splitter, joiner, language check, caps) are in
// internal/speech, which imports nothing of this module. This package is the
// part that calls an engine, so it sits with the other engine-calling passes.
// The speech gateway route drives it today; File op=speak will too.
package synth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/speech"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/helper"
)

// DefaultMaxBytes bounds the joined (and the encoded) audio when Deps sets no
// bound: about 90 minutes of 24 kHz 16-bit mono WAV.
const DefaultMaxBytes = 256 << 20

// ParagraphGap is the pause after the last piece of a paragraph.
const ParagraphGap = 200 * time.Millisecond

// Refusals of a request, before anything is reserved or sent. Each is wrapped
// with the detail; match with errors.Is.
var (
	// ErrEmpty: nothing speakable is left once the Markdown is normalised.
	ErrEmpty = errors.New("nothing to speak")
	// ErrLanguage: the language named has no entry in speech.voices.
	ErrLanguage = errors.New("no voice is configured for the language")
	// ErrVoice: the voice named is not in the language's list.
	ErrVoice = errors.New("the voice is not one configured for the language")
	// ErrFormat: the format is not wav, mp3 or ogg.
	ErrFormat = errors.New("unknown audio format")
	// ErrEncodeNotConfigured: the format needs speech.encode, and the encoder
	// is not set or produces another format.
	ErrEncodeNotConfigured = errors.New("the format needs speech.encode")
	// ErrDisabled: no speech engine is configured.
	ErrDisabled = errors.New("text-to-speech is not configured")
)

// Deps is what a pass runs on.
type Deps struct {
	// Config is the operator's resolved speech block.
	Config *config.SpeechConfig
	// Resolve names the provider and concrete model of a synthesizer alias,
	// read from the live configuration.
	Resolve func(ctx context.Context, alias string) (providerID, model string, err error)
	// Provider returns a registered provider by id.
	Provider func(id string) (providers.Provider, error)
	// Ledger prices each engine request and records it; nil records nothing.
	Ledger transcribe.Ledger
	// Scratch names the operator directory an encode may make its scratch
	// folder in; the encoder is fenced to it. Asked only when encoding.
	Scratch func(ctx context.Context) (string, error)
	// Slot, when set, is taken before each engine request and released after
	// it, so a caller can bound how many requests reach the engine at once.
	Slot func(ctx context.Context) (release func(), err error)
	// MaxBytes bounds the joined and the encoded audio; 0 is DefaultMaxBytes.
	MaxBytes int64
}

func (d Deps) maxBytes() int64 {
	if d.MaxBytes > 0 {
		return d.MaxBytes
	}
	return DefaultMaxBytes
}

// OperatorCaps are the speech block's caps.
func OperatorCaps(cfg *config.SpeechConfig) speech.Caps {
	if cfg == nil {
		return speech.Caps{}
	}
	return speech.Caps{
		MaxCharsPerCall:  cfg.MaxCharsPerCall,
		MaxCharsPerRun:   cfg.MaxCharsPerRun,
		MaxSecondsPerRun: float64(cfg.MaxSecondsPerRun),
	}
}

// Formats are the output formats the runtime can produce: wav always, and the
// configured format when an encoder is set. There is one encode command, and
// an operator's command may fix its codec (ffmpeg -c:a libopus), so the
// runtime cannot assume it produces any format but the one configured.
func Formats(cfg *config.SpeechConfig) []string {
	out := []string{config.SpeechFormatWAV}
	if cfg != nil && cfg.Encode != nil && cfg.Format != config.SpeechFormatWAV && cfg.Format != "" {
		out = append(out, cfg.Format)
	}
	return out
}

// Languages are the configured languages, sorted.
func Languages(cfg *config.SpeechConfig) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Voices))
	for lang := range cfg.Voices {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// Request is what a caller asks to have spoken.
type Request struct {
	// Text is Markdown or plain text.
	Text string
	// Language is a code from speech.voices; "" chooses by the text's
	// script, else speech.default_language.
	Language string
	// Voice is one of the language's voices; "" is its default voice.
	Voice string
	// Format is wav, mp3 or ogg (opus is read as ogg); "" is speech.format.
	Format string
}

// Job is a request checked and cut, before anything is reserved or sent.
type Job struct {
	Language string
	Voice    string
	// Model is the synthesizer alias that speaks the language.
	Model  string
	Format string
	Pieces []speech.Piece
	// Chars is what the pieces send, in characters.
	Chars int
	// EstSeconds is how long the audio is expected to last.
	EstSeconds float64
}

// Prepare checks a request against the operator's configuration and cuts its
// text into pieces of at most caps.MaxCharsPerCall characters.
func Prepare(cfg *config.SpeechConfig, caps speech.Caps, req Request) (Job, error) {
	if !cfg.Enabled() {
		return Job{}, ErrDisabled
	}
	format, err := checkFormat(cfg, req.Format)
	if err != nil {
		return Job{}, err
	}
	text := speech.Normalize(req.Text)
	pieces := speech.Split(text, caps.MaxCharsPerCall)
	chars := 0
	for _, p := range pieces {
		chars += utf8.RuneCountInString(p.Text)
	}
	if chars == 0 {
		return Job{}, ErrEmpty
	}
	langs := Languages(cfg)
	lang := speech.Detect(text, langs, cfg.DefaultLanguage)
	if req.Language != "" {
		l, err := config.NormalizeTranscriptionLanguage(req.Language, false)
		if err != nil {
			return Job{}, fmt.Errorf("%w: %v", ErrLanguage, err)
		}
		lang = l
	}
	model, voice, _, ok := cfg.VoiceFor(lang)
	if !ok {
		return Job{}, fmt.Errorf("%w: %q (configured: %v)", ErrLanguage, lang, langs)
	}
	if req.Voice != "" {
		allowed := cfg.AllowedVoices(lang)
		if !slices.Contains(allowed, req.Voice) {
			return Job{}, fmt.Errorf("%w: %q is not one of %v for %s", ErrVoice, req.Voice, allowed, lang)
		}
		voice = req.Voice
	}
	return Job{
		Language: lang, Voice: voice, Model: model, Format: format,
		Pieces: pieces, Chars: chars, EstSeconds: speech.EstimateSeconds(chars, speech.DefaultCharsPerSecond),
	}, nil
}

func checkFormat(cfg *config.SpeechConfig, f string) (string, error) {
	switch f {
	case "":
		f = cfg.Format
		if f == "" {
			f = config.SpeechFormatWAV
		}
	case "opus":
		f = config.SpeechFormatOgg
	case config.SpeechFormatWAV, config.SpeechFormatMP3, config.SpeechFormatOgg:
	default:
		return "", fmt.Errorf("%w: %q (wav, mp3 or ogg)", ErrFormat, f)
	}
	if !slices.Contains(Formats(cfg), f) {
		if cfg.Encode == nil {
			return "", fmt.Errorf("%w: %s is encoded from the engine's WAV by speech.encode, which is not set; ask for wav", ErrEncodeNotConfigured, f)
		}
		return "", fmt.Errorf("%w: speech.encode produces %s (speech.format), not %s; ask for wav or %s", ErrEncodeNotConfigured, cfg.Format, f, cfg.Format)
	}
	return f, nil
}

// Result is the audio of one pass and what it cost.
type Result struct {
	Audio       []byte
	ContentType string
	Format      string
	Language    string
	Voice       string
	// Model is the synthesizer alias; Provider and ProviderModel what served it.
	Model         string
	Provider      string
	ProviderModel string
	// Seconds is how long the audio lasts (pauses included); Chars what the
	// pieces sent.
	Seconds  float64
	Chars    int
	Pieces   int
	Cost     float64
	Currency string
	Priced   bool
	// Unrecorded counts requests whose ledger row could not be written.
	Unrecorded int
}

// Run reserves the job's characters and estimated seconds on every budget,
// then sends the pieces in order, joins them and encodes the result. It is all
// or nothing: on any failure no audio is returned, while the requests already
// made stay recorded in the ledger and counted on the budgets.
func Run(ctx context.Context, d Deps, job Job, budgets []*speech.Budget) (Result, error) {
	if err := reserve(budgets, job.Chars, job.EstSeconds); err != nil {
		return Result{}, err
	}
	res := Result{Format: job.Format, Language: job.Language, Voice: job.Voice, Model: job.Model, Pieces: len(job.Pieces)}
	spentChars, spentSeconds := 0, 0.0
	defer func() {
		for _, b := range budgets {
			b.ReleaseChars(job.Chars - spentChars)
			switch {
			case spentSeconds < job.EstSeconds:
				b.ReleaseSeconds(job.EstSeconds - spentSeconds)
			case spentSeconds > job.EstSeconds:
				b.AddSeconds(spentSeconds - job.EstSeconds)
			}
		}
	}()

	providerID, model, err := d.Resolve(ctx, job.Model)
	if err != nil {
		return Result{}, fmt.Errorf("the speech model %q could not be resolved: %w", job.Model, err)
	}
	prov, err := d.Provider(providerID)
	if err != nil {
		return Result{}, fmt.Errorf("the speech provider %q is not available: %w", providerID, err)
	}
	synth, ok := providers.As[providers.SpeechSynthesizer](prov)
	if !ok {
		return Result{}, fmt.Errorf("the provider %q does not serve text-to-speech; point speech.voices at a provider on the openai driver", providerID)
	}
	res.Provider, res.ProviderModel = providerID, model

	audio := make([][]byte, 0, len(job.Pieces))
	gaps := make([]time.Duration, 0, len(job.Pieces))
	for i, pc := range job.Pieces {
		a, secs, err := speakOne(ctx, d, synth, model, job.Voice, pc.Text)
		if err != nil {
			return Result{}, fmt.Errorf("piece %d of %d: %w", i+1, len(job.Pieces), err)
		}
		n := utf8.RuneCountInString(pc.Text)
		spentChars += n
		spentSeconds += secs
		chargeOne(ctx, d.Ledger, &res, a, providerID, model, n, secs)
		audio = append(audio, a.Audio)
		if i < len(job.Pieces)-1 {
			gap := time.Duration(0)
			if pc.ParagraphEnd {
				gap = ParagraphGap
			}
			gaps = append(gaps, gap)
		}
	}
	joined, _, err := speech.JoinWAV(audio, gaps, d.maxBytes())
	if err != nil {
		return Result{}, err
	}
	if total, err := speech.Duration(joined); err == nil {
		res.Seconds = total.Seconds()
	}
	res.Chars = spentChars
	if job.Format == config.SpeechFormatWAV {
		res.Audio, res.ContentType = joined, ContentType(config.SpeechFormatWAV)
		return res, nil
	}
	encoded, err := encode(ctx, d, joined, job.Format)
	if err != nil {
		return Result{}, err
	}
	res.Audio, res.ContentType = encoded, ContentType(job.Format)
	return res, nil
}

// ContentType is a format's media type.
func ContentType(format string) string {
	switch format {
	case config.SpeechFormatMP3:
		return "audio/mpeg"
	case config.SpeechFormatOgg:
		return "audio/ogg"
	}
	return "audio/wav"
}

// reserve reserves on every budget, or on none.
func reserve(budgets []*speech.Budget, chars int, seconds float64) error {
	for i, b := range budgets {
		if err := b.Reserve(chars, seconds); err != nil {
			for _, held := range budgets[:i] {
				held.ReleaseChars(chars)
				held.ReleaseSeconds(seconds)
			}
			return err
		}
	}
	return nil
}

// speakOne makes one engine request for one piece, inside the caller's slot
// when it bounds them, and measures the audio it returned.
func speakOne(ctx context.Context, d Deps, synth providers.SpeechSynthesizer, model, voice, text string) (providers.SpeechResponse, float64, error) {
	if d.Slot != nil {
		release, err := d.Slot(ctx)
		if err != nil {
			return providers.SpeechResponse{}, 0, err
		}
		defer release()
	}
	timeout := time.Duration(d.Config.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(config.SpeechDefaultTimeoutSecs) * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	a, err := synth.SynthesizeSpeech(callCtx, providers.SpeechRequest{Model: model, Voice: voice, Input: text, Format: config.SpeechFormatWAV})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return a, 0, fmt.Errorf("the speech request took longer than %s (speech.timeout_s)", timeout)
		}
		return a, 0, err
	}
	dur, err := speech.Duration(a.Audio)
	if err != nil {
		return a, 0, fmt.Errorf("the speech engine's answer is not usable WAV: %w", err)
	}
	return a, dur.Seconds(), nil
}

// chargeOne records one request in the ledger: the characters it sent and
// the seconds it produced.
func chargeOne(ctx context.Context, ledger transcribe.Ledger, res *Result, a providers.SpeechResponse, provider, model string, chars int, seconds float64) {
	if ledger == nil {
		return
	}
	price, err := ledger.Charge(ctx, transcribe.Charge{
		Purpose: transcribe.PurposeTTS, Provider: provider, Model: model,
		CredentialSource: a.CredentialSource, CredentialScopeID: a.CredentialScopeID,
		AudioSeconds: seconds, InputChars: int64(chars),
	})
	if err != nil {
		// The request is made and paid for; the row is lost. The log line
		// carries nothing from the request.
		res.Unrecorded++
		slog.Warn("speech: a text-to-speech request's usage could not be recorded")
	}
	if price.Priced {
		if res.Currency == "" {
			res.Currency = price.Currency
		}
		if price.Currency == res.Currency {
			res.Cost += price.Cost
			res.Priced = true
		}
	}
}

// encode has the operator's encoder turn the joined WAV into format, in a
// scratch folder under the caller's scratch directory. The encoder gets the
// WAV's path and the path to write, never the text; it runs without a shell,
// with a scrubbed environment, under speech.timeout_s. Every file operation
// goes through an os.Root, and the folder is removed afterwards.
func encode(ctx context.Context, d Deps, wav []byte, format string) ([]byte, error) {
	if d.Config.Encode == nil {
		return nil, fmt.Errorf("%w: %s", ErrEncodeNotConfigured, format)
	}
	if d.Scratch == nil {
		return nil, errors.New("there is no scratch directory to encode the audio in")
	}
	root, err := d.Scratch(ctx)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("scratch root: %w", err)
	}
	rootFS, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("scratch root: %w", err)
	}
	defer rootFS.Close()
	name, err := scratchName()
	if err != nil {
		return nil, err
	}
	if err := rootFS.Mkdir(name, 0o700); err != nil {
		return nil, err
	}
	defer func() { _ = rootFS.RemoveAll(name) }()
	scratch, err := rootFS.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer scratch.Close()
	if err := scratch.WriteFile("speech.wav", wav, 0o600); err != nil {
		return nil, fmt.Errorf("writing the audio for the encoder: %w", err)
	}
	outName := "speech." + format
	tmp := filepath.Join(root, name)
	timeout := time.Duration(d.Config.TimeoutSeconds) * time.Second
	if _, err := helper.Run(ctx, helper.Spec{
		Key:      "speech.encode",
		Template: d.Config.Encode.Command,
		Vars:     map[string]string{"in": filepath.Join(tmp, "speech.wav"), "out": filepath.Join(tmp, outName)},
		Root:     root,
		Dir:      tmp,
		Timeout:  timeout,
	}); err != nil {
		return nil, err
	}
	fi, err := scratch.Lstat(outName)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil, errors.New("speech.encode: the encoder wrote no file at {out}")
	}
	if fi.Size() > d.maxBytes() {
		return nil, fmt.Errorf("speech.encode: the encoded audio is %d bytes, over the %d limit", fi.Size(), d.maxBytes())
	}
	fh, err := scratch.Open(outName)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return io.ReadAll(io.LimitReader(fh, d.maxBytes()))
}

// scratchName is a fresh scratch folder name.
func scratchName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".tts-" + hex.EncodeToString(b[:]), nil
}
