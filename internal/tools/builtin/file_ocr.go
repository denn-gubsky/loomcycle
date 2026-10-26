package builtin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/helper"
)

// File op=extract with OCR (RFC FG): a scanned page or an image has its text
// in pixels, so the runtime sends it to the operator's OCR engine and writes
// the transcript where the page's text layer would have been. The model asks
// for it by mode and never handles a page; the engine, the model and every
// cap are operator configuration, and the agent's definition decides whether
// OCR may run at all (its transcription.ocr: never, auto, always).

// FileOCR is what op=extract needs to run OCR. nil (or no engine) leaves OCR
// off, and extract behaves as without it.
type FileOCR struct {
	// Config is the operator's transcription block, resolved.
	Config config.TranscriptionConfig
	// Engine is the configured OCR engine.
	Engine transcribe.OCREngine
	// Ledger prices each engine call and records it against the run.
	Ledger transcribe.Ledger
}

const (
	// ocrRasterizeTimeout bounds rendering one page.
	ocrRasterizeTimeout = 2 * time.Minute
	// ocrMaxPageImage caps the image a rasteriser may hand back for one page.
	ocrMaxPageImage = 64 << 20
	// The dry-run estimate: a page sent as an image or a PDF page reads as
	// about 1 600 input tokens on Claude (pixels / 750 at the 1568-pixel
	// edge), each call adds its instruction, and a page of dense text writes
	// about a thousand tokens (Cyrillic tokenises long), capped by the
	// operator's max_tokens_per_page. An estimate, not a bound.
	ocrEstInputPerPage  = 1600
	ocrEstPromptPerCall = 200
	ocrEstOutputPerPage = 1000
)

// ocrCall is one extract call's OCR request, decided before extraction.
type ocrCall struct {
	mode     transcribe.OCRMode
	caps     transcribe.Caps
	language string
	dryRun   bool
	minChars int
}

// active reports whether this call may run OCR.
func (c ocrCall) active() bool { return c.mode != transcribe.OCRNever }

// ocrRequest checks the call's ocr, language and dry_run against the agent's
// setting and the operator's configuration.
func (f *File) ocrRequest(ctx context.Context, in fileInput) (ocrCall, tools.Result, bool) {
	c := ocrCall{mode: transcribe.OCRNever, dryRun: in.DryRun, minChars: config.TranscriptionDefaultMinCharsPerPage}
	op := f.ocrConfig()
	if op != nil {
		c.minChars = op.OCRMinCharsPerPage
		c.language = op.Language
	}
	if f.ocrAvailable() {
		var agent config.AgentTranscription
		if a := tools.PolicyOf(ctx).Transcription; a != nil {
			agent = *a
		}
		c.caps = transcribe.Effective(f.OCR.Config, agent)
	}
	mode, err := transcribe.ModeFor(c.caps.OCR, in.OCR)
	if err != nil {
		return c, errValidation("extract: "+err.Error(),
			"Leave ocr out to use this agent's setting, or ask for less. Whether an agent may run OCR is set in its definition (transcription.ocr) by an operator."), false
	}
	c.mode = mode
	if in.Language != "" {
		lang, err := transcribe.SanitizeLanguage(in.Language)
		if err != nil {
			return c, errValidation("extract: language: "+err.Error(), "Pass ISO 639-1 codes, e.g. uk or uk+ru."), false
		}
		c.language = lang
	}
	return c, tools.Result{}, true
}

func (f *File) ocrAvailable() bool {
	return f.OCR != nil && f.OCR.Engine != nil && f.OCR.Config.OCREnabled()
}

func (f *File) ocrConfig() *config.OCRConfig {
	if f.OCR == nil || f.OCR.Config.OCR == nil {
		return nil
	}
	return f.OCR.Config.OCR
}

// lowYieldPages are the pages of a PDF whose text layer holds under minChars
// characters: likely scans. A page whose content could not be read at all is
// damaged, not scanned, and is not listed.
func lowYieldPages(rep extract.Report, minChars int) []int {
	var out []int
	for _, y := range rep.PageYields {
		if !y.Unread && y.Chars < minChars {
			out = append(out, y.Page)
		}
	}
	return out
}

