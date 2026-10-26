package transcribe

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// OCRMode is when OCR runs for an agent.
type OCRMode string

const (
	// OCRNever is the default: OCR is billed per page, so it is opt-in.
	OCRNever OCRMode = config.AgentOCRNever
	// OCRAuto runs OCR on pages whose text layer yields too little.
	OCRAuto OCRMode = config.AgentOCRAuto
	// OCRAlways runs OCR on every page.
	OCRAlways OCRMode = config.AgentOCRAlways
)

// Caps are what one agent's runs may do: whether OCR and STT are on, and the
// page and minute caps. A disabled engine has zero caps, so a reservation
// against it fails before any engine is reached.
type Caps struct {
	OCR               OCRMode
	STT               bool
	MaxPagesPerCall   int
	MaxPagesPerRun    int
	MaxMinutesPerCall int
	MaxMinutesPerRun  int
}

// Effective is an agent's caps under the operator's configuration: an agent
// can only LOWER a cap (min semantics) and cannot turn on an engine the
// operator has not configured. Config load and AgentDef create/fork already
// refuse an agent cap above the operator's; the minimum here also covers a
// stored definition the operator undercut later. The operator block is
// resolved here, so a block built in code gets the same defaults as a loaded
// one.
func Effective(operator config.TranscriptionConfig, agent config.AgentTranscription) Caps {
	op := operator.Resolved()
	var c Caps
	if op.OCREnabled() {
		c.OCR = OCRMode(agent.OCR)
		if c.OCR == "" {
			c.OCR = OCRNever
		}
		c.MaxPagesPerRun = lower(op.OCR.MaxPagesPerRun, agent.MaxPagesPerRun)
		c.MaxPagesPerCall = min(op.OCR.MaxPagesPerCall, c.MaxPagesPerRun)
	} else {
		c.OCR = OCRNever
	}
	if op.STTEnabled() && agent.STT != config.AgentSTTOff {
		c.STT = true
		c.MaxMinutesPerRun = lower(op.STT.MaxMinutesPerRun, agent.MaxMinutesPerRun)
		c.MaxMinutesPerCall = min(op.STT.MaxMinutesPerCall, c.MaxMinutesPerRun)
	}
	return c
}

// lower is the operator's cap, lowered by the agent's when the agent set one.
func lower(operator, agent int) int {
	if agent > 0 && agent < operator {
		return agent
	}
	return operator
}

// ErrCapExceeded matches every *CapError (errors.Is).
var ErrCapExceeded = errors.New("transcription cap exceeded")

// CapError is a refused reservation: it names the cap, its limit, what the
// run had already used and what was asked for. Pages are counted in pages;
// minutes caps are counted in seconds.
type CapError struct {
	Cap       string // max_pages_per_run | max_minutes_per_run
	Unit      string // pages | seconds
	Limit     float64
	Used      float64
	Requested float64
}

func (e *CapError) Error() string {
	return fmt.Sprintf("transcription: %s reached: %g %s used of %g, %g more requested",
		e.Cap, e.Used, e.Unit, e.Limit, e.Requested)
}

func (e *CapError) Unwrap() error { return ErrCapExceeded }

// Budget counts one run's transcription against its per-run caps. Reservations
// are all-or-nothing: a refused one reserves nothing. Safe for concurrent use.
//
// Per-call caps are not checked here: they decide how the caller cuts a file
// into units, and a reservation is for the whole of what a call will spend,
// made before its first engine request.
type Budget struct {
	mu        sync.Mutex
	pageLimit int
	secLimit  float64
	pages     int
	seconds   float64
}

// NewBudget starts an empty budget under caps.
func NewBudget(caps Caps) *Budget {
	return &Budget{pageLimit: caps.MaxPagesPerRun, secLimit: float64(caps.MaxMinutesPerRun) * 60}
}

// ReservePages reserves n pages, or returns a *CapError naming
// max_pages_per_run when they do not fit.
func (b *Budget) ReservePages(n int) error {
	if n <= 0 {
		return fmt.Errorf("transcription: cannot reserve %d pages", n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pages+n > b.pageLimit {
		return &CapError{Cap: "max_pages_per_run", Unit: "pages", Limit: float64(b.pageLimit), Used: float64(b.pages), Requested: float64(n)}
	}
	b.pages += n
	return nil
}

// ReleasePages gives back n pages a reservation took: one that a second
// budget then refused, so a refused call holds nothing in either. More than
// is held releases everything.
func (b *Budget) ReleasePages(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pages = max(b.pages-n, 0)
}

// ReserveSeconds reserves s seconds of audio, or returns a *CapError naming
// max_minutes_per_run when they do not fit.
func (b *Budget) ReserveSeconds(s float64) error {
	if !(s > 0) || math.IsInf(s, 0) {
		return fmt.Errorf("transcription: cannot reserve %v seconds", s)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seconds+s > b.secLimit {
		return &CapError{Cap: "max_minutes_per_run", Unit: "seconds", Limit: b.secLimit, Used: b.seconds, Requested: s}
	}
	b.seconds += s
	return nil
}

// ReleaseSeconds gives back s seconds a reservation took and no request
// spent: a later unit that was never sent, or a reservation a second budget
// refused. More than is held releases everything.
func (b *Budget) ReleaseSeconds(s float64) {
	if !(s > 0) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seconds = max(b.seconds-s, 0)
}

// AddSeconds counts s seconds a request spent beyond what was reserved for
// it, without the cap check: the engine reported more audio than the header
// claimed, and the audio is already transcribed. The count may pass the cap;
// the run's next reservation is then refused.
func (b *Budget) AddSeconds(s float64) {
	if !(s > 0) || math.IsInf(s, 0) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seconds += s
}

// Pages is how many pages the run has reserved.
func (b *Budget) Pages() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pages
}

// Seconds is how many seconds of audio the run has reserved.
func (b *Budget) Seconds() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seconds
}
