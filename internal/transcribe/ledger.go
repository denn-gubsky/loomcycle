package transcribe

import "context"

// The ledger purposes of a transcription engine call (token_usage.purpose).
const (
	PurposeOCR = "ocr"
	PurposeSTT = "stt"
)

// Charge is one engine call for the usage ledger: what served it, whose key
// paid, and what it read.
type Charge struct {
	Purpose  string
	Provider string
	Model    string
	// CredentialSource is "operator" (or empty), "tenant" or "user";
	// CredentialScopeID names the override's owner.
	CredentialSource  string
	CredentialScopeID string
	InputTokens       int
	OutputTokens      int
	// AudioSeconds is what a speech-to-text call transcribed; 0 for OCR.
	AudioSeconds float64
	// ReportedCost is the provider's own price for the call, which wins over
	// the pricing table when set.
	ReportedCost     float64
	ReportedCurrency string
}

// Price is a cost under the operator's pricing table. Priced false is an
// unpriced pair (no cost known), distinct from a priced zero.
type Price struct {
	Cost     float64
	Currency string
	Priced   bool
}

// Ledger prices transcription calls and records them. The server implements
// it over its usage ledger and its live pricing table; the File tool holds it
// so it never imports the server.
type Ledger interface {
	// Charge prices c and writes it to the ledger, charged to the run on ctx.
	// The returned price is what was recorded, also when writing failed.
	Charge(ctx context.Context, c Charge) (Price, error)
	// Estimate prices tokens a call has not spent yet (a dry run).
	Estimate(provider, model string, inputTokens, outputTokens int) Price
}
