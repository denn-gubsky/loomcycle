// Package sttpass is one speech-to-text pass over a recording: how it reaches
// the engine (whole, cut by the runtime, or converted by the media helper and
// then cut), the pieces it is sent in, the seconds reserved on the caller's
// budgets before the first request, one engine request per piece, and a ledger
// row for each request.
//
// Two callers drive it: the File tool (op=transcribe, and op=extract on a
// recording), which reads the recording from a run's inbox and writes the
// transcript back there, and the transcription gateway route, which reads an
// upload and answers with the text. Each keeps its own framing — where the
// recording comes from, where its scratch folder lives, which budgets count it,
// how a refusal is worded — and this package keeps the rules they share, so
// the caps and the cutting cannot drift between them.
package sttpass

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/convert"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/media"
)

// Route is how a recording reaches the engine.
type Route string

const (
	// RouteWhole sends the file as it is.
	RouteWhole Route = "whole"
	// RouteCut cuts a WAV into pieces in the runtime.
	RouteCut Route = "cut"
	// RouteConvert has the media helper make it a WAV first, then cuts it.
	RouteConvert Route = "convert"
)

const (
	// MaxSource caps a recording the converter is handed a copy of.
	MaxSource = 4 << 30
	// wavType is what a cut piece is sent as.
	wavType = "audio/wav"
)

// Deps is what a pass runs on.
type Deps struct {
	// Engine is the configured speech engine.
	Engine transcribe.STTEngine
	// Config is the operator's resolved transcription.stt block.
	Config *config.STTConfig
	// Ledger prices each engine request and records it; nil records nothing.
	Ledger transcribe.Ledger
	// Scratch names the operator directory a conversion may make its scratch
	// folder in; the converter is fenced to it. It is asked only when a
	// recording is converted.
	Scratch func(ctx context.Context) (string, error)
	// Convert, when set, converts recordings instead of the media helper run
	// in this process (RFC GO); Config.Media and Scratch then go unused.
	Convert convert.Client
	// Slot, when set, is taken before each engine request and released after
	// it, so a caller can bound how many requests reach the engine at once.
	// An error (the caller gave up waiting) stops the pass like a failed
	// request that never left.
	Slot func(ctx context.Context) (release func(), err error)
}

// IsRecording reports a media type a pass reads.
func IsRecording(mt string) bool {
	return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") || mt == "application/ogg"
}

// Plan is how one recording will reach the engine, decided before anything is
// sent or converted.
type Plan struct {
	Engine    transcribe.STTPlan
	Route     Route
	MediaType string
	// Pieces are the stretches to send. A converted recording's are an
	// estimate from its header (none when the header claims no length) until
	// Run has converted it.
	Pieces []transcribe.Piece
	// Exact is false when the length is a header's claim or a bound from the
	// file's size rather than a count of its samples.
	Exact bool

	probe media.Info
	size  int64
}

// PrepareErrorKind says why a recording cannot be planned.
type PrepareErrorKind int

const (
	// Unreadable: the container could not be read.
	Unreadable PrepareErrorKind = iota + 1
	// NotRecording: the file is not audio or video.
	NotRecording
	// EngineUnavailable: the engine could not be resolved.
	EngineUnavailable
	// NotWhole: the engine cannot take the file whole and nothing is set up
	// to convert it.
	NotWhole
)

// PrepareError is a refusal before anything is sent. Detail is the reason in
// words, for the caller to frame.
type PrepareError struct {
	Kind      PrepareErrorKind
	MediaType string
	Detail    string
	Err       error
}

func (e *PrepareError) Error() string { return e.Detail }
func (e *PrepareError) Unwrap() error { return e.Err }