// ocrPages are the pages this call sends to the engine.
func ocrPages(rep extract.Report, c ocrCall) []int {
	switch {
	case !c.active():
		return nil
	case rep.Format == "image":
		return []int{1}
	case rep.Format != "pdf":
		return nil
	case c.mode == transcribe.OCRAlways:
		var all []int
		for _, y := range rep.PageYields {
			all = append(all, y.Page)
		}
		return all
	default:
		return lowYieldPages(rep, c.minChars)
	}
}

// chunkPages cuts pages into runs of at most n, in order.
func chunkPages(pages []int, n int) [][]int {
	if n < 1 {
		n = 1
	}
	var out [][]int
	for len(pages) > 0 {
		k := min(n, len(pages))
		out = append(out, pages[:k:k])
		pages = pages[k:]
	}
	return out
}

// ---- the run tree's page budget ----

type ocrBudgetKey struct{}

// ocrBudgets is the run tree's page count: one budget at the operator's
// per-run cap for the whole tree, so fanning out to sub-agents cannot
// multiply it, and one per run at that run's agent's (possibly lower) cap.
type ocrBudgets struct {
	mu   sync.Mutex
	tree *transcribe.Budget
	runs map[string]*transcribe.Budget
}

// ocrBudgetsFor returns the run's and its tree's budgets. They live with the
// tree's inbox state, so a sub-agent and a resumed run count against the same
// tree.
func (f *File) ocrBudgetsFor(ctx context.Context, caps transcribe.Caps) (run, tree *transcribe.Budget, ok bool) {
	op := f.ocrConfig()
	v, ok := inbox.Shared(ctx, ocrBudgetKey{}, func() any {
		return &ocrBudgets{
			tree: transcribe.NewBudget(transcribe.Caps{MaxPagesPerRun: op.MaxPagesPerRun}),
			runs: map[string]*transcribe.Budget{},
		}
	})
	if !ok {
		return nil, nil, false
	}
	b := v.(*ocrBudgets)
	b.mu.Lock()
	defer b.mu.Unlock()
	id := tools.RunID(ctx)
	if b.runs[id] == nil {
		b.runs[id] = transcribe.NewBudget(caps)
	}
	return b.runs[id], b.tree, true
}

// reserveOCRPages reserves n pages on both budgets, or neither.
func reserveOCRPages(run, tree *transcribe.Budget, n int) error {
	if err := run.ReservePages(n); err != nil {
		return err
	}
	if err := tree.ReservePages(n); err != nil {
		run.ReleasePages(n)
		return err
	}
	return nil
}

// ---- one pass ----

// ocrPass is what one extract's OCR did.
type ocrPass struct {
	plan     transcribe.Plan
	language string
	text     map[int]string // page → transcript
	done     []int          // pages transcribed, in order
	units    int            // engine calls that answered
	// sent counts the pages of every call that reached the model, failed
	// ones included: what stays reserved, and what the event reports.
	sent     int
	in, out  int // tokens reported by the calls made
	cost     float64
	currency string
	priced   bool
	// unrecorded counts calls whose usage row could not be written: the
	// result reports it beside the provider, model and tokens, so the run's
	// transcript holds what an operator needs to reconcile the ledger.
	unrecorded int
	capErr     *transcribe.CapError
	callErr    error // an engine call failed
	err        error // cutting, rendering or a unit too large
}

func (p *ocrPass) status() string {
	switch {
	case p.capErr == nil && p.callErr == nil && p.err == nil:
		return "ok"
	case len(p.done) > 0:
		return "partial"
	}
	return "failed"
}

func (p *ocrPass) failure() error {
	switch {
	case p.capErr != nil:
		return p.capErr
	case p.callErr != nil:
		return p.callErr
	}
	return p.err
}

