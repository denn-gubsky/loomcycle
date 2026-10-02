// Package throughput measures how fast each (provider, model) actually is, as a
// slowdown against a reference machine, and the timeout multiplier that slowdown
// implies (RFC DT).
//
// It only measures and reports. Nothing in the runtime reads the multiplier to
// change a timeout yet; that is a later phase, gated by the `timeout_scaling`
// mode.
//
// # The sample
//
// One completed call with at least minOutputTokens of output gives one sample,
// its slowdown against the reference machine:
//
//	T_ref = ttft_ref + uncached_input / prefill_ref + output / decode_ref
//	s     = (wall − load − queue) / T_ref
//
// On a decode-dominated call s ≈ decode_ref / decode_tps; on a prefill-heavy one
// it grows with the prefill cost, which a pure output-tokens-per-second figure
// would miss. Model load and queue time are subtracted: a cold load is tracked
// separately, and queueing behind other calls measures the box's load, not the
// model's speed.
//
// # The aggregate
//
// Per (provider, model), as RFC 6298 smooths a round-trip time, but in log space
// (a slowdown is a ratio): a smoothed mean μ and mean deviation σ of ln s, with
// each sample clipped to ±ln(cap). The multiplier is clamp(exp(μ + σ), 1, cap),
// so a model at or above the reference gets exactly 1. The cap is the model's
// own timeout_scaling.models max_multiplier when set, else the block's.
//
// Keyed globally by the provider id and model that actually SERVED the call
// (post-fallback), in memory, per replica; seeded from the usage ledger at boot.
// The model is always the CONCRETE one: a usage or ledger row that names a
// `models:` alias is filed under the model the alias names, and a lookup by
// alias finds it there, so the routing view (which shows concrete models) and
// every other reader see one model, not two.
package throughput

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// alpha and beta are RFC 6298's gains for the mean and the deviation.
	alpha = 1.0 / 8
	beta  = 1.0 / 4
	// sigmaK weights the deviation in the multiplier. TCP uses 4; the bases
	// these multiply already carry headroom, so 1.
	sigmaK = 1.0

	// minOutputTokens: a reply shorter than this is mostly fixed overhead and
	// says little about speed.
	minOutputTokens = 16
	// coldLoadMs: a load longer than this was a cold load, not resident-model
	// noise, and is tracked as the model's cold-load cost.
	coldLoadMs = 1000
	// minPhaseMs: a raw tokens-per-second figure is taken only from a phase at
	// least this long. A reply delivered in one burst (a non-streaming backend,
	// or a short coalesced stream) has a decode window of a millisecond or two,
	// and dividing by it reports tens of thousands of tok/s. The slowdown, which
	// uses the whole call, is unaffected.
	minPhaseMs = 100

	// runCap samples per run per runWindow, so one long chat cannot dominate
	// the estimate.
	runCap    = 4
	runWindow = 5 * time.Minute

	// maxKeys bounds the number of (provider, model) pairs tracked. Model names
	// come from what a provider reports serving, so this is a backstop, not a
	// limit anyone should meet.
	maxKeys = 1024
	// queueRing is how many recent queue times feed the reported median.
	queueRing = 64

	// Seed window: the latest seedPerModel timed calls of each model within
	// seedMaxAge.
	seedPerModel = 200
	seedMaxAge   = 7 * 24 * time.Hour
)

// Sources of a reported multiplier.
const (
	SourceMeasured = "measured" // the model's own samples (>= min_samples)
	SourceOverride = "override" // the operator's timeout_scaling.models entry
	SourceProvider = "provider" // the provider's other measured models
	SourcePrior    = "prior"    // local_prior for a local provider, else 1
)

// Key identifies one served model.
type Key struct {
	Provider string
	Model    string
}

// Sample is one completed call, as the estimator reads it.
type Sample struct {
	RunID string
	At    time.Time
	// Key is what the usage or ledger row names, alias or not; Observe files
	// it under the concrete model.
	Key
	// UncachedInput is the prompt the model had to process: input plus cache
	// creation. Drivers disagree on whether InputTokens includes cache reads
	// (Anthropic excludes them, OpenAI includes them); counting input + cache
	// creation is exact for Anthropic and Ollama and, for OpenAI, over-counts
	// the prompt — which only lowers the slowdown, the safe direction.
	UncachedInput int
	Output        int
	providers.CallTiming
}

