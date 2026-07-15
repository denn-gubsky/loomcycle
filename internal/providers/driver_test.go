package providers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// fakeProvider is a minimal Provider so a synthetic DriverFactory can return
// something without pulling a real driver package into this test (which would
// couple the registry-mechanics tests to driver construction). Its ID echoes the
// DriverOptions.ID so tests can assert the factory received the option.
type fakeProvider struct {
	id      string
	dialect string
	caps    providers.Capabilities
}

func (f *fakeProvider) ID() string                           { return f.id }
func (f *fakeProvider) Capabilities() providers.Capabilities { return f.caps }
func (f *fakeProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	return nil, nil
}
func (f *fakeProvider) Probe(context.Context) error                  { return nil }
func (f *fakeProvider) ListModels(context.Context) ([]string, error) { return []string{}, nil }

func fakeFactory(o providers.DriverOptions) (providers.Provider, error) {
	return &fakeProvider{id: o.ID, dialect: o.Dialect}, nil
}

// TestRegisterDriver_LookupAndUnknown covers the happy path (register → NewDriver
// returns an instance the factory built) and the unknown-driver error, which
// must name the registered set so an operator with a typo can self-correct.
func TestRegisterDriver_LookupAndUnknown(t *testing.T) {
	providers.RegisterDriver("test-basic", []string{"d1", "d2"}, fakeFactory)

	got, err := providers.NewDriver("test-basic", providers.DriverOptions{ID: "inst-a"})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	if got.ID() != "inst-a" {
		t.Errorf("factory did not receive DriverOptions.ID: got %q, want %q", got.ID(), "inst-a")
	}

	_, err = providers.NewDriver("test-nope", providers.DriverOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown provider driver") {
		t.Errorf("unknown driver: got %v, want error naming the unknown driver", err)
	}
	if err != nil && !strings.Contains(err.Error(), "test-basic") {
		t.Errorf("unknown-driver error should list registered drivers, got %v", err)
	}
}

// TestRegisterDriver_DuplicatePanics locks the build-time invariant: two
// packages claiming one driver name is a programming error, not a fake seam.
func TestRegisterDriver_DuplicatePanics(t *testing.T) {
	providers.RegisterDriver("test-dup", []string{"d1"}, fakeFactory)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("re-registering a driver name must panic")
		}
		if !strings.Contains(strings.ToLower(err2str(r)), "already registered") {
			t.Errorf("panic message = %v, want 'already registered'", r)
		}
	}()
	providers.RegisterDriver("test-dup", []string{"d1"}, fakeFactory)
}

// TestRegisterDriver_NoDialectsPanics locks that a driver must declare at least
// one dialect (the canonical default has to exist).
func TestRegisterDriver_NoDialectsPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("registering a driver with no dialects must panic")
		}
	}()
	providers.RegisterDriver("test-nodialect", nil, fakeFactory)
}