// charge records one call's usage and adds it to the pass's totals. A call
// that reported nothing is not a ledger row.
func (f *File) charge(ctx context.Context, p *ocrPass, res transcribe.Result) {
	p.in += res.InputTokens
	p.out += res.OutputTokens
	if res.InputTokens == 0 && res.OutputTokens == 0 && res.ReportedCost == 0 {
		return
	}
	provider, model := res.Provider, res.Model
	if provider == "" {
		provider = p.plan.Provider
	}
	if model == "" {
		model = p.plan.Model
	}
	price := transcribe.Price{}
	if f.OCR.Ledger != nil {
		var err error
		price, err = f.OCR.Ledger.Charge(ctx, transcribe.Charge{
			Purpose: transcribe.PurposeOCR, Provider: provider, Model: model,
			CredentialSource: res.CredentialSource, CredentialScopeID: res.CredentialScopeID,
			InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
			ReportedCost: res.ReportedCost, ReportedCurrency: res.ReportedCurrency,
		})
		if err != nil {
			// The call is made and paid for; failing the extract would not
			// unspend it. The row is lost: the log says so, and the result
			// (persisted in the run's transcript, beside its provider, model
			// and tokens) counts it for reconciling. The log line carries no
			// request-derived value: a store error can quote the request it
			// failed on, and run data reaching a log is what CodeQL flags.
			p.unrecorded++
			slog.Warn("transcription: an OCR call's usage could not be recorded; the extract result counts it under ocr.usage_unrecorded")
		}
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

// ocrUnit is one engine call's input: its bytes, their type, and the
// original pages they hold, in order.
type ocrUnit struct {
	data      []byte
	mediaType string
	pages     []int
}

// runOCR sends the pages to the engine, unit by unit, reserving each unit's
// pages before its call. It stops at the first cap, failure or refusal; what
// was transcribed before it stays in the pass.
func (f *File) runOCR(ctx context.Context, c ocrCall, plan transcribe.Plan, fh io.ReaderAt, size int64, mediaType string, pages []int) *ocrPass {
	p := &ocrPass{plan: plan, language: c.language, text: map[int]string{}}
	run, tree, ok := f.ocrBudgetsFor(ctx, c.caps)
	if !ok {
		p.err = errors.New("this run has no inbox to count its pages against")
		return p
	}
	op := f.ocrConfig()
	opts := transcribe.OCROptions{Language: c.language, Prompt: transcribe.OCRPrompt(op.PromptTemplate, c.language)}

	var queue [][]int
	switch {
	case mediaType != "application/pdf":
		queue = [][]int{pages}
	case plan.WholePDF:
		queue = chunkPages(pages, c.caps.MaxPagesPerCall)
	default:
		queue = chunkPages(pages, 1) // one rendered page per call
	}
	for len(queue) > 0 {
		chunk := queue[0]
		queue = queue[1:]
		u, split, err := f.ocrUnitFor(ctx, plan, fh, size, mediaType, chunk, c.language)
		if err != nil {
			p.err = err
			return p
		}
		if split != nil {
			queue = append(split, queue...)
			continue
		}
		if err := reserveOCRPages(run, tree, len(u.pages)); err != nil {
			var capErr *transcribe.CapError
			if errors.As(err, &capErr) {
				p.capErr = capErr
			} else {
				p.err = err
			}
			return p
		}
		unit, err := transcribe.NewUnit(bytes.NewReader(u.data), 0, int64(len(u.data)), u.mediaType)
		if err != nil {
			p.err = err
			return p
		}
		unit.Pages = transcribe.PageRange{First: 1, Last: len(u.pages)}
		opts.MaxTokens = op.MaxTokensPerPage * len(u.pages)
		res, err := f.OCR.Engine.OCR(ctx, plan, unit, opts)
		f.charge(ctx, p, res)
		if err != nil {
			// Pages go back only for a call that provably consumed nothing:
			// refused before any request, with no usage. A call that reached
			// the model may have been billed, and giving its pages back would
			// let a retry loop pay again and again under the cap.
			if !res.Reached && res.InputTokens == 0 && res.OutputTokens == 0 && res.ReportedCost == 0 {
				run.ReleasePages(len(u.pages))
				tree.ReleasePages(len(u.pages))
			} else {
				p.sent += len(u.pages)
			}
			p.callErr = err
			return p
		}
		p.sent += len(u.pages)
		p.units++
		for i, orig := range u.pages {
			t := noOCRText
			if i < len(res.Pages) {
				t = res.Pages[i].Text
			}
			p.text[orig] = t
			p.done = append(p.done, orig)
		}
	}
	return p
}

// noOCRText stands for a page the engine returned nothing for.
const noOCRText = "[no text]"

// ocrUnitFor builds the unit for a chunk of pages: the image itself, the
// pages cut out as one PDF, or one page rendered to an image. A PDF cut over
// the engine's request size is returned as two halves to send instead.
func (f *File) ocrUnitFor(ctx context.Context, plan transcribe.Plan, fh io.ReaderAt, size int64, mediaType string, chunk []int, language string) (ocrUnit, [][]int, error) {
	if mediaType != "application/pdf" {
		if plan.MaxUnitBytes > 0 && size > plan.MaxUnitBytes {
			return ocrUnit{}, nil, fmt.Errorf("the image is %d bytes; one OCR request takes at most %d", size, plan.MaxUnitBytes)
		}
		data, err := io.ReadAll(io.NewSectionReader(fh, 0, size))
		if err != nil {
			return ocrUnit{}, nil, fmt.Errorf("reading the image: %w", err)
		}
		return ocrUnit{data: data, mediaType: mediaType, pages: chunk}, nil, nil
	}
	cut, err := extract.SelectPages(ctx, fh, size, chunk)
	if err != nil {
		return ocrUnit{}, nil, fmt.Errorf("cutting page %s out of the PDF: %w", pageListText(chunk), err)
	}
	if plan.WholePDF {
		if plan.MaxUnitBytes > 0 && int64(len(cut)) > plan.MaxUnitBytes {
			if len(chunk) == 1 {
				return ocrUnit{}, nil, fmt.Errorf("page %d alone is %d bytes; one OCR request takes at most %d", chunk[0], len(cut), plan.MaxUnitBytes)
			}
			half := len(chunk) / 2
			return ocrUnit{}, [][]int{chunk[:half:half], chunk[half:]}, nil
		}
		return ocrUnit{data: cut, mediaType: "application/pdf", pages: chunk}, nil, nil
	}
	img, err := f.rasterize(ctx, cut, language)
	if err != nil {
		return ocrUnit{}, nil, fmt.Errorf("rendering page %d: %w", chunk[0], err)
	}
	return ocrUnit{data: img, mediaType: "image/png", pages: chunk}, nil, nil
}

// rasterize renders a one-page PDF to PNG with the operator's rasterize
// helper. The helper runs without a shell, in a scratch folder of the inbox
// that is removed afterwards; only the paths the runtime made reach its
// command line.
//
// Every file operation goes through an os.Root (the inbox's, then one held on
// the scratch folder from the moment it is made), never through a joined path
// string the kernel walks again: a folder swapped for a symlink while the
// helper runs cannot carry the page write, the PNG read or the removal out of
// the inbox. The absolute paths exist only for the helper's command line and
// working directory, which helper.Run fences itself.
func (f *File) rasterize(ctx context.Context, onePage []byte, language string) ([]byte, error) {
	op := f.ocrConfig()
	if op == nil || op.Rasterize == nil {
		return nil, errors.New("set transcription.ocr.rasterize.command to render PDF pages for this model")
	}
	root, _, err := effectiveRoot(ctx, inbox.Name, false)
	if err != nil {
		return nil, err
	}
	// Only the operator's root is resolved; everything below it is derived.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("inbox root: %w", err)
	}
	inboxFS, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("inbox root: %w", err)
	}
	defer inboxFS.Close()
	name, err := ocrScratchName()
	if err != nil {
		return nil, err
	}
	if err := inboxFS.Mkdir(name, 0o700); err != nil {
		return nil, err
	}
	defer inboxFS.RemoveAll(name)
	scratch, err := inboxFS.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer scratch.Close()
	if err := writeNew(scratch, "page.pdf", onePage); err != nil {
		return nil, err
	}
	tmp := filepath.Join(root, name)
	prefix := filepath.Join(tmp, "page")
	if _, err := helper.Run(ctx, helper.Spec{
		Key:      "transcription.ocr.rasterize",
		Template: op.Rasterize.Command,
		Vars:     map[string]string{"in": filepath.Join(tmp, "page.pdf"), "out_prefix": prefix, "out": prefix + ".png", "lang": language},
		Root:     root,
		Dir:      tmp,
		Timeout:  ocrRasterizeTimeout,
	}); err != nil {
		return nil, err
	}
	return readRenderedPage(scratch)
}

