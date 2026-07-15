package main

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestToCapabilityPatch covers the RFC BF config→providers boundary translation:
// nil in → nil out, and each set/unset field maps across faithfully (a pointer
// override survives, an unset field stays nil so it doesn't clobber a driver
// default when Apply runs).
func TestToCapabilityPatch(t *testing.T) {
	if got := toCapabilityPatch(nil); got != nil {
		t.Errorf("nil override must translate to nil patch, got %+v", got)
	}

	tru := true
	fls := false
	n := 262144
	in := &config.CapabilityOverride{
		SupportsThinking: &tru,
		SupportsVision:   &fls,
		MaxContextTokens: &n,
		// SupportsEffort / NativePromptCache / ParallelToolCalls intentionally
		// left nil to prove unset fields stay nil across the boundary.
	}
	got := toCapabilityPatch(in)
	if got == nil {
		t.Fatal("non-nil override translated to nil patch")
	}
	if got.SupportsThinking == nil || *got.SupportsThinking != true {
		t.Errorf("SupportsThinking = %v, want *true", got.SupportsThinking)
	}
	if got.SupportsVision == nil || *got.SupportsVision != false {
		t.Errorf("SupportsVision = %v, want *false", got.SupportsVision)
	}
	if got.MaxContextTokens == nil || *got.MaxContextTokens != n {
		t.Errorf("MaxContextTokens = %v, want *%d", got.MaxContextTokens, n)
	}
	if got.SupportsEffort != nil || got.NativePromptCache != nil || got.ParallelToolCalls != nil {
		t.Errorf("unset override fields must stay nil, got effort=%v cache=%v parallel=%v",
			got.SupportsEffort, got.NativePromptCache, got.ParallelToolCalls)
	}

	// The translated patch must overlay correctly onto a base Capabilities —
	// end-to-end proof the boundary + the providers-side Apply agree.
	base := providers.Capabilities{SupportsEffort: true, SupportsVision: true}
	out := got.Apply(base)
	if !out.SupportsThinking || out.SupportsVision || out.MaxContextTokens != n {
		t.Errorf("Apply(base) = %+v, want thinking=true vision=false max=%d", out, n)
	}
	if !out.SupportsEffort {
		t.Error("SupportsEffort should be untouched by the patch (still true)")
	}
}