// Prepare decides how the recording in src reaches the engine: a PCM WAV is
// cut by the runtime; anything else is converted when the media helper is
// configured, or sent whole when the engine reads its type, it fits one
// request and its bound length fits one call. sniffed is the caller's guess
// at the type, used when the container probe does not know it.
func Prepare(ctx context.Context, d Deps, caps transcribe.Caps, src io.ReaderAt, size int64, sniffed string) (Plan, error) {
	probe, perr := media.Probe(src, size)
	if perr != nil && !errors.Is(perr, media.ErrUnknown) {
		return Plan{}, &PrepareError{Kind: Unreadable, Detail: strings.TrimPrefix(perr.Error(), "media: "), Err: perr}
	}
	mt := probe.MediaType
	if mt == "" {
		mt = sniffed
	}
	if !IsRecording(mt) {
		return Plan{}, &PrepareError{Kind: NotRecording, MediaType: mt, Detail: "not an audio or video file (" + mt + ")"}
	}
	engine, err := d.Engine.Plan(ctx)
	if err != nil {
		return Plan{}, &PrepareError{Kind: EngineUnavailable, MediaType: mt, Detail: err.Error(), Err: err}
	}
	op := d.Config
	perCall := time.Duration(caps.MaxMinutesPerCall) * time.Minute
	chunk := shorter(time.Duration(op.ChunkMinutes)*time.Minute, perCall)
	overlap := time.Duration(op.ChunkOverlapSeconds) * time.Second
	bound := probe.Bound(size)

	route := RouteConvert
	switch {
	case probe.WAV != nil:
		route = RouteCut
	case op.Media != nil:
	case d.Engine.Accepts(mt) && size <= engine.MaxBytes && bound <= perCall:
		route = RouteWhole
	default:
		why := fmt.Sprintf("the engine does not read %s whole", mt)
		switch {
		case !d.Engine.Accepts(mt):
		case size > engine.MaxBytes:
			why = fmt.Sprintf("it is %d bytes and one request takes at most %d", size, engine.MaxBytes)
		default:
			why = fmt.Sprintf("it may run %s and one request takes at most %s (max_minutes_per_call)", roundMin(bound), roundMin(perCall))
		}
		return Plan{}, &PrepareError{Kind: NotWhole, MediaType: mt, Detail: why}
	}

	plan := Plan{Engine: engine, Route: route, MediaType: mt, Exact: true, probe: probe, size: size}
	switch route {
	case RouteWhole:
		// The header's claim, raised to the size floor (media.Info.Bound).
		plan.Pieces = []transcribe.Piece{{To: bound}}
		plan.Exact = false
	case RouteCut:
		plan.Pieces = transcribe.Pieces(probe.Duration, pieceLength(probe.WAV, chunk, engine.MaxBytes), overlap)
	case RouteConvert:
		// Not known until converted: the header's claim, for the estimate.
		plan.Exact = false
		if probe.Duration > 0 {
			plan.Pieces = transcribe.Pieces(probe.Duration, chunk, overlap)
		}
	}
	return plan, nil
}

// Pass is what one pass did. Results holds a piece's segments, with speakers
// renamed Speaker 1, Speaker 2 … in the order they first speak.
type Pass struct {
	Plan    transcribe.STTPlan
	Route   Route
	Pieces  []transcribe.Piece
	Results []transcribe.Segments
	// Done counts the pieces transcribed.
	Done int
	// Seconds is the audio counted for the requests that were sent.
	Seconds                   float64
	InputTokens, OutputTokens int
	Cost                      float64
	Currency                  string
	Priced                    bool
	// Language is the first language an engine reported.
	Language string
	Speakers bool
	// Unrecorded counts requests whose ledger row could not be written.
	Unrecorded int
	CapErr     *transcribe.CapError
	CallErr    error
	Err        error
}

// Failed is a pass that stopped before its first request, for a refusal the
// caller found itself.
func Failed(plan Plan, err error) *Pass {
	return &Pass{Plan: plan.Engine, Route: plan.Route, Err: err}
}

// Status is ok, partial (some pieces were transcribed before it stopped) or
// failed.
func (p *Pass) Status() string {
	switch {
	case p.CapErr == nil && p.CallErr == nil && p.Err == nil:
		return "ok"
	case p.Done > 0:
		return "partial"
	}
	return "failed"
}

// Failure is why the pass stopped; nil when it did not.
func (p *Pass) Failure() error {
	switch {
	case p.CapErr != nil:
		return p.CapErr
	case p.CallErr != nil:
		return p.CallErr
	}
	return p.Err
}

// Run converts the recording when the plan says so, then sends its pieces in
// order, counting them against every budget in budgets. It stops at the first
// failure; what was transcribed before it stays in the pass.
func Run(ctx context.Context, d Deps, plan Plan, src io.ReaderAt, opts transcribe.STTOptions, caps transcribe.Caps, budgets []*transcribe.Budget) *Pass {
	op := d.Config
	perCall := time.Duration(caps.MaxMinutesPerCall) * time.Minute
	chunk := shorter(time.Duration(op.ChunkMinutes)*time.Minute, perCall)
	overlap := time.Duration(op.ChunkOverlapSeconds) * time.Second

	pass := &Pass{Plan: plan.Engine, Route: plan.Route}
	pieces := plan.Pieces
	source := src
	wav := plan.probe.WAV
	if plan.Route == RouteConvert {
		// A header that already claims more than the caller may spend is
		// refused before anything is converted.
		if err := fits(plan.probe.Duration, caps); err != nil {
			pass.CapErr = err
			return pass
		}
		conv, cleanup, err := convertRecording(ctx, d, src, plan.size, plan.MediaType, opts.Language)
		if cleanup != nil {
			defer cleanup()
		}
		switch {
		case err != nil:
			pass.Err = err
			return pass
		case len(transcribe.Pieces(conv.info.Duration, chunk, overlap)) == 0:
			pass.Err = errors.New("the converted recording holds no audio")
			return pass
		}
		source, wav = conv.file, conv.info.WAV
		pieces = transcribe.Pieces(conv.info.Duration, pieceLength(wav, chunk, plan.Engine.MaxBytes), overlap)
	}
	pass.Pieces = pieces
	run(ctx, d, pass, source, plan.size, plan.MediaType, wav, opts, budgets)
	return pass
}