// ocrScratchName is a fresh scratch folder name; Mkdir refuses one that
// exists, so a planted entry is an error, never reused.
func ocrScratchName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".ocr-" + hex.EncodeToString(b[:]), nil
}

// readRenderedPage returns the first regular page*.png the helper wrote in
// the scratch folder ({out_prefix}*.png, or {out}), read through the folder's
// own handle and bounded by ocrMaxPageImage.
func readRenderedPage(scratch *os.Root) ([]byte, error) {
	dh, err := scratch.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := dh.Readdirnames(-1)
	dh.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	for _, n := range names {
		if !strings.HasPrefix(n, "page") || !strings.HasSuffix(n, ".png") {
			continue
		}
		fi, err := scratch.Lstat(n)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		fh, err := scratch.Open(n)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(fh, ocrMaxPageImage+1))
		fh.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > ocrMaxPageImage {
			return nil, fmt.Errorf("the rendered page is over %d bytes", ocrMaxPageImage)
		}
		return data, nil
	}
	return nil, errors.New("the rasterize helper wrote no PNG (expected {out_prefix}*.png or {out})")
}

// writeNew writes data to a file of dir that must not exist yet.
func writeNew(dir *os.Root, name string, data []byte) error {
	fh, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

func pageListText(pages []int) string {
	s := make([]string, len(pages))
	for i, p := range pages {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ", ")
}

// ---- the Markdown ----

// spliceOCR writes each transcribed page in place of the page's own Markdown
// (cut at the offsets the extractor reported), keeps every other page as it
// was, and puts a provenance line under the title.
func spliceOCR(md string, starts map[int]int, p *ocrPass) string {
	if len(p.done) == 0 || len(starts) == 0 {
		return md
	}
	pages := make([]int, 0, len(starts))
	for n := range starts {
		pages = append(pages, n)
	}
	sort.Ints(pages)
	at := func(n int) int { return min(starts[n], len(md)) }
	var b strings.Builder
	b.WriteString(md[:at(pages[0])])
	fmt.Fprintf(&b, "\n\n<!-- loom: ocr engine=%s provider=%s model=%s pages=%d language=%s -->",
		metaValue(p.plan.Engine), metaValue(p.plan.Provider), metaValue(p.plan.Model), len(p.done), metaValue(p.language))
	for i, n := range pages {
		end := len(md)
		if i+1 < len(pages) {
			end = at(pages[i+1])
		}
		if t, ok := p.text[n]; ok {
			fmt.Fprintf(&b, "\n\n## Page %d\n\n%s", n, demoteHeadings(t))
			continue
		}
		b.WriteString(md[at(n):end])
	}
	return b.String()
}

// metaValue keeps a provenance value to characters that can neither split
// the line into fields nor end the comment it sits in ("--" cannot occur).
func metaValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._:/@+-", r):
			if r == '-' && strings.HasSuffix(b.String(), "-") {
				continue
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}

// demoteHeadings puts a transcript's headings two levels down, under its
// "## Page N", so a page's "# Title" does not become a section of the
// document beside the pages. Fenced code is left as it is.
func demoteHeadings(md string) string {
	lines := strings.Split(md, "\n")
	var fenceChar byte
	fenceRun := 0
	for i, l := range lines {
		if c, run := mdFenceDelim(l); run > 0 {
			switch {
			case fenceRun == 0:
				fenceChar, fenceRun = c, run
			case c == fenceChar && run >= fenceRun:
				fenceRun = 0
			}
			continue
		}
		if fenceRun > 0 {
			continue
		}
		if m := mdHeadingRe.FindStringSubmatch(l); m != nil {
			lines[i] = strings.Repeat("#", min(len(m[1])+2, 6)) + " " + m[2]
		}
	}
	return strings.Join(lines, "\n")
}

// ---- results and events ----

// maxEventErrorLen bounds the reason a transcription event carries: enough for
// an engine's error line, never a dump of a provider's response body.
const maxEventErrorLen = 300

// eventError folds a reason to one line and cuts it to maxEventErrorLen.
func eventError(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxEventErrorLen {
		cut := maxEventErrorLen - len("…")
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// ocrSummary is the "ocr" object of an extract result.
func (p *ocrPass) summary(mode transcribe.OCRMode) map[string]any {
	out := map[string]any{
		"mode": string(mode), "engine": p.plan.Engine, "provider": p.plan.Provider, "model": p.plan.Model,
		"pages": append([]int{}, p.done...), "pages_sent": p.sent, "units": p.units, "status": p.status(),
		"input_tokens": p.in, "output_tokens": p.out,
	}
	if p.unrecorded > 0 {
		out["usage_unrecorded"] = p.unrecorded
	}
	if p.priced {
		out["cost"], out["currency"] = p.cost, p.currency
	}
	return out
}

// event reports the pass, counting what ran: the pages of every call that
// reached the model (those the budget keeps) and the tokens the calls
// reported, as the ledger rows do. A pass that failed or stopped early also
// says why (the cap, the engine's error, or a cutting failure), on one line.
func (p *ocrPass) event(ctx context.Context, src, out string) providers.Event {
	info := &providers.TranscriptionInfo{
		Ref: src, Out: out, Purpose: transcribe.PurposeOCR, Engine: p.plan.Engine,
		Provider: p.plan.Provider, Model: p.plan.Model, Pages: p.sent,
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

// ocrFailure is the structured error of a pass that stopped: why, and where
// the Markdown written so far is.
func ocrFailure(p *ocrPass, ref string) tools.Result {
	body := map[string]any{"error": "extract: OCR stopped: " + p.failure().Error(), "ref": ref, "ocr_pages": append([]int{}, p.done...), "status": p.status()}
	hint := "The Markdown written so far is at ref."
	if p.capErr != nil {
		body["cap"], body["unit"], body["limit"], body["used"], body["requested"] = p.capErr.Cap, p.capErr.Unit, p.capErr.Limit, p.capErr.Used, p.capErr.Requested
		hint += " The run's page cap is reached; an operator sets it."
	}
	b, _ := json.Marshal(body)
	switch {
	case p.capErr != nil:
		return errBusiness(string(b), hint)
	case p.callErr != nil:
		return errTransient(string(b), hint+" Try again for the pages left.")
	}
	return errBusiness(string(b), hint)
}

// ocrDryRun answers what OCR would read and cost without calling the engine,
// counting units by the same rule runOCR cuts by (a cut that turns out over
// the request size would be split further).
func (f *File) ocrDryRun(ctx context.Context, c ocrCall, plan transcribe.Plan, src, mediaType string, pages []int, lowYield []int) (tools.Result, error) {
	units := 0
	switch {
	case len(pages) == 0:
	case mediaType != "application/pdf":
		units = 1
	case plan.WholePDF:
		units = len(chunkPages(pages, c.caps.MaxPagesPerCall))
	default:
		units = len(pages)
	}
	op := f.ocrConfig()
	outPerPage := ocrEstOutputPerPage
	if op != nil {
		outPerPage = min(outPerPage, op.MaxTokensPerPage)
	}
	in := len(pages)*ocrEstInputPerPage + units*ocrEstPromptPerCall
	out := len(pages) * outPerPage
	res := map[string]any{
		"dry_run": true, "ocr": string(c.mode), "source": src, "pages": len(pages), "page_numbers": append([]int{}, pages...),
		"units": units, "engine": plan.Engine, "provider": plan.Provider, "model": plan.Model,
		"est_tokens": in + out, "est_input_tokens": in, "est_output_tokens": out,
	}
	if lowYield != nil {
		res["low_yield_pages"] = lowYield
	}
	info := &providers.TranscriptionInfo{
		Ref: src, Purpose: transcribe.PurposeOCR, Engine: plan.Engine, Provider: plan.Provider, Model: plan.Model,
		Pages: len(pages), Status: "ok", DryRun: true, RunID: tools.RunID(ctx),
	}
	if len(pages) > 0 && f.OCR.Ledger != nil {
		if pr := f.OCR.Ledger.Estimate(plan.Provider, plan.Model, in, out); pr.Priced {
			res["est_cost"], res["currency"] = pr.Cost, pr.Currency
			cost := pr.Cost
			info.Cost, info.Currency = &cost, pr.Currency
		}
	}
	tools.EventEmitter(ctx)(providers.Event{Type: providers.EventTranscription, Transcription: info})
	return okJSON(res)
}
