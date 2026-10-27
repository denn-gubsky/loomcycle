package builtin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/helper"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/media"
)

// File op=transcribe (RFC FG): a recording's words are in its audio, so the
// runtime sends it to the operator's speech-to-text engine and writes the
// transcript into the inbox. The model names the file and, at most, a
// language, a vocabulary hint, whether to label speakers and whether to keep
// timestamps; the engine, the model, the converter and every cap are operator
// configuration, and the agent's definition decides whether speech-to-text
// may run at all (its transcription.stt).

// FileSTT is what op=transcribe needs. nil (or no engine) leaves
// speech-to-text off.
type FileSTT struct {
	// Config is the operator's transcription block, resolved.
	Config config.TranscriptionConfig
	// Engine is the configured speech engine.
	Engine transcribe.STTEngine
	// Ledger prices each engine request and records it against the run.
	Ledger transcribe.Ledger
}

const (
	// sttMaxSource caps a recording the converter is handed a copy of.
	sttMaxSource = 4 << 30
	// sttWAVType is what a cut piece is sent as.
	sttWAVType = "audio/wav"
)

// sttCall is one transcribe call's request, checked before anything is read.
type sttCall struct {
	caps       transcribe.Caps
	opts       transcribe.STTOptions
	timestamps bool
	dryRun     bool
	out        string // the transcript's file name in extracted/, "" for the source's
}

func (f *File) sttAvailable() bool {
	return f.STT != nil && f.STT.Engine != nil && f.STT.Config.STTEnabled()
}

func (f *File) sttConfig() *config.STTConfig {
	if f.STT == nil {
		return nil
	}
	return f.STT.Config.STT
}

// sttRequest checks the call's language, vocabulary, diarize and timestamps
// against the operator's configuration and the agent's setting.
func (f *File) sttRequest(ctx context.Context, in fileInput) (sttCall, tools.Result, bool) {
	if !f.sttAvailable() {
		return sttCall{}, errBusiness("transcribe: speech-to-text is not set up here",
			"An operator configures a speech engine (transcription.stt)."), false
	}
	var agent config.AgentTranscription
	if a := tools.PolicyOf(ctx).Transcription; a != nil {
		agent = *a
	}
	c := sttCall{caps: transcribe.Effective(f.STT.Config, agent), dryRun: in.DryRun, timestamps: true}
	if !c.caps.STT {
		return c, errPermission("transcribe: speech-to-text is off for this agent",
			"Whether an agent may transcribe is set in its definition (transcription.stt) by an operator."), false
	}
	op := f.sttConfig()
	c.opts = transcribe.STTOptions{Language: op.Language, Vocabulary: op.Vocabulary, Diarize: transcribe.Diarize(op.Diarize)}
	if in.Language != "" {
		lang, err := transcribe.SanitizeSTTLanguage(in.Language)
		if err != nil {
			return c, errValidation("transcribe: language: "+err.Error(), "Pass one ISO 639-1 code, e.g. uk."), false
		}
		c.opts.Language = lang
	}
	if in.Vocabulary != "" {
		v, err := transcribe.SanitizeVocabulary(in.Vocabulary)
		if err != nil {
			return c, errValidation("transcribe: vocabulary: "+err.Error(), "Pass a short list of names and terms."), false
		}
		c.opts.Vocabulary = v
	}
	if in.Diarize != nil {
		c.opts.Diarize = transcribe.DiarizeOff
		if *in.Diarize {
			c.opts.Diarize = transcribe.DiarizeOn
		}
	}
	switch in.Timestamps {
	case "", "segments":
	case "none":
		c.timestamps = false
	default:
		return c, errValidation("transcribe: timestamps must be segments or none", ""), false
	}
	if in.To != "" {
		name := inbox.SanitizeName(strings.TrimSuffix(strings.TrimSpace(in.To), ".md"))
		if name == "" {
			return c, errValidation("transcribe: to must be a file name", "Pass a name such as board-call; the transcript goes to volume:inbox:extracted/<name>.md."), false
		}
		c.out = name
	}
	return c, tools.Result{}, true
}

// isRecording reports a media type transcribe reads.
func isRecording(mt string) bool {
	return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") || mt == "application/ogg"
}

