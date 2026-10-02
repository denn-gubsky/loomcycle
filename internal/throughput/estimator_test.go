package throughput

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func newEst(t *testing.T, ts config.TimeoutScaling, local ...string) *Estimator {
	t.Helper()
	cfg := &config.Config{TimeoutScaling: ts, Models: map[string]config.ModelRef{
		"local-medium": {Provider: "ollama-local", Model: "qwen3.6"},
	}}
	isLocal := func(p string) bool {
		for _, l := range local {
			if l == p {
				return true
			}
		}
		return false
	}
	e := New(cfg, isLocal)
	if e == nil {
		t.Fatal("New returned nil for a measuring config")
	}
	return e
}

// slowCall is a call whose slowdown against the default reference is exactly
// s: no prompt, 100 output tokens, so T_ref = 1 s TTFT + 1 s decode = 2 s.
func slowCall(run string, at time.Time, provider, model string, s float64) Sample {
	return Sample{RunID: run, At: at, Key: Key{provider, model}, Output: 100,
		CallTiming: providers.CallTiming{DurationMs: int64(math.Round(s * 2000))}}
}

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol*want }

func feed(e *Estimator, n int, provider, model string, s float64) {
	for i := 0; i < n; i++ {
		e.Observe(slowCall(fmt.Sprintf("r%d", i), t0.Add(time.Duration(i)*time.Second), provider, model, s))
	}
}

func TestEstimator_ConvergesOnASteadyModel(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	feed(e, 60, "p", "m", 5)
	st := e.Stat("p", "m")
	if st.Samples != 60 || st.Source != SourceMeasured {
		t.Fatalf("stat = %+v, want 60 measured samples", st)
	}
	if !near(st.Slowdown, 5, 0.001) {
		t.Errorf("slowdown = %v, want 5", st.Slowdown)
	}
	// The first sample seeds the deviation at |x|/2 (RFC 6298); a steady model
	// decays it toward 0, so the multiplier converges on the slowdown.
	if !near(st.Multiplier, 5, 0.01) {
		t.Errorf("multiplier = %v, want ≈5 once the deviation has decayed", st.Multiplier)
	}
}

func TestEstimator_ClipsAPathologicalSample(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	feed(e, 40, "p", "m", 2)
	before := math.Log(e.Stat("p", "m").Slowdown)
	e.Observe(slowCall("odd", t0.Add(time.Hour), "p", "m", 1000))
	after := math.Log(e.Stat("p", "m").Slowdown)
	// The sample enters as ln 8 (the max multiplier), not ln 1000.
	want := (1-alpha)*before + alpha*math.Log(8)
	if !near(after, want, 1e-9) {
		t.Fatalf("ln slowdown after the outlier = %v, want %v (clipped to ln 8)", after, want)
	}

	first := newEst(t, config.TimeoutScaling{})
	first.Observe(slowCall("r", t0, "p", "m", 1e6))
	if s := first.Stat("p", "m").Slowdown; !near(s, 8, 1e-9) {
		t.Errorf("a first sample of 1e6 gave slowdown %v, want the clip, 8", s)
	}
}

// A slowdown is a ratio, so it is averaged in log space: a model alternating 2×
// and 8× is a 4× model with real spread, not a 5× model.
func TestEstimator_AveragesAndSpreadsInLogSpace(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	for i := 0; i < 80; i++ {
		s := 2.0
		if i%2 == 1 {
			s = 8
		}
		e.Observe(slowCall(fmt.Sprintf("r%d", i), t0.Add(time.Duration(i)*time.Second), "p", "m", s))
	}
	st := e.Stat("p", "m")
	if !near(st.Slowdown, 4, 0.1) {
		t.Errorf("slowdown = %v, want ≈4 (the geometric mean), not 5", st.Slowdown)
	}
	// The deviation of ln s is ≈ ln 2 here, so the multiplier sits about one
	// factor of 2 above the mean, clamped to 8.
	if st.Multiplier < 6.5 || st.Multiplier > 8 {
		t.Errorf("multiplier = %v, want ≈ 4·2 (mean plus one log-space deviation)", st.Multiplier)
	}
}

func TestEstimator_FastModelGetsExactlyOne(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{}, "ollama-local")
	feed(e, 20, "anthropic", "claude", 0.3)
	if m := e.Stat("anthropic", "claude").Multiplier; m != 1 {
		t.Fatalf("multiplier = %v, want exactly 1 for a model faster than the reference", m)
	}
}

