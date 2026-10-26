// Package vision is the OCR engine that reads pages through a vision-capable
// chat model: a Claude model takes a PDF page range whole as a document, any
// other vision model (a local Ollama model) one page image at a time.
//
// The engine makes one call per unit and nothing else: the File tool cuts the
// file, reserves the pages, records the ledger row and writes the transcript.
// The call is made on the tool call's own context, so the run's credential
// resolver (a tenant's own provider key) and its operator-key restriction
// apply to it exactly as to the run's own turns, as does the run's slot in the
// provider's call lane.
package vision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// MaxUnitBytes caps what one call sends. The Messages API takes a request of
// up to 32 MB, base64 makes bytes a third larger, and the prompt rides along;
// the File tool cuts a page range smaller when it would not fit.
const MaxUnitBytes = 16 << 20

// callTimeout bounds one call. A cold local vision model can take minutes on
// its first page; a hundred-page document on a hosted model is long too.
const callTimeout = 10 * time.Minute

// Resolver names the provider and model OCR calls for the run on ctx: the
// tier's cascade, or the pinned models: alias when there is one. It is the
// server's resolution, so a restricted run is routed to a provider its tenant
// can key.
type Resolver func(ctx context.Context, tier, alias string) (providerID, model string, err error)

// Options build an Engine.
type Options struct {
	// Config is the operator's resolved transcription.ocr block.
	Config config.OCRConfig
	// Resolve and Provider are the server's model resolution and its
	// provider registry.
	Resolve  Resolver
	Provider func(id string) (providers.Provider, error)
}

// Engine is the vision OCR engine.
type Engine struct {
	cfg      config.OCRConfig
	resolve  Resolver
	provider func(id string) (providers.Provider, error)
	// decode is image.Decode; a test counts its calls.
	decode func(io.Reader) (image.Image, string, error)
	// decodeSlot admits one image decode at a time. It hangs off the engine,
	// not a package variable: the server builds one engine, so this bounds
	// the process, and a test's engine has its own.
	decodeSlot chan struct{}
}

// New builds the engine.
func New(o Options) (*Engine, error) {
	if o.Resolve == nil || o.Provider == nil {
		return nil, errors.New("vision OCR: a resolver and a provider registry are required")
	}
	if o.Config.Engine != config.OCREngineVision {
		return nil, fmt.Errorf("vision OCR: transcription.ocr.engine is %q, not %s", o.Config.Engine, config.OCREngineVision)
	}
	return &Engine{cfg: o.Config, resolve: o.Resolve, provider: o.Provider,
		decode: image.Decode, decodeSlot: make(chan struct{}, 1)}, nil
}

func (e *Engine) Name() string { return config.OCREngineVision }

// Accepts is every type the engine can be handed: a PDF (whole or
// rasterised, decided by the plan) and the image types the extractor reads.
func (e *Engine) Accepts(mediaType string) bool {
	switch mediaType {
	case "application/pdf", "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return true
	}
	return false
}

// Plan resolves the model and how it reads a file of mediaType. The model
// must see; a PDF needs either a model that reads documents or the rasterize
// helper, and the refusal names that setting.
func (e *Engine) Plan(ctx context.Context, mediaType string) (transcribe.Plan, error) {
	if !e.Accepts(mediaType) {
		return transcribe.Plan{}, fmt.Errorf("the OCR engine does not read %s", mediaType)
	}
	providerID, model, err := e.resolve(ctx, e.cfg.Tier, e.cfg.Model)
	if err != nil {
		return transcribe.Plan{}, fmt.Errorf("the OCR model could not be resolved: %w", err)
	}
	prov, err := e.provider(providerID)
	if err != nil {
		return transcribe.Plan{}, fmt.Errorf("the OCR model's provider %q is not available: %w", providerID, err)
	}
	caps := prov.Capabilities()
	if !caps.SupportsVision {
		return transcribe.Plan{}, fmt.Errorf("the OCR model %s/%s cannot accept images; point transcription.ocr.tier (or .model) at a vision-capable model", providerID, model)
	}
	plan := transcribe.Plan{Engine: e.Name(), Provider: providerID, Model: model, WholePDF: caps.SupportsDocumentInput}
	if mediaType == "application/pdf" && !plan.WholePDF && e.cfg.Rasterize == nil {
		return transcribe.Plan{}, fmt.Errorf("the OCR model %s/%s reads page images, not PDFs; set transcription.ocr.rasterize.command (e.g. pdftoppm -r 150 -png {in} {out_prefix}) so its pages can be rendered", providerID, model)
	}
	return plan, nil
}