// SampleFromUsage reads a sample from a call's usage. ok is false when the
// usage carries no timing or does not name what served it.
func SampleFromUsage(runID string, at time.Time, u *providers.Usage) (Sample, bool) {
	if u == nil || u.Timing == nil || u.Provider == "" || u.Model == "" {
		return Sample{}, false
	}
	return Sample{
		RunID: runID, At: at, Key: Key{u.Provider, u.Model},
		UncachedInput: u.InputTokens + u.CacheCreationTokens,
		Output:        u.OutputTokens,
		CallTiming:    *u.Timing,
	}, true
}

// sampleFromRow reads a sample from a ledger row (the boot seed).
func sampleFromRow(r store.TokenUsageRow) (Sample, bool) {
	if r.DurationMs <= 0 || r.Provider == "" || r.Model == "" {
		return Sample{}, false
	}
	return Sample{
		RunID: r.RunID, At: r.TS, Key: Key{r.Provider, r.Model},
		UncachedInput: r.InputTokens + r.CacheCreationTokens,
		Output:        r.OutputTokens,
		CallTiming: providers.CallTiming{DurationMs: r.DurationMs, TTFTMs: r.TTFTMs, LoadMs: r.LoadMs,
			PrefillMs: r.PrefillMs, DecodeMs: r.DecodeMs, QueueMs: r.QueueMs},
	}, true
}

// Stat is what the estimate says about one model.
type Stat struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Samples is how many calls the model's own estimate is built from.
	Samples int `json:"samples"`
	// Slowdown is exp(μ): how many times slower than the reference the model
	// typically runs. 0 = no sample yet.
	Slowdown float64 `json:"slowdown,omitempty"`
	// Multiplier is what a scaled timeout WOULD be multiplied by, and Source
	// where it came from. Reported only: no timeout uses it in this version.
	Multiplier float64 `json:"multiplier"`
	Source     string  `json:"source"`
	// MaxMultiplier is the cap on this model's multiplier and on each of its
	// samples: its own timeout_scaling.models max_multiplier, else the global.
	MaxMultiplier float64 `json:"max_multiplier,omitempty"`
	// Smoothed raw speeds, where measurable. 0 = not measured.
	DecodeTPS  float64 `json:"decode_tps,omitempty"`
	PrefillTPS float64 `json:"prefill_tps,omitempty"`
	TTFTMs     float64 `json:"ttft_ms,omitempty"`
	ColdLoadMs float64 `json:"cold_load_ms,omitempty"`
	// QueueMsP50 is the median recent unaccounted-for wait — contention on the
	// box. Operator-facing only; 0 = none recorded.
	QueueMsP50 int64 `json:"queue_ms_p50,omitempty"`
}

// ewma is an exponentially-weighted mean that starts at its first value.
type ewma struct {
	v   float64
	set bool
}

func (e *ewma) add(x float64) {
	if !e.set {
		e.v, e.set = x, true
		return
	}
	e.v += alpha * (x - e.v)
}

type modelState struct {
	n         int
	mu, sigma float64 // of ln(slowdown)

	decodeTPS, prefillTPS, ttftMs, coldLoadMs ewma

	queue    []int64 // ring of recent queue times
	queueIdx int
}

type runWindowState struct {
	start time.Time
	n     int
}

// Estimator is the per-replica throughput estimate. Safe for concurrent use. A
// nil *Estimator (timeout_scaling.mode: off) is valid and measures nothing.
type Estimator struct {
	cfg     config.TimeoutScaling
	ref     reference
	isLocal func(provider string) bool
	// concrete names the concrete model behind a (provider, model) pair, so an
	// alias and the model it names share one key.
	concrete func(provider, model string) string
	// overrides are timeout_scaling.models resolved to the keys they name.
	// Read-only after New, so read without the lock.
	overrides map[Key]config.TimeoutScalingModel

	mu     sync.Mutex
	models map[Key]*modelState
	runs   map[string]*runWindowState
}

type reference struct {
	decodeTPS, prefillTPS, ttftSec float64
}

