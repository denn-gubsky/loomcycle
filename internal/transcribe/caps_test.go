package transcribe

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

func operator() config.TranscriptionConfig {
	return config.TranscriptionConfig{
		OCR: &config.OCRConfig{Engine: config.OCREngineVision, MaxPagesPerCall: 50, MaxPagesPerRun: 100},
		STT: &config.STTConfig{Engine: config.STTEngineOpenAIAudio, MaxMinutesPerCall: 30, MaxMinutesPerRun: 60},
	}
}

// TestEffective_DefaultsOCRNeverAndSTTOn — an agent that says nothing gets no
// OCR (the cost is opt-in) and STT whenever the operator configured an engine.
func TestEffective_DefaultsOCRNeverAndSTTOn(t *testing.T) {
	c := Effective(operator(), config.AgentTranscription{})
	if c.OCR != OCRNever {
		t.Errorf("ocr = %q, want never", c.OCR)
	}
	if !c.STT {
		t.Error("stt = off, want on (the operator configured an engine)")
	}
	if c.MaxPagesPerRun != 100 || c.MaxPagesPerCall != 50 || c.MaxMinutesPerRun != 60 || c.MaxMinutesPerCall != 30 {
		t.Errorf("caps = %+v, want the operator's", c)
	}
}

// TestEffective_AgentCapsOnlyLower — min semantics: an agent value below the
// operator's lowers it, one above (a stored def the operator later undercut)
// does not raise it, and the per-call cap never exceeds the per-run one.
func TestEffective_AgentCapsOnlyLower(t *testing.T) {
	low := Effective(operator(), config.AgentTranscription{OCR: "auto", MaxPagesPerRun: 20, MaxMinutesPerRun: 10})
	if low.MaxPagesPerRun != 20 || low.MaxMinutesPerRun != 10 {
		t.Errorf("lowered caps = %+v, want 20 pages / 10 minutes", low)
	}
	if low.MaxPagesPerCall != 20 || low.MaxMinutesPerCall != 10 {
		t.Errorf("per-call caps = %d / %d, want them held to the lowered per-run caps", low.MaxPagesPerCall, low.MaxMinutesPerCall)
	}
	high := Effective(operator(), config.AgentTranscription{OCR: "always", MaxPagesPerRun: 1000, MaxMinutesPerRun: 1000})
	if high.MaxPagesPerRun != 100 || high.MaxMinutesPerRun != 60 {
		t.Errorf("raised caps = %+v, want the operator's 100 / 60", high)
	}
	if high.OCR != OCRAlways {
		t.Errorf("ocr = %q, want always", high.OCR)
	}
}

// TestEffective_EngineOffDisablesWhatTheAgentAsks — an agent's opt-in cannot
// turn on an engine the operator has not configured, and the caps are zero so
// any reservation fails before an engine is reached.
func TestEffective_EngineOffDisablesWhatTheAgentAsks(t *testing.T) {
	ask := config.AgentTranscription{OCR: "always", STT: "on", MaxPagesPerRun: 5, MaxMinutesPerRun: 5}
	for name, op := range map[string]config.TranscriptionConfig{
		"absent block": {},
		"engines off": {
			OCR: &config.OCRConfig{Engine: config.OCREngineOff, MaxPagesPerRun: 100},
			STT: &config.STTConfig{Engine: config.STTEngineOff, MaxMinutesPerRun: 60},
		},
	} {
		c := Effective(op, ask)
		if c.OCR != OCRNever || c.STT || c.MaxPagesPerRun != 0 || c.MaxMinutesPerRun != 0 {
			t.Errorf("%s: caps = %+v, want everything off", name, c)
		}
	}
	if c := Effective(operator(), config.AgentTranscription{STT: "off"}); c.STT || c.MaxMinutesPerRun != 0 {
		t.Errorf("agent stt: off = %+v, want stt off with no minutes", c)
	}
}

// TestEffective_AppliesOperatorDefaults — an unresolved operator block (built
// in code rather than loaded) gets the documented defaults, so a caller cannot
// end up with a zero cap by forgetting to resolve it.
func TestEffective_AppliesOperatorDefaults(t *testing.T) {
	c := Effective(config.TranscriptionConfig{OCR: &config.OCRConfig{Engine: config.OCREngineVision}}, config.AgentTranscription{OCR: "auto"})
	if c.MaxPagesPerRun != 300 || c.MaxPagesPerCall != 100 {
		t.Errorf("caps = %+v, want the 300 / 100 defaults", c)
	}
}