func (f *File) transcribe(ctx context.Context, in fileInput) (tools.Result, error) {
	r, err := fileRef(in.Path)
	if err != nil {
		return errValidation("transcribe: "+err.Error(), "Pass path, e.g. volume:inbox:documents/call.m4a."), nil
	}
	call, res, ok := f.sttRequest(ctx, in)
	if !ok {
		return res, nil
	}
	if !inbox.Available(ctx) {
		return errBusiness("transcribe: this run cannot keep files, so there is nowhere to write the transcript",
			"An operator must configure a dynamic_root volume for the run's inbox."), nil
	}
	vfs, rel, res, ok := openRef(ctx, r, false)
	if !ok {
		return res, nil
	}
	defer vfs.Close()
	fh, err := vfs.open(rel)
	if err != nil {
		return errNotFound(fmt.Sprintf("transcribe: no file at %s", r.raw), "List the folder with op=ls."), nil
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errValidation(fmt.Sprintf("transcribe: %s is not a file", r.raw), ""), nil
	}
	head := make([]byte, 512)
	n, _ := fh.ReadAt(head, 0)
	return f.transcribeFile(ctx, call, r, rel, fh, info.Size(), inbox.Sniff("", head[:n], filepath.Base(rel)))
}

// sttRoute is how a recording reaches the engine.
type sttRoute string

const (
	// routeWhole sends the file as it is.
	routeWhole sttRoute = "whole"
	// routeCut cuts a WAV into pieces in the runtime.
	routeCut sttRoute = "cut"
	// routeConvert has the media helper make it a WAV first, then cuts it.
	routeConvert sttRoute = "convert"
)

// sttPass is what one transcribe call did.
type sttPass struct {
	plan     transcribe.STTPlan
	route    sttRoute
	pieces   []transcribe.Piece
	results  []transcribe.Segments
	done     int     // pieces transcribed
	seconds  float64 // audio counted for the requests that were sent
	in, out  int
	cost     float64
	currency string
	priced   bool
	language string
	speakers bool
	// unrecorded counts requests whose ledger row could not be written.
	unrecorded int
	capErr     *transcribe.CapError
	callErr    error
	err        error
}

func (p *sttPass) status() string {
	switch {
	case p.capErr == nil && p.callErr == nil && p.err == nil:
		return "ok"
	case p.done > 0:
		return "partial"
	}
	return "failed"
}

func (p *sttPass) failure() error {
	switch {
	case p.capErr != nil:
		return p.capErr
	case p.callErr != nil:
		return p.callErr
	}
	return p.err
}

