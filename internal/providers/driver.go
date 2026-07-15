package providers

import (
	"fmt"
	"sort"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// CapabilityPatch is the providers-side mirror of config.CapabilityOverride
// (RFC BF): an operator override of a driver's advertised Capabilities. Every
// field is a pointer so "unset" (nil) is distinct from "set to false/0" — an
// override that only flips SupportsVision must not silently zero the rest.
//
// The config package cannot be imported here (internal/providers is a
// dependency of config's downstream consumers and a config→providers edge would
// cycle), so the config.CapabilityOverride → CapabilityPatch translation lives
// at the composition root in cmd/loomcycle (toCapabilityPatch). This type is the
// providers-side half of that boundary.
type CapabilityPatch struct {
	SupportsThinking  *bool
	SupportsVision    *bool
	SupportsEffort    *bool
	NativePromptCache *bool
	ParallelToolCalls *bool
	MaxContextTokens  *int
}

// Apply overlays the non-nil fields of p onto c and returns the result. A nil
// receiver returns c unchanged, so a driver can call
// `return d.capsPatch.Apply(base)` unconditionally whether or not an override
// was configured. Mirrors how the DeepSeek driver hand-patches its inner caps —
// the patch is a config-driven generalisation of that pattern.
func (p *CapabilityPatch) Apply(c Capabilities) Capabilities {
	if p == nil {
		return c
	}
	if p.SupportsThinking != nil {
		c.SupportsThinking = *p.SupportsThinking
	}
	if p.SupportsVision != nil {
		c.SupportsVision = *p.SupportsVision
	}
	if p.SupportsEffort != nil {
		c.SupportsEffort = *p.SupportsEffort
	}
	if p.NativePromptCache != nil {
		c.NativePromptCache = *p.NativePromptCache
	}
	if p.ParallelToolCalls != nil {
		c.ParallelToolCalls = *p.ParallelToolCalls
	}
	if p.MaxContextTokens != nil {
		c.MaxContextTokens = *p.MaxContextTokens
	}
	return c
}

// DriverOptions is the construction input a DriverFactory receives (RFC BF): the
// operator's `providers:` entry translated into the shape a driver needs to
// build one instance. It is the LLM sibling of EmbedderOptions — everything a
// driver's New(...) needs, sourced from config + env at the composition root.
type DriverOptions struct {
	// ID is the provider identity this instance reports from Provider.ID()
	// (the key under `providers:` — e.g. "ollama-local" for the "ollama"
	// driver). Empty defaults to the driver's canonical name in the factory.
	ID string
	// Dialect is the wire dialect the driver should speak; empty resolves to
	// the driver's canonical default (see NewDriver). Validated against the
	// driver's registered dialect set before the factory runs.
	Dialect string
	// BaseURL overrides the driver's default endpoint. Empty = driver default.
	BaseURL string
	// APIKey is the operator host key for this provider. Tenant/user overrides
	// (RFC AR) still resolve per-call via the credential store; this is the
	// fallback host key the driver was constructed with.
	APIKey string
	// KeyEnvName is the well-known env-var NAME whose tenant/user credential
	// overrides the host key (RFC AR/AX). Empty leaves the driver's built-in
	// default (e.g. "OPENAI_API_KEY"); the DeepSeek/OpenAI factories forward it
	// via SetKeyEnvName so a self-hosted OpenAI-compatible mirror can name its
	// own key var.
	KeyEnvName string
	// StreamOpts carries the per-stream HTTP timeouts (header + idle). Zero
	// fields resolve to streamhttp defaults inside each driver's New(...).
	StreamOpts streamhttp.Options
	// Options carries driver-specific tuning from the `providers:` entry's
	// `options:` map (e.g. ollama num_ctx/num_gpu, code-js code_root). Unknown
	// keys are logged via Logf and ignored, never fatal (see WarnUnknownOptions).
	Options map[string]any
	// Capabilities is the optional operator override applied inside the driver's
	// Capabilities(). Nil = advertise the driver's built-in defaults.
	Capabilities *CapabilityPatch
	// Logf is the boot-time logger for advisory warnings (unknown option keys).
	// Nil is tolerated (warnings are dropped).
	Logf func(string, ...any)
}

// DriverFactory builds one Provider instance from DriverOptions. Errors are
// surfaced to the operator at boot — typical failure modes mirror the embedder
// side ("missing API key", "bad base URL").
type DriverFactory func(DriverOptions) (Provider, error)

// driverEntry is one registered driver: the ordered set of wire dialects it can
// speak (first = canonical default) plus its factory.
type driverEntry struct {
	dialects []string
	factory  DriverFactory
}

var (
	driverRegistryMu sync.RWMutex
	driverRegistry   = map[string]driverEntry{}
)

// RegisterDriver records a driver name → (dialects, factory) mapping. Drivers
// call this from their init() so the consumer side (cmd/loomcycle) never needs
// to know which drivers are compiled in. dialects must be non-empty; the first
// entry is the canonical default NewDriver picks when DriverOptions.Dialect is
// blank.
//
// Panics on a duplicate driver name (unlike RegisterEmbedder, which allows
// re-registration): two packages claiming the same driver name is a build-time
// programming error, not a runtime fake-injection seam — tests exercise
// NewDriver against the real registered factories rather than re-registering.
func RegisterDriver(driver string, dialects []string, f DriverFactory) {
	if len(dialects) == 0 {
		panic(fmt.Sprintf("providers: driver %q registered with no dialects", driver))
	}
	driverRegistryMu.Lock()
	defer driverRegistryMu.Unlock()
	if _, dup := driverRegistry[driver]; dup {
		panic(fmt.Sprintf("providers: driver %q already registered", driver))
	}
	// Copy so a caller mutating its slice after registration can't corrupt the
	// registry's dialect set.
	dcopy := append([]string(nil), dialects...)
	driverRegistry[driver] = driverEntry{dialects: dcopy, factory: f}
}

// NewDriver looks up the registered factory for `driver` and returns a
// configured Provider. Unknown driver → error listing RegisteredDrivers() (an
// operator with a typo sees what's available). When o.Dialect is empty it is
// defaulted to the driver's canonical dialect; when set, it must be one of the
// driver's registered dialects (error otherwise).
func NewDriver(driver string, o DriverOptions) (Provider, error) {
	driverRegistryMu.RLock()
	entry, ok := driverRegistry[driver]
	driverRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider driver %q (known: %v)", driver, RegisteredDrivers())
	}
	if o.Dialect == "" {
		o.Dialect = entry.dialects[0]
	} else if !containsString(entry.dialects, o.Dialect) {
		return nil, fmt.Errorf("driver %q does not support dialect %q (supported: %v)", driver, o.Dialect, entry.dialects)
	}
	return entry.factory(o)
}