func TestEstimator_BelowMinSamplesFallsBackOverrideThenProviderThenPrior(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{Models: map[string]config.TimeoutScalingModel{
		"local-medium":   {DecodeTPS: 20}, // alias → ollama-local/qwen3.6, 100/20 = 5×
		"openai/pinned2": {Multiplier: 2},
	}}, "ollama-local")

	check := func(provider, model string, wantM float64, wantSrc string) {
		t.Helper()
		st := e.Stat(provider, model)
		if !near(st.Multiplier, wantM, 0.01) || st.Source != wantSrc {
			t.Errorf("%s/%s: multiplier %v from %q, want %v from %q", provider, model, st.Multiplier, st.Source, wantM, wantSrc)
		}
	}
	// Nothing measured: override, else the prior (4× local, 1× elsewhere).
	check("ollama-local", "qwen3.6", 5, SourceOverride)
	check("openai", "pinned2", 2, SourceOverride)
	check("ollama-local", "never-seen", 4, SourcePrior)
	check("openai", "never-seen", 1, SourcePrior)

	// Another ollama-local model reaches min_samples at a steady 3×: the
	// provider's unmeasured models now inherit it — but an override still wins.
	feed(e, 30, "ollama-local", "gpt-oss", 3)
	check("ollama-local", "never-seen", 3, SourceProvider)
	check("ollama-local", "qwen3.6", 5, SourceOverride)

	// Four samples of its own are not enough; the fifth makes it measured.
	feed(e, 4, "ollama-local", "fresh", 6)
	check("ollama-local", "fresh", 3, SourceProvider)
	e.Observe(slowCall("r-last", t0.Add(time.Hour), "ollama-local", "fresh", 6))
	if st := e.Stat("ollama-local", "fresh"); st.Source != SourceMeasured || st.Samples != 5 {
		t.Errorf("after 5 samples: %+v, want measured", st)
	}
}

func TestEstimator_CapsSamplesPerRunPerWindow(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	accepted := 0
	for i := 0; i < 10; i++ {
		if e.Observe(slowCall("one-long-chat", t0.Add(time.Duration(i)*time.Second), "p", "m", 2)) {
			accepted++
		}
	}
	if accepted != 4 {
		t.Fatalf("accepted %d samples from one run in a minute, want 4", accepted)
	}
	if !e.Observe(slowCall("one-long-chat", t0.Add(5*time.Minute), "p", "m", 2)) {
		t.Error("a sample in the run's next 5-minute window was refused")
	}
	if !e.Observe(slowCall("another-run", t0.Add(10*time.Second), "p", "m", 2)) {
		t.Error("another run's sample was refused by the first run's cap")
	}
	if !e.Observe(slowCall("", t0.Add(11*time.Second), "p", "m", 2)) {
		t.Error("an off-run call was capped")
	}
}

// Cold load and queue time are not the model's speed: a call that waited 30 s
// to load and 20 s in line measures like the same call warm and alone, while the
// load is tracked as the cold-load cost and the wait as queue time.
func TestEstimator_ExcludesColdLoadAndQueueFromTheSpeed(t *testing.T) {
	warm := newEst(t, config.TimeoutScaling{})
	cold := newEst(t, config.TimeoutScaling{})
	for i := 0; i < 10; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		run := fmt.Sprintf("r%d", i)
		warm.Observe(slowCall(run, at, "p", "m", 3))
		c := slowCall(run, at, "p", "m", 3)
		c.LoadMs, c.QueueMs = 30_000, 20_000
		c.DurationMs += c.LoadMs + c.QueueMs
		cold.Observe(c)
	}
	w, c := warm.Stat("p", "m"), cold.Stat("p", "m")
	if !near(c.Slowdown, w.Slowdown, 1e-9) {
		t.Errorf("slowdown with cold load + queue = %v, warm = %v; want equal", c.Slowdown, w.Slowdown)
	}
	if c.ColdLoadMs != 30_000 || w.ColdLoadMs != 0 {
		t.Errorf("cold_load_ms = %v (cold) / %v (warm), want 30000 / 0", c.ColdLoadMs, w.ColdLoadMs)
	}
	if c.QueueMsP50 != 20_000 {
		t.Errorf("queue p50 = %v, want 20000", c.QueueMsP50)
	}
}

// On a decode-dominated call the slowdown is the owner's formula: reference
// decode speed over the model's.
func TestEstimator_DecodeHeavySampleIsReferenceOverDecodeSpeed(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	// 10,000 output tokens at 25 tok/s after a 1 s first token: 401 s wall.
	e.Observe(Sample{Key: Key{"p", "m"}, Output: 10_000,
		CallTiming: providers.CallTiming{DurationMs: 401_000, TTFTMs: 1000}})
	st := e.Stat("p", "m")
	if !near(st.Slowdown, 100.0/25, 0.01) {
		t.Errorf("slowdown = %v, want ≈ 100/25 = 4", st.Slowdown)
	}
	if !near(st.DecodeTPS, 25, 0.001) {
		t.Errorf("decode_tps = %v, want 25", st.DecodeTPS)
	}
}

// On a prefill-heavy call (an extractor: a long transcript in, little out) the
// slowdown carries the prefill cost a decode-only figure would miss.
func TestEstimator_PrefillHeavySampleCountsThePrefill(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	// 8,000 tokens in at 200 tok/s (40 s), 200 out at 25 tok/s (8 s).
	e.Observe(Sample{Key: Key{"p", "m"}, UncachedInput: 8000, Output: 200,
		CallTiming: providers.CallTiming{DurationMs: 48_000, PrefillMs: 40_000, DecodeMs: 8000, TTFTMs: 40_000}})
	st := e.Stat("p", "m")
	// T_ref = 1 + 8000/2000 + 200/100 = 7 s → 48/7, well above the decode-only 4.
	if !near(st.Slowdown, 48.0/7, 0.001) {
		t.Errorf("slowdown = %v, want 48/7 ≈ 6.86", st.Slowdown)
	}
	if !near(st.PrefillTPS, 200, 0.001) || !near(st.DecodeTPS, 25, 0.001) {
		t.Errorf("prefill/decode tps = %v / %v, want 200 / 25", st.PrefillTPS, st.DecodeTPS)
	}
}

