package sttpass

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/transcribe"
)

// The File tool's suite (internal/tools/builtin/file_transcribe_test.go)
// covers the routes, cutting, conversion and caps through op=transcribe. These
// tests cover what the pass adds for its second caller: a slot per request,
// and a reservation over any number of budgets.

// wav8k is a PCM WAV of 8-bit mono at 8 kHz lasting d.
func wav8k(d time.Duration) []byte {
	const rate = 8000
	data := make([]byte, int(d.Seconds()*rate))
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate), uint16(1), uint16(8)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// engine answers every request with one segment, and counts them.
type engine struct {
	accepts bool
	mu      sync.Mutex
	calls   int
}

func (e *engine) Name() string           { return "openai-audio" }
func (e *engine) Accepts(mt string) bool { return e.accepts }
func (e *engine) count() int             { e.mu.Lock(); defer e.mu.Unlock(); return e.calls }
func (e *engine) Plan(context.Context) (transcribe.STTPlan, error) {
	return transcribe.STTPlan{Engine: "openai-audio", Provider: "whisper", Model: "large-v3", MaxBytes: 25 << 20}, nil
}
func (e *engine) Transcribe(_ context.Context, _ transcribe.STTPlan, u transcribe.Unit, _ transcribe.STTOptions) (transcribe.STTResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return transcribe.STTResult{Reached: true, Segments: transcribe.Segments{{End: u.Span.End - u.Span.Start, Text: "слово"}}}, nil
}

// minuteChunks cuts a WAV into one-minute pieces with no overlap.
func minuteChunks(e *engine) Deps {
	return Deps{Engine: e, Config: &config.STTConfig{ChunkMinutes: 1, TimeoutSeconds: 60}}
}

var bigCaps = transcribe.Caps{STT: true, MaxMinutesPerCall: 120, MaxMinutesPerRun: 300}

func prepare(t *testing.T, d Deps, rec []byte) Plan {
	t.Helper()
	plan, err := Prepare(context.Background(), d, bigCaps, bytes.NewReader(rec), int64(len(rec)), "audio/wav")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

func TestRun_TakesASlotPerRequestAndReleasesIt(t *testing.T) {
	e := &engine{}
	d := minuteChunks(e)
	var mu sync.Mutex
	held, most, taken, given := 0, 0, 0, 0
	d.Slot = func(context.Context) (func(), error) {
		mu.Lock()
		defer mu.Unlock()
		held++
		taken++
		most = max(most, held)
		return func() { mu.Lock(); held--; given++; mu.Unlock() }, nil
	}
	rec := wav8k(150 * time.Second)
	plan := prepare(t, d, rec)
	b := transcribe.NewBudget(bigCaps)
	p := Run(context.Background(), d, plan, bytes.NewReader(rec), transcribe.STTOptions{}, bigCaps, []*transcribe.Budget{b})
	if p.Failure() != nil {
		t.Fatalf("pass failed: %v", p.Failure())
	}
	if e.count() != 3 || taken != 3 || given != 3 || most != 1 {
		t.Fatalf("calls=%d slots taken=%d given back=%d most held at once=%d, want 3/3/3/1", e.count(), taken, given, most)
	}
	if got := b.Seconds(); got != 150 {
		t.Fatalf("budget holds %v seconds, want 150", got)
	}
}

func TestRun_RefusedSlotStopsBeforeTheRequestAndGivesTheSecondsBack(t *testing.T) {
	e := &engine{}
	d := minuteChunks(e)
	n := 0
	d.Slot = func(ctx context.Context) (func(), error) {
		if n++; n == 2 {
			return nil, context.Canceled
		}
		return func() {}, nil
	}
	rec := wav8k(150 * time.Second)
	plan := prepare(t, d, rec)
	b := transcribe.NewBudget(bigCaps)
	p := Run(context.Background(), d, plan, bytes.NewReader(rec), transcribe.STTOptions{}, bigCaps, []*transcribe.Budget{b})
	if !errors.Is(p.CallErr, context.Canceled) || p.Done != 1 || p.Status() != "partial" {
		t.Fatalf("CallErr=%v done=%d status=%s, want Canceled, 1, partial", p.CallErr, p.Done, p.Status())
	}
	if e.count() != 1 {
		t.Fatalf("engine got %d requests, want 1: the refused slot's request must not be sent", e.count())
	}
	if got := b.Seconds(); got != 60 {
		t.Fatalf("budget holds %v seconds, want 60 (the one piece sent)", got)
	}
}

func TestRun_ReservesOnEveryBudgetOrNone(t *testing.T) {
	e := &engine{}
	d := minuteChunks(e)
	rec := wav8k(150 * time.Second)
	plan := prepare(t, d, rec)
	roomy := transcribe.NewBudget(bigCaps)
	tight := transcribe.NewBudget(transcribe.Caps{MaxMinutesPerRun: 2})
	p := Run(context.Background(), d, plan, bytes.NewReader(rec), transcribe.STTOptions{}, bigCaps, []*transcribe.Budget{roomy, tight})
	if p.CapErr == nil || p.CapErr.Cap != "max_minutes_per_run" {
		t.Fatalf("CapErr = %v, want max_minutes_per_run", p.CapErr)
	}
	if e.count() != 0 || roomy.Seconds() != 0 || tight.Seconds() != 0 {
		t.Fatalf("calls=%d roomy=%v tight=%v: a refused reservation must send nothing and hold nothing", e.count(), roomy.Seconds(), tight.Seconds())
	}
}

// A browser recording (webm) on a host with no converter goes to an engine
// that reads it whole, bounded by its size; one the engine does not read is
// refused before anything is sent.
func TestPrepare_EngineReadableFileGoesWholeWithoutAConverter(t *testing.T) {
	webm := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 64<<10)...)
	plan, err := Prepare(context.Background(), minuteChunks(&engine{accepts: true}), bigCaps, bytes.NewReader(webm), int64(len(webm)), "audio/webm")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if plan.Route != RouteWhole || plan.Exact || len(plan.Pieces) != 1 || plan.Pieces[0].To <= 0 {
		t.Fatalf("route=%s exact=%v pieces=%v, want one whole piece bounded by the size", plan.Route, plan.Exact, plan.Pieces)
	}

	_, err = Prepare(context.Background(), minuteChunks(&engine{accepts: false}), bigCaps, bytes.NewReader(webm), int64(len(webm)), "audio/webm")
	var pe *PrepareError
	if !errors.As(err, &pe) || pe.Kind != NotWhole {
		t.Fatalf("err = %v, want a NotWhole PrepareError", err)
	}
}