// transcribeFile is op=transcribe on an open file; op=extract on a recording
// comes here too.
func (f *File) transcribeFile(ctx context.Context, call sttCall, r dataRef, rel string, fh *os.File, size int64, sniffed string) (tools.Result, error) {
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(fh, 0, size)); err != nil {
		return errTransient(fmt.Sprintf("transcribe: reading %s failed", r.raw), "Try again."), nil
	}
	origin := "extract:" + sourceOrigin(ctx, r, rel, hex.EncodeToString(sum.Sum(nil)))
	probe, perr := media.Probe(fh, size)
	if perr != nil && !errors.Is(perr, media.ErrUnknown) {
		return errValidation(fmt.Sprintf("transcribe: %s could not be read as a recording: %s", r.raw, strings.TrimPrefix(perr.Error(), "media: ")),
			"Check the file is not damaged."), nil
	}
	mt := probe.MediaType
	if mt == "" {
		mt = sniffed
	}
	if !isRecording(mt) {
		return errValidation(fmt.Sprintf("transcribe: %s is not an audio or video file (%s)", r.raw, mt),
			"Transcribe reads audio and video: M4A, MP3, WAV, FLAC, OGG/Opus, MP4, WebM. For a document use op=extract."), nil
	}
	plan, err := f.STT.Engine.Plan(ctx)
	if err != nil {
		return errBusiness("transcribe: speech-to-text cannot run: "+err.Error(), "An operator configures the speech engine (transcription.stt)."), nil
	}
	op := f.sttConfig()
	perCall := time.Duration(call.caps.MaxMinutesPerCall) * time.Minute
	chunk := shorter(time.Duration(op.ChunkMinutes)*time.Minute, perCall)
	overlap := time.Duration(op.ChunkOverlapSeconds) * time.Second
	bound := probe.Bound(size)

	route := routeConvert
	switch {
	case probe.WAV != nil:
		route = routeCut
	case op.Media != nil:
	case f.STT.Engine.Accepts(mt) && size <= plan.MaxBytes && bound <= perCall:
		route = routeWhole
	default:
		why := fmt.Sprintf("the engine does not read %s whole", mt)
		switch {
		case !f.STT.Engine.Accepts(mt):
		case size > plan.MaxBytes:
			why = fmt.Sprintf("it is %d bytes and one request takes at most %d", size, plan.MaxBytes)
		default:
			why = fmt.Sprintf("it may run %s and one request takes at most %s (max_minutes_per_call)", roundMin(bound), roundMin(perCall))
		}
		return errBusiness(fmt.Sprintf("transcribe: %s cannot be sent whole: %s, and no media converter is set up to cut it (transcription.stt.media.command)", r.raw, why),
			"An operator sets transcription.stt.media.command (e.g. ffmpeg -i {in} -ac 1 -ar 16000 -f wav {out}); the loomcycle-media image has ffmpeg."), nil
	}

	var pieces []transcribe.Piece
	exact := true
	switch route {
	case routeWhole:
		// The header's claim, raised to the size floor (media.Info.Bound).
		pieces = []transcribe.Piece{{To: bound}}
		exact = false
	case routeCut:
		pieces = transcribe.Pieces(probe.Duration, pieceLength(probe.WAV, chunk, plan.MaxBytes), overlap)
	case routeConvert:
		// Not known until converted: the header's claim, for the estimate.
		exact = false
		if probe.Duration > 0 {
			pieces = transcribe.Pieces(probe.Duration, chunk, overlap)
		}
	}

	if call.dryRun {
		return f.sttDryRun(ctx, call, plan, route, r.raw, pieces, exact)
	}
	if route == routeCut && len(pieces) == 0 {
		return errValidation(fmt.Sprintf("transcribe: %s holds no audio", r.raw), ""), nil
	}

	pass := &sttPass{plan: plan, route: route}
	var source io.ReaderAt = fh
	var wav *media.WAVLayout = probe.WAV
	if route == routeConvert {
		// A header that already claims more than the run may spend is
		// refused before anything is converted.
		if err := sttFits(probe.Duration, call.caps); err != nil {
			pass.capErr = err
		} else {
			conv, cleanup, err := f.convertRecording(ctx, fh, size, mt, call.opts.Language)
			if cleanup != nil {
				defer cleanup()
			}
			switch {
			case err != nil:
				pass.err = err
			case len(transcribe.Pieces(conv.info.Duration, chunk, overlap)) == 0:
				pass.err = errors.New("the converted recording holds no audio")
			default:
				source, wav = conv.file, conv.info.WAV
				pieces = transcribe.Pieces(conv.info.Duration, pieceLength(wav, chunk, plan.MaxBytes), overlap)
			}
		}
	}
	if pass.capErr == nil && pass.err == nil {
		pass.pieces = pieces
		f.runSTT(ctx, call, pass, source, size, mt, wav)
	}
	return f.finishSTT(ctx, call, pass, r, rel, mt, origin)
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