// New builds the estimator for cfg's timeout_scaling block, or nil when the mode
// is off. isLocal says whether a provider id is a local-inference backend (its
// prior is local_prior); nil = none is. concrete names the concrete model behind
// a (provider, model) pair; nil = cfg.ConcreteModel on this cfg. A server whose
// `models:` map reloads passes one that reads the live config.
func New(cfg *config.Config, isLocal func(provider string) bool, concrete func(provider, model string) string) *Estimator {
	ts := cfg.TimeoutScaling.WithDefaults()
	if ts.Mode == config.TimeoutScalingOff {
		return nil
	}
	if isLocal == nil {
		isLocal = func(string) bool { return false }
	}
	if concrete == nil {
		concrete = cfg.ConcreteModel
	}
	e := &Estimator{
		cfg:       ts,
		ref:       reference{ts.Reference.DecodeTPS, ts.Reference.PrefillTPS, float64(ts.Reference.TTFTMs) / 1000},
		isLocal:   isLocal,
		concrete:  concrete,
		overrides: map[Key]config.TimeoutScalingModel{},
		models:    map[Key]*modelState{},
		runs:      map[string]*runWindowState{},
	}
	for k, o := range ts.Models {
		if p, m, ok := cfg.ResolveModelKey(k); ok {
			e.overrides[e.key(p, m)] = o
		}
	}
	return e
}

// key is the one place a (provider, model) becomes a key — for a sample being
// learned, a ledger row being seeded, an override, and every lookup — so an
// alias and its model can never land on different keys.
func (e *Estimator) key(provider, model string) Key {
	return Key{provider, e.concrete(provider, model)}
}

// capFor is the model's multiplier cap. overrides and cfg are read-only after
// New, so this needs no lock.
func (e *Estimator) capFor(k Key) float64 {
	return e.cfg.CapFor(e.overrides[k])
}

// Mode reports the configured mode ("" for a nil estimator, i.e. off).
func (e *Estimator) Mode() string {
	if e == nil {
		return ""
	}
	return e.cfg.Mode
}

// Observe learns from one call. It reports whether the call became a sample:
// too-short replies, calls with no usable wall time, and calls past their run's
// cap are not.
func (e *Estimator) Observe(s Sample) bool {
	if e == nil || s.Provider == "" || s.Model == "" || s.Output < minOutputTokens || s.DurationMs <= 0 {
		return false
	}
	work := float64(s.DurationMs-s.LoadMs-s.QueueMs) / 1000
	tRef := e.ref.ttftSec + float64(s.UncachedInput)/e.ref.prefillTPS + float64(s.Output)/e.ref.decodeTPS
	if work <= 0 || tRef <= 0 {
		return false
	}
	k := e.key(s.Provider, s.Model)
	limit := math.Log(e.capFor(k))
	x := math.Max(-limit, math.Min(limit, math.Log(work/tRef)))

	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.models[k]
	if st == nil && len(e.models) >= maxKeys {
		return false
	}
	if !e.admitLocked(s.RunID, s.At) {
		return false
	}
	if st == nil {
		st = &modelState{}
		e.models[k] = st
	}
	// RFC 6298 order: the deviation is updated against the OLD mean.
	if st.n == 0 {
		st.mu, st.sigma = x, math.Abs(x)/2
	} else {
		st.sigma = (1-beta)*st.sigma + beta*math.Abs(st.mu-x)
		st.mu = (1-alpha)*st.mu + alpha*x
	}
	st.n++

	decodeMs := s.DecodeMs
	if decodeMs <= 0 && s.TTFTMs > 0 {
		decodeMs = s.DurationMs - s.TTFTMs
	}
	if decodeMs >= minPhaseMs {
		st.decodeTPS.add(float64(s.Output) / (float64(decodeMs) / 1000))
	}
	// A call queued behind another waits before its first token, so the wait
	// is part of the wall-clock TTFT; it is the box's load, not the model's.
	var ttftMs int64
	if s.TTFTMs > 0 {
		ttftMs = s.TTFTMs - s.QueueMs
	}
	prefillMs := s.PrefillMs
	if prefillMs <= 0 {
		prefillMs = ttftMs
	}
	if prefillMs >= minPhaseMs && s.UncachedInput > 0 {
		st.prefillTPS.add(float64(s.UncachedInput) / (float64(prefillMs) / 1000))
	}
	if ttftMs > 0 {
		st.ttftMs.add(float64(ttftMs))
	}
	if s.LoadMs > coldLoadMs {
		st.coldLoadMs.add(float64(s.LoadMs))
	}
	if s.QueueMs > 0 {
		if len(st.queue) < queueRing {
			st.queue = append(st.queue, s.QueueMs)
		} else {
			st.queue[st.queueIdx] = s.QueueMs
			st.queueIdx = (st.queueIdx + 1) % queueRing
		}
	}
	return true
}

