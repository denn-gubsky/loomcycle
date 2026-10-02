package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// TimeoutScaling is the `timeout_scaling:` block (RFC DT). It configures the
// per-(provider, model) throughput measurement: how fast each model actually is
// against a reference machine, and the timeout multiplier that speed implies.
//
// Only measurement exists so far. In `measure` mode (the default) the runtime
// records and reports the numbers and changes NO timeout; `off` turns the
// estimate off entirely. Applying the multiplier comes in a later version, so
// `on` is refused rather than accepted and silently ignored.
type TimeoutScaling struct {
	// Mode is off | measure. Env LOOMCYCLE_TIMEOUT_SCALING wins when set.
	Mode string `yaml:"mode,omitempty"`
	// Reference is the machine a slowdown of 1 means.
	Reference TimeoutScalingReference `yaml:"reference,omitempty"`
	// MaxMultiplier caps the multiplier and clips each sample's slowdown, so one
	// pathological call cannot swing the estimate. Env
	// LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER wins when set.
	MaxMultiplier float64 `yaml:"max_multiplier,omitempty"`
	// MinSamples is how many measured calls a model needs before its own
	// estimate is trusted; below it the multiplier comes from an override, the
	// provider's other models, or the prior.
	MinSamples int `yaml:"min_samples,omitempty"`
	// LocalPrior is the multiplier assumed for a local-inference provider
	// (Capabilities().Local) before it has MinSamples; every other provider's
	// prior is 1.
	LocalPrior float64 `yaml:"local_prior,omitempty"`
	// Models are per-model settings, keyed "provider/model" or by a `models:`
	// alias. An entry sets at most one of decode_tps (→ multiplier
	// reference.decode_tps / decode_tps) or multiplier, the model's estimate
	// before it has MinSamples; and/or max_multiplier, its own cap.
	Models map[string]TimeoutScalingModel `yaml:"models,omitempty"`
}

// TimeoutScalingReference is the reference machine. A call's expected time is
// ttft_ms + uncached_input / prefill_tps + output / decode_tps.
type TimeoutScalingReference struct {
	// DecodeTPS is output tokens per second. Env
	// LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS wins when set.
	DecodeTPS  float64 `yaml:"decode_tps,omitempty"`
	PrefillTPS float64 `yaml:"prefill_tps,omitempty"`
	TTFTMs     int     `yaml:"ttft_ms,omitempty"`
}

// TimeoutScalingModel is one model's settings.
type TimeoutScalingModel struct {
	DecodeTPS  float64 `yaml:"decode_tps,omitempty"`
	Multiplier float64 `yaml:"multiplier,omitempty"`
	// MaxMultiplier replaces the block's max_multiplier for this model — both
	// the clip on each of its samples and the clamp on its multiplier — for a
	// model known to run slower than the global cap allows. Unlike decode_tps
	// and multiplier it holds at every sample count: it is a bound, not a
	// prior. 0 = the global cap.
	MaxMultiplier float64 `yaml:"max_multiplier,omitempty"`
}

// CapFor is the multiplier cap for the model a timeout_scaling.models entry
// describes: its own max_multiplier when set, else the block's.
func (t TimeoutScaling) CapFor(m TimeoutScalingModel) float64 {
	if m.MaxMultiplier > 0 {
		return m.MaxMultiplier
	}
	return t.MaxMultiplier
}

// Timeout-scaling modes and defaults.
const (
	TimeoutScalingOff     = "off"
	TimeoutScalingMeasure = "measure"

	defaultScalingDecodeTPS     = 100
	defaultScalingPrefillTPS    = 2000
	defaultScalingTTFTMs        = 1000
	defaultScalingMaxMultiplier = 8
	defaultScalingMinSamples    = 5
	defaultScalingLocalPrior    = 4
	// maxScalingMultiplier bounds every max_multiplier, the block's and each
	// model's: whatever they are set to, a scaled budget must stay bounded. One
	// ceiling for both, so a per-model cap can never reach past what the global
	// one could be set to.
	maxScalingMultiplier = 100
)

// WithDefaults returns t with every unset field at its default. A config built
// without LoadLayers (a test fixture) gets the same values a loaded one does.
func (t TimeoutScaling) WithDefaults() TimeoutScaling {
	if t.Mode == "" {
		t.Mode = TimeoutScalingMeasure
	}
	if t.Reference.DecodeTPS == 0 {
		t.Reference.DecodeTPS = defaultScalingDecodeTPS
	}
	if t.Reference.PrefillTPS == 0 {
		t.Reference.PrefillTPS = defaultScalingPrefillTPS
	}
	if t.Reference.TTFTMs == 0 {
		t.Reference.TTFTMs = defaultScalingTTFTMs
	}
	if t.MaxMultiplier == 0 {
		t.MaxMultiplier = defaultScalingMaxMultiplier
	}
	if t.MinSamples == 0 {
		t.MinSamples = defaultScalingMinSamples
	}
	if t.LocalPrior == 0 {
		t.LocalPrior = defaultScalingLocalPrior
	}
	return t
}