// shorter is the shorter of two lengths. (The package's tests declare an int
// min of their own, which hides the built-in one from this file in a test
// build.)
func shorter(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// sttFits refuses a length over the per-run cap before any work.
func sttFits(d time.Duration, caps transcribe.Caps) *transcribe.CapError {
	limit := float64(caps.MaxMinutesPerRun) * 60
	if s := d.Seconds(); s > limit {
		return &transcribe.CapError{Cap: "max_minutes_per_run", Unit: "seconds", Limit: limit, Requested: s}
	}
	return nil
}

func roundMin(d time.Duration) string {
	return fmt.Sprintf("%.1f min", d.Minutes())
}

// ---- the run tree's minute budget ----

type sttBudgetKey struct{}

type sttBudgets struct {
	mu   sync.Mutex
	tree *transcribe.Budget
	runs map[string]*transcribe.Budget
}

// sttBudgetsFor returns the run's and its tree's minute budgets, kept with
// the tree's inbox state as the page budgets are.
func (f *File) sttBudgetsFor(ctx context.Context, caps transcribe.Caps) (run, tree *transcribe.Budget, ok bool) {
	op := f.sttConfig()
	v, ok := inbox.Shared(ctx, sttBudgetKey{}, func() any {
		return &sttBudgets{
			tree: transcribe.NewBudget(transcribe.Caps{MaxMinutesPerRun: op.MaxMinutesPerRun}),
			runs: map[string]*transcribe.Budget{},
		}
	})
	if !ok {
		return nil, nil, false
	}
	b := v.(*sttBudgets)
	b.mu.Lock()
	defer b.mu.Unlock()
	id := tools.RunID(ctx)
	if b.runs[id] == nil {
		b.runs[id] = transcribe.NewBudget(caps)
	}
	return b.runs[id], b.tree, true
}

// ---- the requests ----

// runSTT reserves every piece's seconds on the run's and the tree's budgets
// before the first request, then sends the pieces in order. It stops at the
// first failure; what was transcribed before it stays in the pass. After each
// request the count is set to what the request spent: the piece's own length
// for a cut piece, the engine's reported length for a file sent whole (its
// header's claim is only an estimate), and seconds of pieces never sent are
// given back.
func (f *File) runSTT(ctx context.Context, call sttCall, p *sttPass, source io.ReaderAt, size int64, mt string, wav *media.WAVLayout) {
	run, tree, ok := f.sttBudgetsFor(ctx, call.caps)
	if !ok {
		p.err = errors.New("this run has no inbox to count its minutes against")
		return
	}
	reserved := make([]float64, len(p.pieces))
	total := 0.0
	for i, pc := range p.pieces {
		reserved[i] = pc.Length().Seconds()
		total += reserved[i]
	}
	if err := run.ReserveSeconds(total); err != nil {
		p.capErr = asCapErr(err, &p.err)
		return
	}
	if err := tree.ReserveSeconds(total); err != nil {
		run.ReleaseSeconds(total)
		p.capErr = asCapErr(err, &p.err)
		return
	}
	release := func(s float64) {
		run.ReleaseSeconds(s)
		tree.ReleaseSeconds(s)
	}
	p.results = make([]transcribe.Segments, len(p.pieces))
	for i, pc := range p.pieces {
		unit, err := sttUnit(source, size, mt, wav, pc)
		if err != nil {
			p.err = err
			release(sum(reserved[i:]))
			return
		}
		res, err := f.STT.Engine.Transcribe(ctx, p.plan, unit, call.opts)
		if !res.Reached {
			release(sum(reserved[i:]))
			p.callErr = err
			if err == nil {
				p.callErr = errors.New("the speech engine sent no request")
			}
			return
		}
		// A cut piece is exactly its stretch. A file sent whole was reserved
		// at its bound; the engine's own count of what it heard replaces
		// that. Without one the bound stands: releasing down to the header's
		// claim would let a file that understates its length spend past the
		// caps one call at a time.
		spent := reserved[i]
		if p.route == routeWhole && res.Duration > 0 {
			spent = res.Duration.Seconds()
		}
		switch {
		case spent < reserved[i]:
			release(reserved[i] - spent)
		case spent > reserved[i]:
			run.AddSeconds(spent - reserved[i])
			tree.AddSeconds(spent - reserved[i])
		}
		p.seconds += spent
		f.chargeSTT(ctx, p, res, spent)
		if err != nil {
			release(sum(reserved[i+1:]))
			p.callErr = err
			return
		}
		p.results[i] = speakerNames(res.Segments)
		if res.Language != "" && p.language == "" {
			p.language = res.Language
		}
		for _, s := range res.Segments {
			if s.Speaker != "" {
				p.speakers = true
			}
		}
		p.done++
	}
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

// sttUnit is one piece's request input: the whole file, or a WAV cut.
func sttUnit(source io.ReaderAt, size int64, mt string, wav *media.WAVLayout, pc transcribe.Piece) (transcribe.Unit, error) {
	if wav == nil {
		u, err := transcribe.NewUnit(source, 0, size, mt)
		u.Span = transcribe.TimeRange{Start: pc.From, End: pc.To}
		return u, err
	}
	piece, n, err := media.Cut(source, wav, pc.From, pc.To)
	if err != nil {
		return transcribe.Unit{}, err
	}
	u, err := transcribe.NewUnit(piece, 0, n, sttWAVType)
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

// chargeSTT records one request in the ledger: its audio seconds and any
// tokens it reported.
func (f *File) chargeSTT(ctx context.Context, p *sttPass, res transcribe.STTResult, seconds float64) {
	p.in += res.InputTokens
	p.out += res.OutputTokens
	if f.STT.Ledger == nil {
		return
	}
	provider, model := res.Provider, res.Model
	if provider == "" {
		provider = p.plan.Provider
	}
	if model == "" {
		model = p.plan.Model
	}
	price, err := f.STT.Ledger.Charge(ctx, transcribe.Charge{
		Purpose: transcribe.PurposeSTT, Provider: provider, Model: model,
		CredentialSource: res.CredentialSource, CredentialScopeID: res.CredentialScopeID,
		InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, AudioSeconds: seconds,
	})
	if err != nil {
		// The request is made and paid for; the row is lost. The result
		// counts it, and the log line carries nothing from the request.
		p.unrecorded++
		slog.Warn("transcription: a speech-to-text request's usage could not be recorded; the transcribe result counts it under stt.usage_unrecorded")
	}
	if price.Priced {
		if p.currency == "" {
			p.currency = price.Currency
		}
		if price.Currency == p.currency {
			p.cost += price.Cost
			p.priced = true
		}
	}
}

// ---- conversion ----

type converted struct {
	file *os.File
	info media.Info
}

// convertRecording has the operator's media helper write the recording as a
// WAV in a scratch folder of the inbox, and opens that WAV. The helper gets
// a copy of the recording in the scratch folder, never the original's path;
// it runs without a shell, with a scrubbed environment, under
// transcription.stt.timeout_s. Every file operation goes through an os.Root,
// as for the rasterize helper. cleanup closes the WAV and removes the folder.
func (f *File) convertRecording(ctx context.Context, src io.ReaderAt, size int64, mt, language string) (converted, func(), error) {
	op := f.sttConfig()
	if op == nil || op.Media == nil {
		return converted{}, nil, errors.New("set transcription.stt.media.command to convert recordings")
	}
	if size > sttMaxSource {
		return converted{}, nil, fmt.Errorf("the recording is over %d bytes", int64(sttMaxSource))
	}
	root, _, err := effectiveRoot(ctx, inbox.Name, false)
	if err != nil {
		return converted{}, nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return converted{}, nil, fmt.Errorf("inbox root: %w", err)
	}
	inboxFS, err := os.OpenRoot(real)
	if err != nil {
		return converted{}, nil, fmt.Errorf("inbox root: %w", err)
	}
	name, err := sttScratchName()
	if err != nil {
		inboxFS.Close()
		return converted{}, nil, err
	}
	if err := inboxFS.Mkdir(name, 0o700); err != nil {
		inboxFS.Close()
		return converted{}, nil, err
	}
	var out *os.File
	cleanup := func() {
		if out != nil {
			out.Close()
		}
		_ = inboxFS.RemoveAll(name)
		inboxFS.Close()
	}
	scratch, err := inboxFS.OpenRoot(name)
	if err != nil {
		return converted{}, cleanup, err
	}
	defer scratch.Close()
	inName := "input" + recordingExt(mt)
	if err := copyNew(scratch, inName, io.NewSectionReader(src, 0, size)); err != nil {
		return converted{}, cleanup, fmt.Errorf("copying the recording for the converter: %w", err)
	}
	tmp := filepath.Join(root, name)
	timeout := time.Duration(op.TimeoutSeconds) * time.Second
	if _, err := helper.Run(ctx, helper.Spec{
		Key:      "transcription.stt.media",
		Template: op.Media.Command,
		Vars:     map[string]string{"in": filepath.Join(tmp, inName), "out": filepath.Join(tmp, "audio.wav"), "lang": language},
		Root:     root,
		Dir:      tmp,
		Timeout:  timeout,
	}); err != nil {
		return converted{}, cleanup, err
	}
	fi, err := scratch.Lstat("audio.wav")
	if err != nil || !fi.Mode().IsRegular() {
		return converted{}, cleanup, errors.New("transcription.stt.media: the converter wrote no WAV at {out}")
	}
	if out, err = scratch.Open("audio.wav"); err != nil {
		return converted{}, cleanup, err
	}
	info, err := media.Probe(out, fi.Size())
	if err != nil || info.WAV == nil {
		return converted{}, cleanup, errors.New("transcription.stt.media: the converter's output is not a PCM WAV; it must write WAV to {out} (e.g. -f wav)")
	}
	return converted{file: out, info: info}, cleanup, nil
}

// recordingExt is the extension the converter's input copy gets, so a tool
// that reads the format from the name is not misled.
func recordingExt(mt string) string {
	switch mt {
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "video/mp4":
		return ".mp4"
	case "audio/flac":
		return ".flac"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "video/webm", "audio/webm":
		return ".webm"
	case "audio/wav", "audio/wave", "audio/x-wav":
		return ".wav"
	}
	return ".bin"
}

// sttScratchName is a fresh scratch folder name, as for OCR's.
func sttScratchName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".stt-" + hex.EncodeToString(b[:]), nil
}

// copyNew copies r into a new file of dir.
func copyNew(dir *os.Root, name string, r io.Reader) error {
	fh, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fh, r); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// ---- the transcript ----

// finishSTT writes what the pass transcribed (also a partial pass) to the
// inbox, reports the pass, and answers the call.
func (f *File) finishSTT(ctx context.Context, call sttCall, p *sttPass, r dataRef, rel, mt, origin string) (tools.Result, error) {
	name := filepath.Base(rel)
	ref := ""
	var excerpt string
	var truncated bool
	if p.done > 0 {
		md := f.sttMarkdown(ctx, call, p, name)
		slug := call.out
		if slug == "" {
			slug = inbox.SanitizeName(strings.TrimSuffix(name, path.Ext(name)))
		}
		if slug == "" {
			slug = "recording"
		}
		kept := inbox.Capture(ctx, inbox.Item{Name: slug + ".md", MediaType: "text/markdown", Origin: origin, Dir: inbox.DirExtracted}, []byte(md))
		if !kept.Kept {
			tools.EventEmitter(ctx)(p.event(ctx, r.raw, ""))
			return errBusiness("transcribe: the transcript could not be kept: "+kept.Stub, "Free inbox space with op=rm or op=clear, then try again."), nil
		}
		ref = kept.Record.Ref
		excerpt, truncated = cutRunes(md, fileExcerptChars)
	}
	tools.EventEmitter(ctx)(p.event(ctx, r.raw, ref))
	if p.failure() != nil {
		return sttFailure(p, ref), nil
	}
	out := map[string]any{
		"ref": ref, "source": r.raw, "media_type": mt, "route": string(p.route),
		"minutes": round1(p.seconds / 60), "stt": p.summary(),
		"excerpt": fileContentBlock(ref, excerpt, truncated),
	}
	if call.opts.Diarize == transcribe.DiarizeOn && !p.plan.Diarizes {
		out["notes"] = []string{"the speech model does not label speakers"}
	}
	return okJSONRaw(out)
}

func round1(x float64) float64 { return float64(int64(x*10+0.5)) / 10 }

// sttMarkdown renders the transcript: provenance under the title, a section
// per chunk_minutes, speaker labels when the engine gave them, each segment's
// start unless timestamps are off. Bounded like an extraction's Markdown.
func (f *File) sttMarkdown(ctx context.Context, call sttCall, p *sttPass, name string) string {
	segs := transcribe.Merge(p.pieces, p.results)
	cues := make([]extract.TranscriptCue, len(segs))
	for i, s := range segs {
		cues[i] = extract.TranscriptCue{Start: s.Start, End: s.End, Speaker: s.Speaker, Text: s.Text}
	}
	language := call.opts.Language
	meta := fmt.Sprintf("stt engine=%s provider=%s model=%s minutes=%.1f language=%s",
		metaValue(p.plan.Engine), metaValue(p.plan.Provider), metaValue(p.plan.Model), p.seconds/60, metaValue(language))
	if p.speakers && len(p.pieces) > 1 {
		meta += " speakers=per-section"
	}
	var end time.Duration
	switch n := len(p.pieces); {
	case p.route == routeWhole && p.done == 1:
		// The piece was the header's bound; what the engine heard is the
		// recording's length.
		end = time.Duration(p.seconds * float64(time.Second))
	case n > 0 && p.done == n:
		end = p.pieces[n-1].To
	}
	var b strings.Builder
	cut := false
	_ = extract.WriteTranscript(ctx, cues, extract.TranscriptOptions{
		Title: name, Meta: meta, SectionLength: time.Duration(f.sttConfig().ChunkMinutes) * time.Minute,
		Total: end, Timestamps: call.timestamps,
	}, func(s string) error {
		if b.Len()+len(s) > fileExtractMaxMarkdown {
			cut = true
			return extract.ErrSinkFull
		}
		b.WriteString(s)
		return nil
	})
	if cut {
		b.WriteString("\n\n[the rest of the transcript was not written]")
	}
	if p.done < len(p.pieces) {
		fmt.Fprintf(&b, "\n\n[the transcript stops at %s: the rest was not transcribed]", extract.Clock(p.pieces[p.done].Start))
	}
	return b.String()
}

func (p *sttPass) summary() map[string]any {
	out := map[string]any{
		"engine": p.plan.Engine, "provider": p.plan.Provider, "model": p.plan.Model,
		"units": p.done, "units_planned": len(p.pieces), "seconds": round1(p.seconds), "status": p.status(),
		"speakers": p.speakers, "input_tokens": p.in, "output_tokens": p.out,
	}
	if p.language != "" {
		out["language_detected"] = p.language
	}
	if p.unrecorded > 0 {
		out["usage_unrecorded"] = p.unrecorded
	}
	if p.priced {
		out["cost"], out["currency"] = p.cost, p.currency
	}
	return out
}

// event reports the pass: the seconds counted for the requests sent, the
// tokens they reported, and why a pass stopped.
func (p *sttPass) event(ctx context.Context, src, out string) providers.Event {
	info := &providers.TranscriptionInfo{
		Ref: src, Out: out, Purpose: transcribe.PurposeSTT, Engine: p.plan.Engine,
		Provider: p.plan.Provider, Model: p.plan.Model, Seconds: round1(p.seconds),
		InputTokens: p.in, OutputTokens: p.out, Status: p.status(), RunID: tools.RunID(ctx),
	}
	if info.Status != "ok" {
		if err := p.failure(); err != nil {
			info.Error = eventError(err.Error())
		}
	}
	if p.priced {
		c := p.cost
		info.Cost, info.Currency = &c, p.currency
	}
	return providers.Event{Type: providers.EventTranscription, Transcription: info}
}

// sttFailure is the structured error of a pass that stopped.
func sttFailure(p *sttPass, ref string) tools.Result {
	body := map[string]any{"error": "transcribe: stopped: " + p.failure().Error(), "status": p.status(),
		"units": p.done, "units_planned": len(p.pieces)}
	hint := "Nothing was transcribed."
	if ref != "" {
		body["ref"] = ref
		hint = "The transcript written so far is at ref."
	}
	if p.capErr != nil {
		body["cap"], body["unit"], body["limit"], body["used"], body["requested"] = p.capErr.Cap, p.capErr.Unit, p.capErr.Limit, p.capErr.Used, p.capErr.Requested
		hint += " The run's minute cap is reached; an operator sets it."
	}
	b, _ := json.Marshal(body)
	switch {
	case p.capErr != nil:
		return errBusiness(string(b), hint)
	case p.callErr != nil:
		return errTransient(string(b), hint+" Try again.")
	}
	return errBusiness(string(b), hint)
}

// sttDryRun answers what a transcription would send and cost without calling
// the engine or converting anything.
func (f *File) sttDryRun(ctx context.Context, call sttCall, plan transcribe.STTPlan, route sttRoute, src string, pieces []transcribe.Piece, exact bool) (tools.Result, error) {
	seconds := 0.0
	for _, pc := range pieces {
		seconds += pc.Length().Seconds()
	}
	res := map[string]any{
		"dry_run": true, "source": src, "route": string(route), "units": len(pieces),
		"engine": plan.Engine, "provider": plan.Provider, "model": plan.Model,
		"diarize": plan.Diarizes && call.opts.Diarize != transcribe.DiarizeOff,
	}
	info := &providers.TranscriptionInfo{Ref: src, Purpose: transcribe.PurposeSTT, Engine: plan.Engine,
		Provider: plan.Provider, Model: plan.Model, Status: "ok", DryRun: true, RunID: tools.RunID(ctx)}
	if len(pieces) == 0 {
		res["note"] = "the recording's length is not in its header; it is measured when converted"
	} else {
		res["est_seconds"], res["est_minutes"] = round1(seconds), round1(seconds/60)
		info.Seconds = round1(seconds)
		if !exact {
			res["estimate"] = "from the file's header and size; the engine's own count is what is charged"
		}
		if f.STT.Ledger != nil {
			if pr := f.STT.Ledger.EstimateSeconds(plan.Provider, plan.Model, seconds); pr.Priced {
				res["est_cost"], res["currency"] = pr.Cost, pr.Currency
				c := pr.Cost
				info.Cost, info.Currency = &c, pr.Currency
			}
		}
	}
	tools.EventEmitter(ctx)(providers.Event{Type: providers.EventTranscription, Transcription: info})
	return okJSON(res)
}