// OCR makes one call for unit: a PDF page range as a document, or one image.
func (e *Engine) OCR(ctx context.Context, plan transcribe.Plan, unit transcribe.Unit, opts transcribe.OCROptions) (transcribe.Result, error) {
	res := transcribe.Result{Provider: plan.Provider, Model: plan.Model}
	prov, err := e.provider(plan.Provider)
	if err != nil {
		return res, fmt.Errorf("the OCR model's provider %q is not available: %w", plan.Provider, err)
	}
	caps := prov.Capabilities()
	if !caps.SupportsVision {
		return res, fmt.Errorf("the OCR model %s/%s cannot accept images", plan.Provider, plan.Model)
	}
	if unit.Size() > MaxUnitBytes {
		return res, fmt.Errorf("one OCR request may carry at most %d bytes; this one is %d", MaxUnitBytes, unit.Size())
	}
	data, err := io.ReadAll(io.NewSectionReader(unit, 0, unit.Size()))
	if err != nil {
		return res, fmt.Errorf("reading the OCR input: %w", err)
	}
	pages := unit.Pages
	if pages.Count() == 0 {
		pages = transcribe.PageRange{First: 1, Last: 1}
	}
	n := pages.Count()

	prompt := opts.Prompt
	marker := ""
	if n > 1 {
		// The reply is split back into pages on a line only this call knows:
		// a page of the document cannot forge a boundary it never saw.
		marker = "<<<PAGE-" + nonce() + ">>>"
		prompt += fmt.Sprintf("\n\nThe document has %d pages. Begin the transcript of every page, the first one included, "+
			"with a line holding exactly %s and nothing else.", n, marker)
	}

	req := providers.Request{Model: plan.Model, MaxTokens: opts.MaxTokens}
	var content []providers.ContentBlock
	switch {
	case unit.MediaType == "application/pdf":
		if !caps.SupportsDocumentInput || !plan.WholePDF {
			return res, fmt.Errorf("the OCR model %s/%s reads page images; a PDF must be rasterised first", plan.Provider, plan.Model)
		}
		req.Documents = []providers.DocumentInput{{MediaType: "application/pdf", Data: base64.StdEncoding.EncodeToString(data)}}
	case strings.HasPrefix(unit.MediaType, "image/"):
		img, mt, err := e.prepareImage(ctx, data, unit.MediaType)
		if err != nil {
			return res, err
		}
		content = append(content, providers.ContentBlock{Type: "image", MediaType: mt, Data: base64.StdEncoding.EncodeToString(img)})
	default:
		return res, fmt.Errorf("the OCR engine does not read %s", unit.MediaType)
	}
	content = append(content, providers.ContentBlock{Type: "text", Text: prompt})
	req.Messages = []providers.Message{{Role: "user", Content: content}}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	ch, err := prov.Call(callCtx, req)
	if err != nil {
		return res, err
	}
	var b strings.Builder
	var callErr error
	stop := ""
	// Drain to the end even after an error: an abandoned channel leaves the
	// driver's goroutine blocked on its send.
	for ev := range ch {
		switch ev.Type {
		case providers.EventText:
			b.WriteString(ev.Text)
		case providers.EventDone:
			if ev.StopReason != "" {
				stop = ev.StopReason
			}
			if u := ev.Usage; u != nil {
				res.InputTokens, res.OutputTokens = u.InputTokens+u.CacheCreationTokens+u.CacheReadTokens, u.OutputTokens
				res.CredentialSource, res.CredentialScopeID = u.CredentialSource, u.CredentialScopeID
				res.ReportedCost, res.ReportedCurrency = u.ProviderReportedCost, u.ProviderCostCurrency
				if u.Model != "" {
					res.Model = u.Model
				}
			}
		case providers.EventError:
			if callErr == nil {
				callErr = errors.New(ev.Error)
			}
		}
	}
	if callErr != nil {
		return res, callErr
	}
	text := strings.TrimSpace(b.String())
	cutAtCeiling := stop == "max_tokens" || stop == "length"
	if text == "" && cutAtCeiling {
		// Not a blank page: the model ran out of room before writing (a
		// thinking model spends the budget on its trace first). An error, so
		// the page is not recorded as having no text.
		return res, fmt.Errorf("the OCR model reached the %d-token ceiling before writing any text; raise transcription.ocr.max_tokens_per_page", opts.MaxTokens)
	}
	res.Pages = splitPages(text, marker, pages)
	if cutAtCeiling {
		last := &res.Pages[len(res.Pages)-1]
		last.Text += "\n\n[the transcript was cut here: the model reached its token ceiling]"
	}
	return res, nil
}

// noText stands for a page the model found nothing on.
const noText = "[no text]"

// splitPages files the reply under the unit's pages. With one page the whole
// reply is that page. With several, the reply is cut at the marker lines;
// when their count is not the page count, the reply stays whole on the
// first page and the others say where their text went.
func splitPages(text, marker string, r transcribe.PageRange) []transcribe.Page {
	n := r.Count()
	out := make([]transcribe.Page, n)
	for i := range out {
		out[i].Number = r.First + i
	}
	orNoText := func(s string) string {
		if s = strings.TrimSpace(s); s == "" {
			return noText
		}
		return s
	}
	if n == 1 {
		out[0].Text = orNoText(text)
		return out
	}
	var parts []*strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == marker {
			parts = append(parts, &strings.Builder{})
			continue
		}
		if len(parts) == 0 {
			continue // before the first marker: preamble, not a page
		}
		parts[len(parts)-1].WriteString(line + "\n")
	}
	if len(parts) == n {
		for i := range out {
			out[i].Text = orNoText(parts[i].String())
		}
		return out
	}
	whole := strings.ReplaceAll(text, marker, "")
	out[0].Text = orNoText(whole)
	for i := 1; i < n; i++ {
		out[i].Text = fmt.Sprintf("[transcribed together with page %d]", r.First)
	}
	return out
}

func nonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
