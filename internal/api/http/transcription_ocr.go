package http

import (
	"context"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
	"github.com/denn-gubsky/loomcycle/internal/transcribe/vision"
)

// FileOCR builds what the File tool needs to OCR (RFC FG) from the operator's
// transcription block: the vision engine on this server's model resolution
// and provider registry, and the usage ledger. nil when OCR is off. Built
// once at boot: a reload of the transcription block takes a restart, while
// the tier, the providers and the pricing table it reads stay live.
func (s *Server) FileOCR() (*builtin.FileOCR, error) {
	t := s.cfg().Transcription.Resolved()
	if !t.OCREnabled() {
		return nil, nil
	}
	if t.OCR.Engine != config.OCREngineVision {
		return nil, errors.New("transcription.ocr: only the vision engine is built")
	}
	engine, err := vision.New(vision.Options{
		Config:   *t.OCR,
		Resolve:  s.ResolveOCRModel,
		Provider: func(id string) (providers.Provider, error) { return s.providers.Get(id) },
	})
	if err != nil {
		return nil, err
	}
	return &builtin.FileOCR{Config: t, Engine: engine, Ledger: transcriptionLedger{s: s}}, nil
}

// ocrResolveName is the name OCR resolution goes by in errors and logs.
const ocrResolveName = "transcription_ocr"

// ResolveOCRModel resolves the OCR model for the run on ctx the way the run's
// own model is resolved: the pinned models: alias when the operator set one,
// else the tier's cascade (default middle, as the describe-images pass). It
// goes through resolveAgentDef, so the run's user tier shapes the cascade and
// a run restricted from the operator's key (RFC AX) is routed to a provider
// its tenant can key; the engine's call then carries the run's credential
// resolver on ctx, as any of the run's calls does.
func (s *Server) ResolveOCRModel(ctx context.Context, tier, alias string) (string, string, error) {
	def := config.AgentDef{Tier: tier}
	if alias != "" {
		def = config.AgentDef{Model: alias}
	}
	rid := tools.RunIdentity(ctx)
	providerID, model, _, err := s.resolveAgentDef(ctx, def, rid.TenantID, rid.UserID, ocrResolveName, rid.UserTier, rid.OperatorKeyRestricted)
	return providerID, model, err
}

// transcriptionLedger prices a transcription call with the live pricing table
// and records it through RecordTranscriptionUsage.
type transcriptionLedger struct{ s *Server }

func (l transcriptionLedger) price(c transcribe.Charge) transcribe.Price {
	if c.ReportedCost > 0 {
		cur := c.ReportedCurrency
		if cur == "" {
			cur = "USD"
		}
		return transcribe.Price{Cost: c.ReportedCost, Currency: cur, Priced: true}
	}
	pricing := l.s.cfg().Pricing
	var cost float64
	var cur string
	var ok bool
	if c.AudioSeconds > 0 {
		cost, cur, ok = pricing.CostSeconds(c.Provider, c.Model, c.AudioSeconds)
	} else {
		cost, cur, ok = pricing.Cost(c.Provider, c.Model, c.InputTokens, c.OutputTokens, 0, 0)
	}
	return transcribe.Price{Cost: cost, Currency: cur, Priced: ok}
}

func (l transcriptionLedger) Charge(ctx context.Context, c transcribe.Charge) (transcribe.Price, error) {
	p := l.price(c)
	err := l.s.RecordTranscriptionUsage(ctx, TranscriptionUsage{
		Purpose: c.Purpose, Provider: c.Provider, Model: c.Model,
		CredentialSource: c.CredentialSource, CredentialScopeID: c.CredentialScopeID,
		InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, AudioSeconds: c.AudioSeconds,
		Cost: p.Cost, Currency: p.Currency, Priced: p.Priced,
	})
	return p, err
}

func (l transcriptionLedger) Estimate(provider, model string, in, out int) transcribe.Price {
	cost, cur, ok := l.s.cfg().Pricing.Cost(provider, model, in, out, 0, 0)
	return transcribe.Price{Cost: cost, Currency: cur, Priced: ok}
}