// A reply that arrives in one burst has a decode window of a millisecond; its
// slowdown still counts, but no tokens-per-second figure is read from it.
func TestEstimator_ABurstReplyGivesNoDecodeSpeed(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	e.Observe(Sample{Key: Key{"p", "m"}, UncachedInput: 80, Output: 32,
		CallTiming: providers.CallTiming{DurationMs: 1503, TTFTMs: 1502}})
	st := e.Stat("p", "m")
	if st.Samples != 1 || st.DecodeTPS != 0 {
		t.Fatalf("stat = %+v, want one sample and no decode_tps (a 1 ms window would read as 32,000 tok/s)", st)
	}
}

func TestEstimator_IgnoresWhatIsNotASample(t *testing.T) {
	e := newEst(t, config.TimeoutScaling{})
	for name, s := range map[string]Sample{
		"short reply":       {Key: Key{"p", "m"}, Output: 15, CallTiming: providers.CallTiming{DurationMs: 5000}},
		"no wall time":      {Key: Key{"p", "m"}, Output: 100},
		"load exceeds wall": {Key: Key{"p", "m"}, Output: 100, CallTiming: providers.CallTiming{DurationMs: 500, LoadMs: 900}},
		"unattributed":      {Output: 100, CallTiming: providers.CallTiming{DurationMs: 5000}},
	} {
		if e.Observe(s) {
			t.Errorf("%s became a sample", name)
		}
	}
	if len(e.All()) != 0 {
		t.Errorf("All = %+v, want nothing learned", e.All())
	}
	if _, ok := SampleFromUsage("r", t0, &providers.Usage{Provider: "p", Model: "m", OutputTokens: 100}); ok {
		t.Error("an untimed usage (an errored call) became a sample")
	}
}

func TestEstimator_OffModeIsNilAndNilIsSafe(t *testing.T) {
	var e *Estimator = New(&config.Config{TimeoutScaling: config.TimeoutScaling{Mode: "off"}}, nil)
	if e != nil {
		t.Fatal("mode off built an estimator")
	}
	if e.Observe(slowCall("r", t0, "p", "m", 2)) || e.All() != nil || e.Stat("p", "m") != (Stat{}) || e.Mode() != "" {
		t.Error("a nil estimator did something")
	}
	if n, err := e.Seed(context.Background(), &fakeLedger{}, t0); n != 0 || err != nil {
		t.Errorf("nil Seed = %d, %v", n, err)
	}
}

type fakeLedger struct {
	rows  []store.TokenUsageRow
	since time.Time
	per   int
}

func (f *fakeLedger) RecentCallTimings(_ context.Context, since time.Time, per int) ([]store.TokenUsageRow, error) {
	f.since, f.per = since, per
	return f.rows, nil
}

// The seed replays the ledger through Observe, so a restarted replica knows
// what it knew — and the per-run cap applies to replayed calls as it did live.
func TestEstimator_SeedRestoresTheEstimateFromTheLedger(t *testing.T) {
	live := newEst(t, config.TimeoutScaling{})
	var rows []store.TokenUsageRow
	add := func(run string, at time.Time, s float64) {
		c := slowCall(run, at, "ollama-local", "qwen3.6", s)
		live.Observe(c)
		rows = append(rows, store.TokenUsageRow{RunID: run, TS: at, Provider: "ollama-local", Model: "qwen3.6",
			OutputTokens: 100, DurationMs: c.DurationMs})
	}
	for i := 0; i < 8; i++ { // one chat, 8 calls in 80 s: the cap keeps 4
		add("chat", t0.Add(time.Duration(i)*10*time.Second), 6)
	}
	for i := 0; i < 6; i++ {
		add(fmt.Sprintf("r%d", i), t0.Add(time.Duration(i)*time.Minute), 3)
	}
	l := &fakeLedger{rows: rows}
	seeded := newEst(t, config.TimeoutScaling{})
	now := t0.Add(time.Hour)
	n, err := seeded.Seed(context.Background(), l, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("seeded %d samples, want 10 (4 capped chat calls + 6 runs)", n)
	}
	if !l.since.Equal(now.Add(-7*24*time.Hour)) || l.per != 200 {
		t.Errorf("seed read since %v, %d per model; want the last 7 days, 200 per model", l.since, l.per)
	}
	if want, got := live.Stat("ollama-local", "qwen3.6"), seeded.Stat("ollama-local", "qwen3.6"); got != want {
		t.Fatalf("seeded estimate %+v, want the live one %+v", got, want)
	}
}