// admitLocked enforces the per-run cap. A call outside any run (an off-run side
// call) is not capped.
func (e *Estimator) admitLocked(runID string, at time.Time) bool {
	if runID == "" {
		return true
	}
	if len(e.runs) > maxKeys {
		for id, w := range e.runs {
			if at.Sub(w.start) >= runWindow {
				delete(e.runs, id)
			}
		}
	}
	w := e.runs[runID]
	if w == nil || at.Sub(w.start) >= runWindow {
		e.runs[runID] = &runWindowState{start: at, n: 1}
		return true
	}
	if w.n >= runCap {
		return false
	}
	w.n++
	return true
}

// Stat reports the estimate for one model, including one it has never seen
// (its multiplier then comes from the fallback chain). A `models:` alias finds
// the model it names; the Stat names the concrete model.
func (e *Estimator) Stat(provider, model string) Stat {
	if e == nil {
		return Stat{}
	}
	k := e.key(provider, model)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statLocked(k)
}

// All reports every model with at least one sample, sorted by provider, model.
func (e *Estimator) All() []Stat {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Stat, 0, len(e.models))
	for k := range e.models {
		out = append(out, e.statLocked(k))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func (e *Estimator) statLocked(k Key) Stat {
	s := Stat{Provider: k.Provider, Model: k.Model}
	if st := e.models[k]; st != nil {
		s.Samples = st.n
		s.Slowdown = math.Exp(st.mu)
		s.DecodeTPS, s.PrefillTPS = st.decodeTPS.v, st.prefillTPS.v
		s.TTFTMs, s.ColdLoadMs = st.ttftMs.v, st.coldLoadMs.v
		s.QueueMsP50 = median(st.queue)
	}
	s.Multiplier, s.Source = e.multiplierLocked(k)
	s.MaxMultiplier = e.capFor(k)
	return s
}

// multiplierLocked is the fallback chain: the model's own estimate once it has
// min_samples; else an operator override; else the provider's other measured
// models; else the prior. Whatever the source, the model's own cap bounds it.
func (e *Estimator) multiplierLocked(k Key) (float64, string) {
	clamp := func(m float64) float64 { return math.Max(1, math.Min(e.capFor(k), m)) }
	if st := e.models[k]; st != nil && st.n >= e.cfg.MinSamples {
		return clamp(math.Exp(st.mu + sigmaK*st.sigma)), SourceMeasured
	}
	// An entry that sets only max_multiplier bounds the model but says nothing
	// about its speed, so it is no prior: fall through.
	if o, ok := e.overrides[k]; ok && (o.Multiplier > 0 || o.DecodeTPS > 0) {
		if o.Multiplier > 0 {
			return clamp(o.Multiplier), SourceOverride
		}
		return clamp(e.ref.decodeTPS / o.DecodeTPS), SourceOverride
	}
	var wMu, wSigma, n float64
	for other, st := range e.models {
		if other.Provider == k.Provider && other != k && st.n >= e.cfg.MinSamples {
			w := float64(st.n)
			wMu += w * st.mu
			wSigma += w * st.sigma
			n += w
		}
	}
	if n > 0 {
		return clamp(math.Exp((wMu + sigmaK*wSigma) / n)), SourceProvider
	}
	if e.isLocal(k.Provider) {
		return clamp(e.cfg.LocalPrior), SourcePrior
	}
	return 1, SourcePrior
}

func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]int64(nil), xs...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// Ledger is the store read the boot seed needs.
type Ledger interface {
	RecentCallTimings(ctx context.Context, since time.Time, perModel int) ([]store.TokenUsageRow, error)
}

// Seed replays the latest timed calls from the usage ledger — at most 200 per
// model from the last 7 days, oldest first, through the same Observe (cap
// included) a live call takes, so a restart does not forget what the replica
// knew. Returns how many rows became samples. Nil-safe.
func (e *Estimator) Seed(ctx context.Context, l Ledger, now time.Time) (int, error) {
	if e == nil || l == nil {
		return 0, nil
	}
	rows, err := l.RecentCallTimings(ctx, now.Add(-seedMaxAge), seedPerModel)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if s, ok := sampleFromRow(r); ok && e.Observe(s) {
			n++
		}
	}
	return n, nil
}