// RegisteredDrivers returns the sorted list of registered driver names. Used in
// error messages + operator introspection so "which drivers are wired in" is
// answerable without grepping the binary.
func RegisteredDrivers() []string {
	driverRegistryMu.RLock()
	defer driverRegistryMu.RUnlock()
	out := make([]string, 0, len(driverRegistry))
	for name := range driverRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DriverDialects returns the ordered dialect set a driver supports (first =
// canonical default), and whether the driver is registered.
func DriverDialects(driver string) ([]string, bool) {
	driverRegistryMu.RLock()
	entry, ok := driverRegistry[driver]
	driverRegistryMu.RUnlock()
	if !ok {
		return nil, false
	}
	return append([]string(nil), entry.dialects...), true
}

// DefaultDialect returns a driver's canonical dialect (the first registered),
// and whether the driver is registered.
func DefaultDialect(driver string) (string, bool) {
	driverRegistryMu.RLock()
	entry, ok := driverRegistry[driver]
	driverRegistryMu.RUnlock()
	if !ok || len(entry.dialects) == 0 {
		return "", false
	}
	return entry.dialects[0], true
}

// WarnUnknownOptions logs (via logf, when non-nil) each key in opts that is not
// in known. Drivers call this from their factory so an operator's typo'd
// provider option surfaces as a boot warning instead of being silently dropped.
// Advisory only — an unknown option never fails construction.
func WarnUnknownOptions(logf func(string, ...any), driver string, opts map[string]any, known ...string) {
	if logf == nil || len(opts) == 0 {
		return
	}
	knownSet := make(map[string]bool, len(known))
	for _, k := range known {
		knownSet[k] = true
	}
	for k := range opts {
		if !knownSet[k] {
			logf("provider driver %q: ignoring unknown option %q", driver, k)
		}
	}
}

// IntOption reads an integer driver option, tolerating the int/int64/float64
// shapes the YAML and JSON decoders produce for a numeric `options:` value.
// Returns (0,false) when the key is absent or not a number.
func IntOption(opts map[string]any, key string) (int, bool) {
	v, ok := opts[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// StringOption reads a string driver option. Returns ("",false) when absent or
// not a string.
func StringOption(opts map[string]any, key string) (string, bool) {
	v, ok := opts[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// BoolOption reads a bool driver option. Returns (false,false) when absent or
// not a bool.
func BoolOption(opts map[string]any, key string) (bool, bool) {
	v, ok := opts[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// containsString reports whether s is in xs. Local helper to keep NewDriver's
// dialect check dependency-free.
func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