// run reserves every piece's seconds on the budgets before the first request,
// then sends the pieces in order. After each request the count is set to what
// the request spent: the piece's own length for a cut piece, the engine's
// reported length for a file sent whole (its header's claim is only an
// estimate), and seconds of pieces never sent are given back.
func run(ctx context.Context, d Deps, p *Pass, source io.ReaderAt, size int64, mt string, wav *media.WAVLayout, opts transcribe.STTOptions, budgets []*transcribe.Budget) {
	reserved := make([]float64, len(p.Pieces))
	total := 0.0
	for i, pc := range p.Pieces {
		reserved[i] = pc.Length().Seconds()
		total += reserved[i]
	}
	for i, b := range budgets {
		if err := b.ReserveSeconds(total); err != nil {
			for _, held := range budgets[:i] {
				held.ReleaseSeconds(total)
			}
			p.CapErr = asCapErr(err, &p.Err)
			return
		}
	}
	release := func(s float64) {
		for _, b := range budgets {
			b.ReleaseSeconds(s)
		}
	}
	p.Results = make([]transcribe.Segments, len(p.Pieces))
	for i, pc := range p.Pieces {
		unit, err := pieceUnit(source, size, mt, wav, pc)
		if err != nil {
			p.Err = err
			release(sum(reserved[i:]))
			return
		}
		res, err := transcribeOne(ctx, d, p.Plan, unit, opts)
		if !res.Reached {
			release(sum(reserved[i:]))
			p.CallErr = err
			if err == nil {
				p.CallErr = errors.New("the speech engine sent no request")
			}
			return
		}
		// A cut piece is exactly its stretch. A file sent whole was reserved
		// at its bound; the engine's own count of what it heard replaces
		// that. Without one the bound stands: releasing down to the header's
		// claim would let a file that understates its length spend past the
		// caps one call at a time.
		spent := reserved[i]
		if p.Route == RouteWhole && res.Duration > 0 {
			spent = res.Duration.Seconds()
		}
		switch {
		case spent < reserved[i]:
			release(reserved[i] - spent)
		case spent > reserved[i]:
			for _, b := range budgets {
				b.AddSeconds(spent - reserved[i])
			}
		}
		p.Seconds += spent
		charge(ctx, d.Ledger, p, res, spent)
		if err != nil {
			release(sum(reserved[i+1:]))
			p.CallErr = err
			return
		}
		p.Results[i] = speakerNames(res.Segments)
		if res.Language != "" && p.Language == "" {
			p.Language = res.Language
		}
		for _, s := range res.Segments {
			if s.Speaker != "" {
				p.Speakers = true
			}
		}
		p.Done++
	}
}

// transcribeOne makes one engine request, inside the caller's slot when it
// bounds them. A slot not granted is a request that never left.
func transcribeOne(ctx context.Context, d Deps, plan transcribe.STTPlan, unit transcribe.Unit, opts transcribe.STTOptions) (transcribe.STTResult, error) {
	if d.Slot != nil {
		release, err := d.Slot(ctx)
		if err != nil {
			return transcribe.STTResult{Provider: plan.Provider, Model: plan.Model}, err
		}
		defer release()
	}
	return d.Engine.Transcribe(ctx, plan, unit, opts)
}

func asCapErr(err error, other *error) *transcribe.CapError {
	var ce *transcribe.CapError
	if errors.As(err, &ce) {
		return ce
	}
	*other = err
	return nil
}

func sum(xs []float64) float64 {
	t := 0.0
	for _, x := range xs {
		t += x
	}
	return t
}

// pieceUnit is one piece's request input: the whole file, or a WAV cut.
func pieceUnit(source io.ReaderAt, size int64, mt string, wav *media.WAVLayout, pc transcribe.Piece) (transcribe.Unit, error) {
	if wav == nil {
		u, err := transcribe.NewUnit(source, 0, size, mt)
		u.Span = transcribe.TimeRange{Start: pc.From, End: pc.To}
		return u, err
	}
	piece, n, err := media.Cut(source, wav, pc.From, pc.To)
	if err != nil {
		return transcribe.Unit{}, err
	}
	u, err := transcribe.NewUnit(piece, 0, n, wavType)
	u.Span = transcribe.TimeRange{Start: pc.From, End: pc.To}
	return u, err
}

