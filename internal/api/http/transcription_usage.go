package http

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TranscriptionUsage is one OCR, speech-to-text or text-to-speech engine call
// to charge: what served it, who paid, how much it read or produced, and what
// it cost. Text-to-speech shares the type rather than having its own so the
// three speech-and-vision units go through one validation and one charge
// path, and cannot drift on who is charged or what a priced unit needs.
//
// The caller prices the call — an STT model by the minute (pricing
// CostSeconds), a TTS model by the minute or by the million characters, a
// vision OCR call by its tokens like any chat call — and passes the result.
// Priced false records no cost (stored NULL, "unpriced"), distinct from a
// priced zero.
type TranscriptionUsage struct {
	// Purpose is store.UsagePurposeOCR, store.UsagePurposeSTT or
	// store.UsagePurposeTTS.
	Purpose  string
	Provider string
	Model    string
	// CredentialSource is whose key paid: "operator" (the default when empty),
	// "tenant" or "user"; CredentialScopeID names the override's owner.
	CredentialSource  string
	CredentialScopeID string

	// Tokens, for an engine that reports them (a vision OCR call; an audio
	// model that counts audio in tokens). They count against the token
	// budgets. A TTS unit carries none: the speech endpoint reports no
	// tokens, and its characters and seconds are not tokens, so it adds
	// nothing to a token budget.
	InputTokens  int
	OutputTokens int
	// AudioSeconds is the audio an STT call transcribed or a TTS call
	// produced. Must be 0 for OCR.
	AudioSeconds float64
	// InputChars is the text a TTS call sent to the engine, in characters.
	// Must be 0 for OCR and STT.
	InputChars int64

	Cost     float64
	Currency string
	Priced   bool
}

// validate refuses a unit the ledger could not mean: chat is the loop's
// purpose, seconds only describe audio, characters only describe text sent to
// a speech engine, a TTS unit has no tokens, and a priced cost needs its
// currency.
func (u TranscriptionUsage) validate() error {
	switch u.Purpose {
	case store.UsagePurposeOCR:
		if u.AudioSeconds != 0 {
			return errors.New("transcription usage: an OCR unit carries no audio seconds")
		}
	case store.UsagePurposeSTT:
	case store.UsagePurposeTTS:
		if u.InputTokens != 0 || u.OutputTokens != 0 {
			return errors.New("transcription usage: a TTS unit carries no tokens")
		}
	default:
		return fmt.Errorf("transcription usage: purpose %q is not ocr, stt or tts", u.Purpose)
	}
	if u.InputChars != 0 && u.Purpose != store.UsagePurposeTTS {
		return errors.New("transcription usage: only a TTS unit carries input characters")
	}
	if u.InputChars < 0 {
		return errors.New("transcription usage: input characters cannot be negative")
	}
	if u.Provider == "" || u.Model == "" {
		return errors.New("transcription usage: provider and model are required")
	}
	switch u.CredentialSource {
	case "", "operator", "tenant", "user":
	default:
		return fmt.Errorf("transcription usage: credential source %q is not operator, tenant or user", u.CredentialSource)
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return errors.New("transcription usage: token counts cannot be negative")
	}
	if u.Priced && (u.Currency == "" || u.Cost < 0 || math.IsNaN(u.Cost) || math.IsInf(u.Cost, 0)) {
		return errors.New("transcription usage: a priced unit needs a currency and a finite, non-negative cost")
	}
	return nil
}

// RecordTranscriptionUsage writes one OCR, STT or TTS engine call to the usage
// ledger, charged the way RecordRunSideCallUsage charges a tool's model call:
// to the run on ctx, or to the caller of an admitted off-run call; with
// neither, nothing is recorded and nil is returned. Its tokens count against
// the token budgets like any call; an STT or TTS unit's seconds and
// characters do not (budgets count tokens), and its cost reaches the run's
// cost summary through the ledger.
//
// A crossed budget tier is not announced here — there is no event stream at a
// tool call's depth — and the run's next loop call that crosses reports it.
//
// The unit is not a throughput sample: the estimate the loop schedules by is
// per chat model, and a transcription call's duration is not comparable.
func (s *Server) RecordTranscriptionUsage(ctx context.Context, u TranscriptionUsage) error {
	if err := u.validate(); err != nil {
		return err
	}
	runID, rid, sessionID, iteration, ok := sideCallCharge(ctx)
	if !ok || s.store == nil {
		return nil
	}
	source := u.CredentialSource
	if source == "" {
		source = "operator"
	}
	parentRunID := ""
	if rid.RootRunID != "" && rid.RootRunID != runID {
		parentRunID = rid.RootRunID
	}
	row := store.TokenUsageRow{
		RunID:             runID,
		SessionID:         sessionID,
		TenantID:          rid.TenantID,
		UserID:            rid.UserID,
		AgentID:           rid.AgentID,
		ParentRunID:       parentRunID,
		Iteration:         iteration,
		Provider:          u.Provider,
		Model:             u.Model,
		CredentialSource:  source,
		CredentialScopeID: u.CredentialScopeID,
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		Purpose:           u.Purpose,
		AudioSeconds:      u.AudioSeconds,
		InputChars:        u.InputChars,
		TS:                time.Now(),
	}
	if u.Priced {
		row.Cost, row.CostCurrency = u.Cost, u.Currency
	}
	_, err := s.appendUsageRow(ctx, row)
	return err
}

// sideCallCharge is who a call made outside the loop is charged to, by the
// rules RecordRunSideCallUsage documents: an admitted off-run call's caller,
// else the run on ctx. ok is false when there is nobody to charge.
func sideCallCharge(ctx context.Context) (runID string, rid tools.RunIdentityValue, sessionID string, iteration int, ok bool) {
	if call, metered := tools.MeteredOffRunCall(ctx); metered {
		return "", tools.RunIdentityValue{TenantID: call.TenantID, UserID: call.UserID}, "", 0, true
	}
	runID = tools.RunID(ctx)
	if runID == "" {
		return "", tools.RunIdentityValue{}, "", 0, false
	}
	rid = tools.RunIdentity(ctx)
	return runID, rid, rid.SessionID, loop.IterationOf(ctx), true
}