// TestNewDriver_DialectDefaultAndValidation covers dialect resolution: empty
// defaults to the canonical (first) dialect; an explicit-but-unsupported dialect
// errors; an explicit-and-supported dialect passes through.
func TestNewDriver_DialectDefaultAndValidation(t *testing.T) {
	providers.RegisterDriver("test-dialect", []string{"canon", "alt"}, fakeFactory)

	// Empty dialect → canonical default reaches the factory.
	got, err := providers.NewDriver("test-dialect", providers.DriverOptions{ID: "x"})
	if err != nil {
		t.Fatalf("NewDriver (default dialect): %v", err)
	}
	if fp := got.(*fakeProvider); fp.dialect != "canon" {
		t.Errorf("default dialect = %q, want canonical %q", fp.dialect, "canon")
	}

	// Explicit supported dialect passes through untouched.
	got, err = providers.NewDriver("test-dialect", providers.DriverOptions{ID: "x", Dialect: "alt"})
	if err != nil {
		t.Fatalf("NewDriver (explicit dialect): %v", err)
	}
	if fp := got.(*fakeProvider); fp.dialect != "alt" {
		t.Errorf("explicit dialect = %q, want %q", fp.dialect, "alt")
	}

	// Unsupported dialect errors, naming the supported set.
	_, err = providers.NewDriver("test-dialect", providers.DriverOptions{Dialect: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "does not support dialect") {
		t.Errorf("unsupported dialect: got %v, want a 'does not support dialect' error", err)
	}
}

// TestDriverIntrospection covers the registry read helpers.
func TestDriverIntrospection(t *testing.T) {
	providers.RegisterDriver("test-introspect", []string{"first", "second"}, fakeFactory)

	if d, ok := providers.DefaultDialect("test-introspect"); !ok || d != "first" {
		t.Errorf("DefaultDialect = (%q,%v), want (first,true)", d, ok)
	}
	if _, ok := providers.DefaultDialect("test-absent"); ok {
		t.Error("DefaultDialect for an unregistered driver should return ok=false")
	}
	ds, ok := providers.DriverDialects("test-introspect")
	if !ok || len(ds) != 2 || ds[0] != "first" || ds[1] != "second" {
		t.Errorf("DriverDialects = (%v,%v), want ([first second],true)", ds, ok)
	}
	// RegisteredDrivers is sorted and includes what we registered here.
	if !containsStr(providers.RegisteredDrivers(), "test-introspect") {
		t.Error("RegisteredDrivers should include test-introspect")
	}
}

// TestCapabilityPatch_Apply covers the overlay: nil receiver is a no-op; a patch
// only touches the fields it sets (unset stays at the base value).
func TestCapabilityPatch_Apply(t *testing.T) {
	base := providers.Capabilities{
		SupportsThinking: false,
		SupportsVision:   true,
		MaxContextTokens: 100,
	}

	// Nil patch → unchanged.
	if got := (*providers.CapabilityPatch)(nil).Apply(base); got != base {
		t.Errorf("nil patch changed caps: %+v", got)
	}

	// Only SupportsThinking + MaxContextTokens overridden; SupportsVision stays.
	yes := true
	n := 4096
	got := (&providers.CapabilityPatch{SupportsThinking: &yes, MaxContextTokens: &n}).Apply(base)
	if !got.SupportsThinking {
		t.Error("SupportsThinking not overridden to true")
	}
	if got.MaxContextTokens != 4096 {
		t.Errorf("MaxContextTokens = %d, want 4096", got.MaxContextTokens)
	}
	if !got.SupportsVision {
		t.Error("SupportsVision should be untouched (still true)")
	}
}

// TestOptionHelpers covers the numeric/string/bool option readers, including the
// float64 shape a JSON decoder produces for an integer literal.
func TestOptionHelpers(t *testing.T) {
	opts := map[string]any{
		"i":     42,
		"i64":   int64(7),
		"jsonN": float64(2048), // JSON decodes integers to float64
		"s":     "hello",
		"b":     true,
	}
	if n, ok := providers.IntOption(opts, "i"); !ok || n != 42 {
		t.Errorf("IntOption(i) = (%d,%v)", n, ok)
	}
	if n, ok := providers.IntOption(opts, "i64"); !ok || n != 7 {
		t.Errorf("IntOption(i64) = (%d,%v)", n, ok)
	}
	if n, ok := providers.IntOption(opts, "jsonN"); !ok || n != 2048 {
		t.Errorf("IntOption(jsonN) = (%d,%v)", n, ok)
	}
	if _, ok := providers.IntOption(opts, "s"); ok {
		t.Error("IntOption on a string should return ok=false")
	}
	if s, ok := providers.StringOption(opts, "s"); !ok || s != "hello" {
		t.Errorf("StringOption(s) = (%q,%v)", s, ok)
	}
	if b, ok := providers.BoolOption(opts, "b"); !ok || !b {
		t.Errorf("BoolOption(b) = (%v,%v)", b, ok)
	}
	if _, ok := providers.StringOption(opts, "absent"); ok {
		t.Error("StringOption on an absent key should return ok=false")
	}
}

func err2str(r any) string {
	if e, ok := r.(error); ok {
		return e.Error()
	}
	if s, ok := r.(string); ok {
		return s
	}
	return ""
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
