package http

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// TestStallFeedbackClosures_UseLoopProvidedArgsNotConstructionPin pins
// the v0.12.7+ correctness fix for the markStalledFn / markRateLimitedFn
// / clearStallFn closure factories: they must use the live (provider,
// model) the loop passes at INVOCATION time, NOT the (provider, model)
// captured at the factory construction time.
//
// Why this matters: v0.8.2 fallback (tryProviderFallback) mutates
// opts.Provider and opts.Model in place when a retryable error
// triggers a provider switch. The loop's subsequent MarkStalled /
// MarkRateLimited / ClearStall calls pass the LIVE pair via
// opts.Provider.ID() + opts.Model — which is the post-fallback
// provider, not the original. Closures that pin to construction-time
// args poison the WRONG matrix entry and praise the WRONG entry.
//
// Pre-2026-05-27 the closures pinned. Code review during the
// MarkRateLimited PR caught this; the fix replaces capture with
// pass-through. This test pins the contract.
func TestStallFeedbackClosures_UseLoopProvidedArgsNotConstructionPin(t *testing.T) {
	// Minimal Server: just the resolver. The closure factories are
	// `*Server` methods and only touch s.resolver.
	r := resolve.NewResolver(
		[]string{"anthropic", "deepseek"},
		map[string][]resolve.Candidate{
			"middle": {
				{Provider: "anthropic", Model: "claude-sonnet-4-6"},
				{Provider: "deepseek", Model: "deepseek-v4-flash"},
			},
		},
	)
	r.SeedModel("anthropic", "claude-sonnet-4-6")
	r.SeedModel("deepseek", "deepseek-v4-flash")
	r.SetProviderReachable("anthropic", true)
	r.SetProviderReachable("deepseek", true)
	srv := &Server{resolver: r}

	t.Run("markRateLimitedFn uses loop args", func(t *testing.T) {
		// Factory called with "original" pair — that's what existed
		// at run start, before fallback.
		fn := srv.markRateLimitedFn("anthropic", "claude-sonnet-4-6")
		if fn == nil {
			t.Fatal("factory returned nil despite resolver set")
		}
		// Loop invokes with "post-fallback" pair — that's the one
		// that actually 429'd.
		fn("deepseek", "deepseek-v4-flash", 1*time.Hour)

		// Assert: deepseek/deepseek-v4-flash is rate-limited, NOT
		// anthropic/claude-sonnet-4-6.
		snap := r.Snapshot()
		if ds := snap["deepseek"].Models["deepseek-v4-flash"]; !ds.RateLimited {
			t.Error("closure routed RateLimited to wrong matrix entry: deepseek/deepseek-v4-flash should have RateLimited=true")
		}
		if anthr := snap["anthropic"].Models["claude-sonnet-4-6"]; anthr.RateLimited {
			t.Error("closure poisoned the construction-pin pair: anthropic/claude-sonnet-4-6 has RateLimited=true (should be false)")
		}
	})

	t.Run("markStalledFn uses loop args", func(t *testing.T) {
		// Reset by re-probing both providers.
		r.SetReachable("anthropic", true, []string{"claude-sonnet-4-6"}, "")
		r.SetReachable("deepseek", true, []string{"deepseek-v4-flash"}, "")

		fn := srv.markStalledFn("anthropic", "claude-sonnet-4-6")
		if fn == nil {
			t.Fatal("factory returned nil")
		}
		fn("deepseek", "deepseek-v4-flash", "upstream 500")

		snap := r.Snapshot()
		if !snap["deepseek"].Models["deepseek-v4-flash"].Stalled {
			t.Error("closure routed Stalled to wrong entry: deepseek/deepseek-v4-flash should be stalled")
		}
		if snap["anthropic"].Models["claude-sonnet-4-6"].Stalled {
			t.Error("closure poisoned the construction-pin pair: anthropic/claude-sonnet-4-6 is stalled (should not be)")
		}
	})

	t.Run("clearStallFn uses loop args", func(t *testing.T) {
		// Set both stalled so we can prove the closure cleared the
		// right one.
		r.MarkStalled("anthropic", "claude-sonnet-4-6", "test")
		r.MarkStalled("deepseek", "deepseek-v4-flash", "test")

		fn := srv.clearStallFn("anthropic", "claude-sonnet-4-6")
		if fn == nil {
			t.Fatal("factory returned nil")
		}
		fn("deepseek", "deepseek-v4-flash")

		snap := r.Snapshot()
		// deepseek's stall cleared, anthropic's still set.
		if snap["deepseek"].Models["deepseek-v4-flash"].Stalled {
			t.Error("closure cleared wrong entry: deepseek/deepseek-v4-flash should be unstalled")
		}
		if !snap["anthropic"].Models["claude-sonnet-4-6"].Stalled {
			t.Error("closure cleared construction-pin pair instead of loop-arg pair")
		}
	})
}

// TestStallFeedbackClosures_NilResolverReturnsNil pins the back-compat
// path: with no resolver wired (Server.resolver == nil), the factories
// return nil and the loop treats that as "feedback disabled."
func TestStallFeedbackClosures_NilResolverReturnsNil(t *testing.T) {
	srv := &Server{resolver: nil}

	if fn := srv.markStalledFn("a", "b"); fn != nil {
		t.Error("markStalledFn should return nil when resolver is unset")
	}
	if fn := srv.markRateLimitedFn("a", "b"); fn != nil {
		t.Error("markRateLimitedFn should return nil when resolver is unset")
	}
	if fn := srv.clearStallFn("a", "b"); fn != nil {
		t.Error("clearStallFn should return nil when resolver is unset")
	}
}