// TestBudget_ReservationOverPerRunCapNamesTheCapAndReservesNothing — the
// refusal is structured and leaves the counter where it was, so the next,
// smaller reservation still fits.
func TestBudget_ReservationOverPerRunCapNamesTheCapAndReservesNothing(t *testing.T) {
	b := NewBudget(Caps{MaxPagesPerRun: 10, MaxMinutesPerRun: 1})
	if err := b.ReservePages(8); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	err := b.ReservePages(3)
	var ce *CapError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a *CapError", err)
	}
	if ce.Cap != "max_pages_per_run" || ce.Limit != 10 || ce.Used != 8 || ce.Requested != 3 {
		t.Errorf("cap error = %+v, want max_pages_per_run limit 10 used 8 requested 3", ce)
	}
	if !errors.Is(err, ErrCapExceeded) {
		t.Error("a cap error must match ErrCapExceeded")
	}
	if !strings.Contains(err.Error(), "max_pages_per_run") {
		t.Errorf("message %q does not name the cap", err)
	}
	if b.Pages() != 8 {
		t.Errorf("pages = %d after a refused reservation, want 8", b.Pages())
	}
	if err := b.ReservePages(2); err != nil {
		t.Errorf("a reservation that exactly fills the cap was refused: %v", err)
	}

	if err := b.ReserveSeconds(45); err != nil {
		t.Fatalf("seconds: %v", err)
	}
	err = b.ReserveSeconds(15.5)
	if !errors.As(err, &ce) || ce.Cap != "max_minutes_per_run" || ce.Limit != 60 || ce.Used != 45 {
		t.Errorf("err = %v, want max_minutes_per_run limit 60 s used 45 s", err)
	}
	if b.Seconds() != 45 {
		t.Errorf("seconds = %v after a refused reservation, want 45", b.Seconds())
	}
}

// TestBudget_RefusesNonsenseAmounts — zero, negative and non-finite amounts are
// errors, not free reservations, and none of them moves the counter.
func TestBudget_RefusesNonsenseAmounts(t *testing.T) {
	b := NewBudget(Caps{MaxPagesPerRun: 10, MaxMinutesPerRun: 10})
	for _, n := range []int{0, -1} {
		if err := b.ReservePages(n); err == nil {
			t.Errorf("ReservePages(%d) succeeded", n)
		}
	}
	for _, s := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if err := b.ReserveSeconds(s); err == nil {
			t.Errorf("ReserveSeconds(%v) succeeded", s)
		}
	}
	if b.Pages() != 0 || b.Seconds() != 0 {
		t.Errorf("counters moved: %d pages, %v s", b.Pages(), b.Seconds())
	}
}

// TestBudget_DisabledEngineRefusesEveryReservation — zero caps (an engine off)
// refuse the first page and the first second.
func TestBudget_DisabledEngineRefusesEveryReservation(t *testing.T) {
	b := NewBudget(Caps{})
	if err := b.ReservePages(1); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("pages err = %v, want ErrCapExceeded", err)
	}
	if err := b.ReserveSeconds(1); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("seconds err = %v, want ErrCapExceeded", err)
	}
}

// TestBudget_ConcurrentReservationsNeverOvershoot — run under -race: many
// goroutines reserving at once fill the cap exactly and never past it.
func TestBudget_ConcurrentReservationsNeverOvershoot(t *testing.T) {
	const limit = 100
	b := NewBudget(Caps{MaxPagesPerRun: limit, MaxMinutesPerRun: limit})
	var wg sync.WaitGroup
	var mu sync.Mutex
	okPages, okSeconds := 0, 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if b.ReservePages(1) == nil {
					mu.Lock()
					okPages++
					mu.Unlock()
				}
				if b.ReserveSeconds(60) == nil {
					mu.Lock()
					okSeconds++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if okPages != limit || b.Pages() != limit {
		t.Errorf("granted %d page reservations, counter %d; want exactly %d", okPages, b.Pages(), limit)
	}
	if okSeconds != limit || b.Seconds() != limit*60 {
		t.Errorf("granted %d second reservations, counter %v; want exactly %d", okSeconds, b.Seconds(), limit)
	}
}

// TestBudget_ReleaseSecondsGivesBackUnspentAudio — seconds a request never
// spent go back, and a release larger than what is held floors at zero.
func TestBudget_ReleaseSecondsGivesBackUnspentAudio(t *testing.T) {
	b := NewBudget(Caps{MaxMinutesPerRun: 1})
	if err := b.ReserveSeconds(50); err != nil {
		t.Fatal(err)
	}
	b.ReleaseSeconds(20)
	if err := b.ReserveSeconds(30); err != nil {
		t.Fatalf("after the release the seconds must fit again: %v", err)
	}
	b.ReleaseSeconds(1000)
	b.ReleaseSeconds(-5)
	if b.Seconds() != 0 {
		t.Errorf("seconds = %v, want 0", b.Seconds())
	}
}

// TestBudget_AddSecondsCountsPastTheCap — audio the engine reported beyond
// the reservation is counted even past the cap, and the next reservation is
// refused.
func TestBudget_AddSecondsCountsPastTheCap(t *testing.T) {
	b := NewBudget(Caps{MaxMinutesPerRun: 1})
	if err := b.ReserveSeconds(50); err != nil {
		t.Fatal(err)
	}
	b.AddSeconds(30)
	if b.Seconds() != 80 {
		t.Errorf("seconds = %v, want 80", b.Seconds())
	}
	if err := b.ReserveSeconds(1); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("a reservation past the cap: %v", err)
	}
}
