package http

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
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
	srv := &Server{
		resolver: r,
		// Empty cfg so markRateLimitedFn(tier) finds no overlay
		// and uses the resolver's hardcoded 30s default. The
		// per-tier substitution path is covered by
		// TestMarkRateLimitedFn_SubstitutesTierCooldown below.
		cfg: &config.Config{},
	}

	t.Run("markRateLimitedFn uses loop args", func(t *testing.T) {
		// Factory called with default tier — the closure has no
		// per-tier cooldown to substitute; the loop's retryAfter
		// flows through unchanged.
		fn := srv.markRateLimitedFn("default")
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
	if fn := srv.markRateLimitedFn("default"); fn != nil {
		t.Error("markRateLimitedFn should return nil when resolver is unset")
	}
	if fn := srv.clearStallFn("a", "b"); fn != nil {
		t.Error("clearStallFn should return nil when resolver is unset")
	}
}

// TestMarkRateLimitedFn_SubstitutesTierCooldown pins the v0.12.x
// operator-tunable behaviour: when the loop passes retryAfter=0
// (the common "use default" case), the closure substitutes the
// per-tier rate_limit_cooldown_ms from the operator's yaml. A
// non-zero retryAfter from the loop still takes precedence
// (future-Retry-After-header threading is operator-trust-supreme).
func TestMarkRateLimitedFn_SubstitutesTierCooldown(t *testing.T) {
	r := resolve.NewResolver(
		[]string{"anthropic"},
		map[string][]resolve.Candidate{
			"middle": {{Provider: "anthropic", Model: "haiku"}},
		},
	)
	r.SeedModel("anthropic", "haiku")
	r.SetProviderReachable("anthropic", true)
	srv := &Server{
		resolver: r,
		cfg: &config.Config{
			UserTiers: map[string]config.UserTier{
				// 5 s cooldown — under the resolver's 30 s default
				// so a re-Resolve right after the closure call still
				// returns the model after a 5 s wait.
				"fast": {
					RateLimitCooldownMs: 5_000,
				},
				// 0 = use the resolver's hardcoded default.
				"unset": {},
			},
		},
	}

	t.Run("operator cooldown substitutes when loop passes zero", func(t *testing.T) {
		fn := srv.markRateLimitedFn("fast")
		if fn == nil {
			t.Fatal("factory returned nil")
		}
		// Loop passes 0 — closure must substitute 5 s.
		fn("anthropic", "haiku", 0)

		snap := r.Snapshot()
		until := snap["anthropic"].Models["haiku"].RateLimitedUntil
		now := time.Now()
		// Cooldown should be ~5 s, not the resolver's 30 s default.
		elapsed := until.Sub(now)
		if elapsed < 4*time.Second || elapsed > 6*time.Second {
			t.Errorf("cooldown deadline = %v from now, want ~5s (operator-supplied), got something else", elapsed)
		}
	})

	t.Run("loop-supplied non-zero takes precedence", func(t *testing.T) {
		r.SetReachable("anthropic", true, []string{"haiku"}, "")
		fn := srv.markRateLimitedFn("fast")
		// Loop passes explicit 1 hour — must NOT be overridden by the
		// 5 s tier value.
		fn("anthropic", "haiku", 1*time.Hour)

		snap := r.Snapshot()
		until := snap["anthropic"].Models["haiku"].RateLimitedUntil
		elapsed := until.Sub(time.Now())
		if elapsed < 50*time.Minute {
			t.Errorf("cooldown deadline = %v from now, want ~1h (loop precedence)", elapsed)
		}
	})

	t.Run("unset tier uses resolver default", func(t *testing.T) {
		r.SetReachable("anthropic", true, []string{"haiku"}, "")
		fn := srv.markRateLimitedFn("unset")
		fn("anthropic", "haiku", 0)

		snap := r.Snapshot()
		until := snap["anthropic"].Models["haiku"].RateLimitedUntil
		elapsed := until.Sub(time.Now())
		// Resolver default is 30 s.
		if elapsed < 28*time.Second || elapsed > 32*time.Second {
			t.Errorf("cooldown deadline = %v from now, want ~30s (resolver default)", elapsed)
		}
	})

	t.Run("unknown tier (no overlay) uses resolver default", func(t *testing.T) {
		r.SetReachable("anthropic", true, []string{"haiku"}, "")
		fn := srv.markRateLimitedFn("does-not-exist")
		fn("anthropic", "haiku", 0)

		snap := r.Snapshot()
		until := snap["anthropic"].Models["haiku"].RateLimitedUntil
		elapsed := until.Sub(time.Now())
		if elapsed < 28*time.Second || elapsed > 32*time.Second {
			t.Errorf("cooldown deadline = %v from now, want ~30s (resolver default for unknown tier)", elapsed)
		}
	})
}

// TestClampRateLimitCooldownMs pins the operator-doc-promised bounds
// on the per-tier cooldown.
func TestClampRateLimitCooldownMs(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 0},         // unset passes through
		{-100, 0},      // negative -> unset (validator already refused)
		{1, 1_000},     // below 1s floor
		{500, 1_000},   // below 1s floor
		{1_000, 1_000}, // at floor
		{30_000, 30_000},
		{600_000, 600_000},   // at ceiling
		{900_000, 600_000},   // above ceiling
		{3_600_000, 600_000}, // way above ceiling
	}
	for _, tc := range cases {
		if got := clampRateLimitCooldownMs(tc.in); got != tc.want {
			t.Errorf("clamp(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