// speakerNames renames a piece's speaker labels "Speaker 1", "Speaker 2" …
// in the order they first speak: an engine's labels (A, B, or its own ids)
// mean nothing to a reader.
func speakerNames(segs transcribe.Segments) transcribe.Segments {
	names := map[string]string{}
	out := make(transcribe.Segments, len(segs))
	for i, s := range segs {
		if s.Speaker != "" {
			n, ok := names[s.Speaker]
			if !ok {
				n = fmt.Sprintf("Speaker %d", len(names)+1)
				names[s.Speaker] = n
			}
			s.Speaker = n
		}
		out[i] = s
	}
	return out
}

// charge records one request in the ledger: its audio seconds and any tokens
// it reported.
func charge(ctx context.Context, ledger transcribe.Ledger, p *Pass, res transcribe.STTResult, seconds float64) {
	p.InputTokens += res.InputTokens
	p.OutputTokens += res.OutputTokens
	if ledger == nil {
		return
	}
	provider, model := res.Provider, res.Model
	if provider == "" {
		provider = p.Plan.Provider
	}
	if model == "" {
		model = p.Plan.Model
	}
	price, err := ledger.Charge(ctx, transcribe.Charge{
		Purpose: transcribe.PurposeSTT, Provider: provider, Model: model,
		CredentialSource: res.CredentialSource, CredentialScopeID: res.CredentialScopeID,
		InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, AudioSeconds: seconds,
	})
	if err != nil {
		// The request is made and paid for; the row is lost. The pass counts
		// it, and the log line carries nothing from the request.
		p.Unrecorded++
		slog.Warn("transcription: a speech-to-text request's usage could not be recorded; the pass counts it under usage_unrecorded")
	}
	if price.Priced {
		if p.Currency == "" {
			p.Currency = price.Currency
		}
		if price.Currency == p.Currency {
			p.Cost += price.Cost
			p.Priced = true
		}
	}
}

// pieceLength is chunk, shortened so a piece of this WAV fits one request.
func pieceLength(lay *media.WAVLayout, chunk time.Duration, maxBytes int64) time.Duration {
	if lay == nil || lay.ByteRate <= 0 {
		return chunk
	}
	room := maxBytes - media.HeaderBytes(lay)
	fit := time.Duration(float64(room) / float64(lay.ByteRate) * float64(time.Second))
	if fit = shorter(chunk, fit); fit < time.Second {
		return time.Second
	}
	return fit
}

func shorter(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// fits refuses a length over the per-run cap before any work.
func fits(d time.Duration, caps transcribe.Caps) *transcribe.CapError {
	limit := float64(caps.MaxMinutesPerRun) * 60
	if s := d.Seconds(); s > limit {
		return &transcribe.CapError{Cap: "max_minutes_per_run", Unit: "seconds", Limit: limit, Requested: s}
	}
	return nil
}

func roundMin(d time.Duration) string {
	return fmt.Sprintf("%.1f min", d.Minutes())
}

// ---- conversion ----

type converted struct {
	file convert.Spool
	info media.Info
}

// converter is the client a conversion runs on: the one the caller gave, or
// the operator's media helper run in this process, in a scratch folder under
// the caller's scratch directory.
func (d Deps) converter() convert.Client {
	if d.Convert != nil {
		return d.Convert
	}
	p := &convert.InProcess{Scratch: d.Scratch}
	if d.Config != nil && d.Config.Media != nil {
		p.Media = &convert.Helper{Key: "transcription.stt.media", Command: d.Config.Media.Command, Wrapper: d.Config.Media.Wrapper,
			Timeout: time.Duration(d.Config.TimeoutSeconds) * time.Second}
	}
	return p
}

// convertRecording has the recording converted to a WAV and checks what came back is
// one. The converter is handed the recording's bytes, never its path; in this
// process it gets a copy in a scratch folder and runs without a shell, with a
// scrubbed environment, under transcription.stt.timeout_s. cleanup closes the
// WAV and removes what holds it.
func convertRecording(ctx context.Context, d Deps, src io.ReaderAt, size int64, mt, language string) (converted, func(), error) {
	if size > MaxSource {
		return converted{}, nil, fmt.Errorf("the recording is over %d bytes", int64(MaxSource))
	}
	out, err := d.converter().ConvertMedia(ctx, src, size, mt, language)
	switch {
	case errors.Is(err, convert.ErrNotConfigured):
		return converted{}, nil, errors.New("set transcription.stt.media.command to convert recordings")
	case err != nil:
		return converted{}, nil, err
	}
	cleanup := func() { _ = out.Close() }
	info, err := media.Probe(out, out.Size())
	if err != nil || info.WAV == nil {
		return converted{}, cleanup, errors.New("transcription.stt.media: the converter's output is not a PCM WAV; it must write WAV to {out} (e.g. -f wav)")
	}
	return converted{file: out, info: info}, cleanup, nil
}
