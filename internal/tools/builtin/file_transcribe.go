package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/denn-gubsky/loomcycle/internal/transcribe/sttpass"
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
func isRecording(mt string) bool { return sttpass.IsRecording(mt) }

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

// sttDeps is what the shared pass runs on for this tool: the inbox is the
// scratch root a conversion writes in.
func (f *File) sttDeps() sttpass.Deps {
	return sttpass.Deps{
		Engine: f.STT.Engine,
		Config: f.sttConfig(),
		Ledger: f.STT.Ledger,
		Scratch: func(ctx context.Context) (string, error) {
			root, _, err := effectiveRoot(ctx, inbox.Name, false)
			return root, err
		},
	}
}

// transcribeFile is op=transcribe on an open file; op=extract on a recording
// comes here too.
func (f *File) transcribeFile(ctx context.Context, call sttCall, r dataRef, rel string, fh *os.File, size int64, sniffed string) (tools.Result, error) {
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(fh, 0, size)); err != nil {
		return errTransient(fmt.Sprintf("transcribe: reading %s failed", r.raw), "Try again."), nil
	}
	origin := "extract:" + sourceOrigin(ctx, r, rel, hex.EncodeToString(sum.Sum(nil)))
	deps := f.sttDeps()
	plan, err := sttpass.Prepare(ctx, deps, call.caps, fh, size, sniffed)
	if err != nil {
		return sttPrepareRefusal(r.raw, err), nil
	}
	if call.dryRun {
		return f.sttDryRun(ctx, call, plan.Engine, plan.Route, r.raw, plan.Pieces, plan.Exact)
	}
	if plan.Route == sttpass.RouteCut && len(plan.Pieces) == 0 {
		return errValidation(fmt.Sprintf("transcribe: %s holds no audio", r.raw), ""), nil
	}
	var pass *sttpass.Pass
	if run, tree, ok := f.sttBudgetsFor(ctx, call.caps); ok {
		pass = sttpass.Run(ctx, deps, plan, fh, call.opts, call.caps, []*transcribe.Budget{run, tree})
	} else {
		pass = sttpass.Failed(plan, errors.New("this run has no inbox to count its minutes against"))
	}
	return f.finishSTT(ctx, call, pass, r, rel, plan.MediaType, origin)
}

// sttPrepareRefusal words a recording the pass would not plan.
func sttPrepareRefusal(src string, err error) tools.Result {
	var pe *sttpass.PrepareError
	if !errors.As(err, &pe) {
		return errBusiness("transcribe: speech-to-text cannot run: "+err.Error(), "")
	}
	switch pe.Kind {
	case sttpass.Unreadable:
		return errValidation(fmt.Sprintf("transcribe: %s could not be read as a recording: %s", src, pe.Detail),
			"Check the file is not damaged.")
	case sttpass.NotRecording:
		return errValidation(fmt.Sprintf("transcribe: %s is not an audio or video file (%s)", src, pe.MediaType),
			"Transcribe reads audio and video: M4A, MP3, WAV, FLAC, OGG/Opus, MP4, WebM. For a document use op=extract.")
	case sttpass.EngineUnavailable:
		return errBusiness("transcribe: speech-to-text cannot run: "+pe.Detail, "An operator configures the speech engine (transcription.stt).")
	}
	return errBusiness(fmt.Sprintf("transcribe: %s cannot be sent whole: %s, and no media converter is set up to cut it (transcription.stt.media.command)", src, pe.Detail),
		"An operator sets transcription.stt.media.command (e.g. ffmpeg -i {in} -ac 1 -ar 16000 -f wav {out}); the loomcycle-media image has ffmpeg.")
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

// ---- the transcript ----

// finishSTT writes what the pass transcribed (also a partial pass) to the
// inbox, reports the pass, and answers the call.
func (f *File) finishSTT(ctx context.Context, call sttCall, p *sttpass.Pass, r dataRef, rel, mt, origin string) (tools.Result, error) {
	name := filepath.Base(rel)
	ref := ""
	var excerpt string
	var truncated bool
	if p.Done > 0 {
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
			tools.EventEmitter(ctx)(sttEvent(ctx, p, r.raw, ""))
			return errBusiness("transcribe: the transcript could not be kept: "+kept.Stub, "Free inbox space with op=rm or op=clear, then try again."), nil
		}
		ref = kept.Record.Ref
		excerpt, truncated = cutRunes(md, fileExcerptChars)
	}
	tools.EventEmitter(ctx)(sttEvent(ctx, p, r.raw, ref))
	if p.Failure() != nil {
		return sttFailure(p, ref), nil
	}
	out := map[string]any{
		"ref": ref, "source": r.raw, "media_type": mt, "route": string(p.Route),
		"minutes": round1(p.Seconds / 60), "stt": sttSummary(p),
		"excerpt": fileContentBlock(ref, excerpt, truncated),
	}
	if call.opts.Diarize == transcribe.DiarizeOn && !p.Plan.Diarizes {
		out["notes"] = []string{"the speech model does not label speakers"}
	}
	return okJSONRaw(out)
}