// ResolveModelKey turns a `models:` override key into the (provider, model) it
// names: a `models:` alias with a concrete model, or "provider/model" split at
// the first slash (a model id may itself contain slashes; a provider id never
// does).
func (c *Config) ResolveModelKey(key string) (provider, model string, ok bool) {
	if ref, isAlias := c.Models[key]; isAlias {
		if ref.Provider == "" || ref.Model == "" {
			return "", "", false // a model_pattern alias names no single model
		}
		return ref.Provider, ref.Model, true
	}
	provider, model, found := strings.Cut(key, "/")
	if !found || provider == "" || model == "" {
		return "", "", false
	}
	return provider, model, true
}

// applyTimeoutScaling applies the env overrides, then the defaults. An env value
// that does not parse is reported as a warning and ignored, like the other
// numeric env knobs; an unknown mode is left for validate to refuse.
func applyTimeoutScaling(cfg *Config) {
	ts := &cfg.TimeoutScaling
	if v := strings.TrimSpace(os.Getenv("LOOMCYCLE_TIMEOUT_SCALING")); v != "" {
		ts.Mode = strings.ToLower(v)
	}
	if v := os.Getenv("LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			ts.Reference.DecodeTPS = f
		} else {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("LOOMCYCLE_TIMEOUT_SCALING_REFERENCE_TPS=%q is not a positive number; ignored", v))
		}
	}
	if v := os.Getenv("LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			ts.MaxMultiplier = f
		} else {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("LOOMCYCLE_TIMEOUT_SCALING_MAX_MULTIPLIER=%q is not a positive number; ignored", v))
		}
	}
	*ts = ts.WithDefaults()
}

// validateTimeoutScaling refuses a block that would measure nonsense. Unset
// fields are fine (WithDefaults fills them), so a hand-built config validates.
func validateTimeoutScaling(c *Config) error {
	ts := c.TimeoutScaling.WithDefaults()
	switch ts.Mode {
	case TimeoutScalingOff, TimeoutScalingMeasure:
	case "on":
		return fmt.Errorf("timeout_scaling.mode: %q is not available in this version — timeouts are not scaled yet; use %q (record and report) or %q", ts.Mode, TimeoutScalingMeasure, TimeoutScalingOff)
	default:
		return fmt.Errorf("timeout_scaling.mode: unknown mode %q (want %q or %q)", ts.Mode, TimeoutScalingMeasure, TimeoutScalingOff)
	}
	if ts.Reference.DecodeTPS < 0 || ts.Reference.PrefillTPS < 0 || ts.Reference.TTFTMs < 0 {
		return fmt.Errorf("timeout_scaling.reference: decode_tps, prefill_tps and ttft_ms must be positive")
	}
	if ts.MaxMultiplier < 1 || ts.MaxMultiplier > maxScalingMultiplier {
		return fmt.Errorf("timeout_scaling.max_multiplier %v out of range [1,%d]", ts.MaxMultiplier, maxScalingMultiplier)
	}
	if ts.MinSamples < 1 {
		return fmt.Errorf("timeout_scaling.min_samples must be >= 1")
	}
	if ts.LocalPrior < 1 || ts.LocalPrior > ts.MaxMultiplier {
		return fmt.Errorf("timeout_scaling.local_prior %v out of range [1, max_multiplier %v]", ts.LocalPrior, ts.MaxMultiplier)
	}
	keys := make([]string, 0, len(ts.Models))
	for k := range ts.Models {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := ts.Models[k]
		if _, _, ok := c.ResolveModelKey(k); !ok {
			return fmt.Errorf("timeout_scaling.models.%s: want \"provider/model\" or a `models:` alias naming one model", k)
		}
		limit := ts.CapFor(m)
		switch {
		case m.MaxMultiplier != 0 && (m.MaxMultiplier < 1 || m.MaxMultiplier > maxScalingMultiplier):
			return fmt.Errorf("timeout_scaling.models.%s: max_multiplier %v out of range [1,%d]", k, m.MaxMultiplier, maxScalingMultiplier)
		case m.DecodeTPS > 0 && m.Multiplier != 0:
			return fmt.Errorf("timeout_scaling.models.%s: set at most one of decode_tps or multiplier", k)
		case m.DecodeTPS == 0 && m.Multiplier == 0 && m.MaxMultiplier == 0:
			return fmt.Errorf("timeout_scaling.models.%s: set decode_tps or multiplier, and/or max_multiplier", k)
		case m.DecodeTPS < 0:
			return fmt.Errorf("timeout_scaling.models.%s: decode_tps must be positive", k)
		case m.Multiplier != 0 && (m.Multiplier < 1 || m.Multiplier > limit):
			return fmt.Errorf("timeout_scaling.models.%s: multiplier %v out of range [1, max_multiplier %v]", k, m.Multiplier, limit)
		}
	}
	return nil
}
