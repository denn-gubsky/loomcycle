package speech

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// DefaultCharsPerSecond is the spoken rate EstimateSeconds assumes when the
// operator sets none: about 17.5 characters per second was measured for
// Ukrainian (Piper) and English (Kokoro, Phonon) alike, rounded down so an
// estimate errs long and a seconds cap refuses early rather than late.
const DefaultCharsPerSecond = 17.0

// Caps are what one agent's runs may synthesise. MaxCharsPerCall is the
// largest piece one engine request carries (the caller passes it to Split);
// the per-run caps are what Budget enforces. A zero cap allows nothing, so an
// engine the operator has not configured has zero caps and every reservation
// against it fails before an engine is reached.
type Caps struct {
	MaxCharsPerCall  int
	MaxCharsPerRun   int
	MaxSecondsPerRun float64
}

// Effective is an agent's caps under the operator's: an agent can only LOWER
// a cap (min semantics). An agent field that is zero (or negative, or not a
// number) takes the operator's value, and one above the operator's does not
// raise it — that covers a stored definition the operator undercut later. The
// per-call cap never exceeds the per-run one, so one piece cannot be larger
// than the run may speak.
func Effective(operator, agent Caps) Caps {
	c := Caps{
		MaxCharsPerRun:   lowerInt(operator.MaxCharsPerRun, agent.MaxCharsPerRun),
		MaxSecondsPerRun: lowerFloat(operator.MaxSecondsPerRun, agent.MaxSecondsPerRun),
	}
	c.MaxCharsPerCall = min(lowerInt(operator.MaxCharsPerCall, agent.MaxCharsPerCall), c.MaxCharsPerRun)
	return c
}

// lowerInt is the operator's cap, lowered by the agent's when the agent set
// one. A negative operator cap is treated as zero (nothing allowed).
func lowerInt(operator, agent int) int {
	operator = max(operator, 0)
	if agent > 0 && agent < operator {
		return agent
	}
	return operator
}

// lowerFloat is lowerInt for a seconds cap. A NaN or negative operator cap is
// zero; a NaN agent cap is unset (NaN > 0 is false).
func lowerFloat(operator, agent float64) float64 {
	if !(operator > 0) {
		operator = 0
	}
	if agent > 0 && agent < operator {
		return agent
	}
	return operator
}

// EstimateSeconds is how long chars characters take to speak at
// charsPerSecond; a rate that is not a positive finite number means
// DefaultCharsPerSecond. It sizes a seconds reservation before any audio
// exists and is what a dry run reports.
func EstimateSeconds(chars int, charsPerSecond float64) float64 {
	if chars <= 0 {
		return 0
	}
	if !(charsPerSecond > 0) || math.IsInf(charsPerSecond, 0) {
		charsPerSecond = DefaultCharsPerSecond
	}
	return float64(chars) / charsPerSecond
}

// ErrCapExceeded matches every *CapError (errors.Is).
var ErrCapExceeded = errors.New("speech cap exceeded")

// CapError is a refused reservation: it names the cap, its limit, what the
// run had already used and what was asked for.
type CapError struct {
	Cap       string // max_chars_per_run | max_seconds_per_run
	Unit      string // chars | seconds
	Limit     float64
	Used      float64
	Requested float64
}

func (e *CapError) Error() string {
	return fmt.Sprintf("speech: %s reached: %g %s used of %g, %g more requested",
		e.Cap, e.Used, e.Unit, e.Limit, e.Requested)
}

func (e *CapError) Unwrap() error { return ErrCapExceeded }

// Budget counts one run's speech against its per-run caps. Every reservation
// is all-or-nothing: a refused one reserves nothing. Safe for concurrent use.
//
// The per-call cap is not checked here: it decides how the caller cuts the
// text (Split), and a reservation is for the whole of what a call will spend,
// made before its first engine request.
type Budget struct {
	mu        sync.Mutex
	charLimit int
	secLimit  float64
	chars     int
	seconds   float64
}

// NewBudget starts an empty budget under caps.
func NewBudget(caps Caps) *Budget {
	sec := caps.MaxSecondsPerRun
	if !(sec > 0) {
		sec = 0
	}
	return &Budget{charLimit: max(caps.MaxCharsPerRun, 0), secLimit: sec}
}

// ReserveChars reserves n characters, or returns a *CapError naming
// max_chars_per_run when they do not fit.
func (b *Budget) ReserveChars(n int) error {
	return b.Reserve(n, 0)
}

// ReserveSeconds reserves s seconds of audio, or returns a *CapError naming
// max_seconds_per_run when they do not fit.
func (b *Budget) ReserveSeconds(s float64) error {
	if err := checkSeconds(s); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fitsSecondsLocked(s); err != nil {
		return err
	}
	b.seconds += s
	return nil
}

// Reserve reserves n characters and s seconds together, or neither: a call
// spends both, and reserving them one at a time would leave the characters
// held when the seconds are refused. s may be zero (characters only); n must
// be positive.
func (b *Budget) Reserve(n int, s float64) error {
	if n <= 0 {
		return fmt.Errorf("speech: cannot reserve %d characters", n)
	}
	if s != 0 {
		if err := checkSeconds(s); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// n <= charLimit first so chars+n cannot overflow on a huge request.
	if n > b.charLimit || b.chars > b.charLimit-n {
		return &CapError{Cap: "max_chars_per_run", Unit: "chars", Limit: float64(b.charLimit), Used: float64(b.chars), Requested: float64(n)}
	}
	if s != 0 {
		if err := b.fitsSecondsLocked(s); err != nil {
			return err
		}
	}
	b.chars += n
	b.seconds += s
	return nil
}

func checkSeconds(s float64) error {
	if !(s > 0) || math.IsInf(s, 0) {
		return fmt.Errorf("speech: cannot reserve %v seconds", s)
	}
	return nil
}

func (b *Budget) fitsSecondsLocked(s float64) error {
	if b.seconds+s > b.secLimit {
		return &CapError{Cap: "max_seconds_per_run", Unit: "seconds", Limit: b.secLimit, Used: b.seconds, Requested: s}
	}
	return nil
}

// ReleaseChars gives back n characters a reservation took and no request
// spent: a piece never sent, or a reservation a second budget refused. More
// than is held releases everything.
func (b *Budget) ReleaseChars(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chars = max(b.chars-n, 0)
}

// ReleaseSeconds gives back s seconds a reservation took and no audio used:
// the estimate was longer than what the engine returned. More than is held
// releases everything.
func (b *Budget) ReleaseSeconds(s float64) {
	if !(s > 0) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seconds = max(b.seconds-s, 0)
}

// AddSeconds counts s seconds the engine produced beyond what was reserved,
// without the cap check: the audio already exists. The count may pass the
// cap; the run's next reservation is then refused.
func (b *Budget) AddSeconds(s float64) {
	if !(s > 0) || math.IsInf(s, 0) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seconds += s
}

// Chars is how many characters the run has reserved.
func (b *Budget) Chars() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.chars
}

// Seconds is how many seconds of audio the run has reserved.
func (b *Budget) Seconds() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seconds
}