func round1(x float64) float64 { return float64(int64(x*10+0.5)) / 10 }

// sttMarkdown renders the transcript: provenance under the title, a section
// per chunk_minutes, speaker labels when the engine gave them, each segment's
// start unless timestamps are off. Bounded like an extraction's Markdown.
func (f *File) sttMarkdown(ctx context.Context, call sttCall, p *sttpass.Pass, name string) string {
	segs := transcribe.Merge(p.Pieces, p.Results)
	cues := make([]extract.TranscriptCue, len(segs))
	for i, s := range segs {
		cues[i] = extract.TranscriptCue{Start: s.Start, End: s.End, Speaker: s.Speaker, Text: s.Text}
	}
	language := call.opts.Language
	meta := fmt.Sprintf("stt engine=%s provider=%s model=%s minutes=%.1f language=%s",
		metaValue(p.Plan.Engine), metaValue(p.Plan.Provider), metaValue(p.Plan.Model), p.Seconds/60, metaValue(language))
	if p.Speakers && len(p.Pieces) > 1 {
		meta += " speakers=per-section"
	}
	var end time.Duration
	switch n := len(p.Pieces); {
	case p.Route == sttpass.RouteWhole && p.Done == 1:
		// The piece was the header's bound; what the engine heard is the
		// recording's length.
		end = time.Duration(p.Seconds * float64(time.Second))
	case n > 0 && p.Done == n:
		end = p.Pieces[n-1].To
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
	if p.Done < len(p.Pieces) {
		fmt.Fprintf(&b, "\n\n[the transcript stops at %s: the rest was not transcribed]", extract.Clock(p.Pieces[p.Done].Start))
	}
	return b.String()
}

func sttSummary(p *sttpass.Pass) map[string]any {
	out := map[string]any{
		"engine": p.Plan.Engine, "provider": p.Plan.Provider, "model": p.Plan.Model,
		"units": p.Done, "units_planned": len(p.Pieces), "seconds": round1(p.Seconds), "status": p.Status(),
		"speakers": p.Speakers, "input_tokens": p.InputTokens, "output_tokens": p.OutputTokens,
	}
	if p.Language != "" {
		out["language_detected"] = p.Language
	}
	if p.Unrecorded > 0 {
		out["usage_unrecorded"] = p.Unrecorded
	}
	if p.Priced {
		out["cost"], out["currency"] = p.Cost, p.Currency
	}
	return out
}

// event reports the pass: the seconds counted for the requests sent, the
// tokens they reported, and why a pass stopped.
func sttEvent(ctx context.Context, p *sttpass.Pass, src, out string) providers.Event {
	info := &providers.TranscriptionInfo{
		Ref: src, Out: out, Purpose: transcribe.PurposeSTT, Engine: p.Plan.Engine,
		Provider: p.Plan.Provider, Model: p.Plan.Model, Seconds: round1(p.Seconds),
		InputTokens: p.InputTokens, OutputTokens: p.OutputTokens, Status: p.Status(), RunID: tools.RunID(ctx),
	}
	if info.Status != "ok" {
		if err := p.Failure(); err != nil {
			info.Error = eventError(err.Error())
		}
	}
	if p.Priced {
		c := p.Cost
		info.Cost, info.Currency = &c, p.Currency
	}
	return providers.Event{Type: providers.EventTranscription, Transcription: info}
}

// sttFailure is the structured error of a pass that stopped.
func sttFailure(p *sttpass.Pass, ref string) tools.Result {
	body := map[string]any{"error": "transcribe: stopped: " + p.Failure().Error(), "status": p.Status(),
		"units": p.Done, "units_planned": len(p.Pieces)}
	hint := "Nothing was transcribed."
	if ref != "" {
		body["ref"] = ref
		hint = "The transcript written so far is at ref."
	}
	if p.CapErr != nil {
		body["cap"], body["unit"], body["limit"], body["used"], body["requested"] = p.CapErr.Cap, p.CapErr.Unit, p.CapErr.Limit, p.CapErr.Used, p.CapErr.Requested
		hint += " The run's minute cap is reached; an operator sets it."
	}
	b, _ := json.Marshal(body)
	switch {
	case p.CapErr != nil:
		return errBusiness(string(b), hint)
	case p.CallErr != nil:
		return errTransient(string(b), hint+" Try again.")
	}
	return errBusiness(string(b), hint)
}

// sttDryRun answers what a transcription would send and cost without calling
// the engine or converting anything.
func (f *File) sttDryRun(ctx context.Context, call sttCall, plan transcribe.STTPlan, route sttpass.Route, src string, pieces []transcribe.Piece, exact bool) (tools.Result, error) {
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
