package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// failingCallProvider fails every Call with a fixed error.
type failingCallProvider struct {
	id  string
	err error
}

func (p *failingCallProvider) ID() string                           { return p.id }
func (p *failingCallProvider) Capabilities() providers.Capabilities { return providers.Capabilities{} }
func (p *failingCallProvider) Probe(_ context.Context) error        { return nil }
func (p *failingCallProvider) ListModels(_ context.Context) ([]string, error) {
	return nil, nil
}
func (p *failingCallProvider) Call(_ context.Context, _ providers.Request) (<-chan providers.Event, error) {
	return nil, p.err
}

// The production incident end to end: one agent's run fails with a 400 on the
// tier's only model, through the server's real stall feedback, and the NEXT
// run on that tier must still resolve the model. A 5xx keeps the old
// behaviour — the model is stalled and the tier has nothing left.
func TestPermanentRequestError_NextRunOnTheTierStillResolvesTheModel(t *testing.T) {
	for _, tc := range []struct {
		name, msg   string
		wantStalled bool
	}{
		{"400", "anthropic 400: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"`temperature` is deprecated for this model.\"}}", false},
		{"5xx", "anthropic 529: overloaded_error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := resolve.NewResolver([]string{"bedrock-claude"}, map[string][]resolve.Candidate{
				"middle": {{Provider: "bedrock-claude", Model: "eu.anthropic.claude-haiku-5-5"}},
			})
			r.SetReachable("bedrock-claude", true, []string{"eu.anthropic.claude-haiku-5-5"}, "")
			srv := &Server{resolver: r, cfgHolder: config.NewHolder(&config.Config{})}

			_, err := loop.Run(context.Background(), loop.RunOptions{
				Provider:    &failingCallProvider{id: "bedrock-claude", err: errors.New(tc.msg)},
				Model:       "eu.anthropic.claude-haiku-5-5",
				Segments:    []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
				MarkStalled: srv.markStalledFn("bedrock-claude", "eu.anthropic.claude-haiku-5-5"),
			})
			if err == nil {
				t.Fatal("the failing run should return its error")
			}

			d, rerr := r.Resolve(resolve.AgentRequest{Name: "other-agent", Tier: "middle"})
			if tc.wantStalled {
				if rerr == nil {
					t.Fatalf("after a %s the tier still resolved to %s/%s; want the model stalled", tc.name, d.Provider, d.Model)
				}
				return
			}
			if rerr != nil {
				t.Fatalf("after a %s the next run on the tier failed to resolve: %v", tc.name, rerr)
			}
			if d.Model != "eu.anthropic.claude-haiku-5-5" {
				t.Errorf("next run resolved %s/%s, want bedrock-claude/eu.anthropic.claude-haiku-5-5", d.Provider, d.Model)
			}
		})
	}
}

// The fallback closure stalls the pair it is leaving so the re-resolve walks
// past it. It must apply the same rule as the loop: a permanent request error
// leaves the shared model alone, a 5xx or a missing model (404) stalls it.
func TestFallbackForRun_StallsTheFailedModelOnlyOnModelFaults(t *testing.T) {
	for _, tc := range []struct {
		cause       string
		wantStalled bool
	}{
		{"anthropic 400: invalid_request_error", false},
		{"anthropic 500: upstream timeout", true},
		{"anthropic 404: model: claude-sonnet-4-6 not found", true},
	} {
		s := pinTierServer(t)
		s.cfg().UserTiers = map[string]config.UserTier{"default": {FallbackOnError: true}}
		storeActiveDef(t, s, "walker", "def_tier", `{"tier":"middle"}`)
		_, restore := captureLog(t)

		policy, reResolve := s.fallbackForRun("", "", "walker", "", false, nil)
		if !policy.Enabled || reResolve == nil {
			restore()
			t.Fatal("fallback policy not armed — fixture does not exercise the fallback path")
		}
		_, _, _, _ = reResolve(context.Background(), "anthropic", "claude-sonnet-4-6", errors.New(tc.cause))
		restore()

		got := s.resolver.Snapshot()["anthropic"].Models["claude-sonnet-4-6"].Stalled
		if got != tc.wantStalled {
			t.Errorf("%s: stalled = %v, want %v", tc.cause, got, tc.wantStalled)
		}
	}
}

// A 429 reaches the fallback closure (it is retryable). It must cool the model
// for the rate-limit window, as the loop's own 429 feedback does, never stall
// it until the next probe: a rate limit says "slow down", not "broken".
func TestFallbackForRun_RateLimitCoolsTheModelInsteadOfStalling(t *testing.T) {
	s := pinTierServer(t)
	s.cfg().UserTiers = map[string]config.UserTier{"default": {FallbackOnError: true}}
	storeActiveDef(t, s, "walker", "def_tier", `{"tier":"middle"}`)
	_, restore := captureLog(t)
	defer restore()

	_, reResolve := s.fallbackForRun("", "", "walker", "", false, nil)
	if reResolve == nil {
		t.Fatal("fallback policy not armed — fixture does not exercise the fallback path")
	}
	_, _, _, _ = reResolve(context.Background(), "anthropic", "claude-sonnet-4-6", errors.New("anthropic 429: rate_limit_error"))

	st := s.resolver.Snapshot()["anthropic"].Models["claude-sonnet-4-6"]
	if st.Stalled {
		t.Error("a 429 stalled the model until the next probe; want a rate-limit cooldown instead")
	}
	if !st.RateLimited {
		t.Error("a 429 left the model un-cooled; want it rate-limited so the re-resolve walks past it")
	}
}

// The operator's own key refused: the provider leaves routing at once, so the
// next run on the tier falls to the other provider (or, with none left, gets
// the unavailable error), and a re-probe is started so a working key brings it
// back. A tenant's key refused leaves the provider in routing.
func TestOperatorKeyRefusal_NextRunFallsToAnotherProviderAndAReprobeRuns(t *testing.T) {
	newResolver := func(cands []resolve.Candidate) (*resolve.Resolver, chan struct{}) {
		r := resolve.NewResolver([]string{"anthropic", "openai"}, map[string][]resolve.Candidate{"middle": cands})
		r.SetReachable("anthropic", true, []string{"claude-haiku-5-5"}, "")
		r.SetReachable("openai", true, []string{"gpt-5"}, "")
		probed := make(chan struct{}, 4)
		r.SetForceProbeCallback(func(context.Context) { probed <- struct{}{} })
		return r, probed
	}
	run := func(srv *Server, source string) {
		_, _ = loop.Run(context.Background(), loop.RunOptions{
			Provider:                &failingCallProvider{id: "anthropic", err: providers.WithKeySource(errors.New("anthropic 401: invalid x-api-key"), source)},
			Model:                   "claude-haiku-5-5",
			Segments:                []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
			MarkStalled:             srv.markStalledFn("anthropic", "claude-haiku-5-5"),
			MarkProviderUnreachable: srv.markProviderUnreachableFn(),
		})
	}
	both := []resolve.Candidate{{Provider: "anthropic", Model: "claude-haiku-5-5"}, {Provider: "openai", Model: "gpt-5"}}

	t.Run("operator key, another provider in the tier", func(t *testing.T) {
		r, probed := newResolver(both)
		srv := &Server{resolver: r, cfgHolder: config.NewHolder(&config.Config{})}
		run(srv, "operator")
		d, err := r.Resolve(resolve.AgentRequest{Name: "next", Tier: "middle"})
		if err != nil || d.Provider != "openai" {
			t.Fatalf("next run resolved (%s, %v), want openai", d.Provider, err)
		}
		select {
		case <-probed:
		case <-time.After(5 * time.Second):
			t.Fatal("no re-probe was started after the operator key was refused")
		}
		// A probe that finds the key working again brings the provider back.
		r.SetReachable("anthropic", true, []string{"claude-haiku-5-5"}, "")
		if d, err := r.Resolve(resolve.AgentRequest{Name: "next", Tier: "middle"}); err != nil || d.Provider != "anthropic" {
			t.Errorf("after a good probe the tier resolved (%s, %v), want anthropic back", d.Provider, err)
		}
	})

	t.Run("operator key, no other provider", func(t *testing.T) {
		r, _ := newResolver(both[:1])
		srv := &Server{resolver: r, cfgHolder: config.NewHolder(&config.Config{})}
		run(srv, "operator")
		if d, err := r.Resolve(resolve.AgentRequest{Name: "next", Tier: "middle"}); err == nil {
			t.Fatalf("next run resolved %s/%s, want the unavailable error", d.Provider, d.Model)
		}
	})

	t.Run("tenant key", func(t *testing.T) {
		r, probed := newResolver(both)
		srv := &Server{resolver: r, cfgHolder: config.NewHolder(&config.Config{})}
		run(srv, "tenant")
		d, err := r.Resolve(resolve.AgentRequest{Name: "next", Tier: "middle"})
		if err != nil || d.Provider != "anthropic" {
			t.Fatalf("after a tenant-key 401 the tier resolved (%s, %v), want anthropic still in routing", d.Provider, err)
		}
		select {
		case <-probed:
			t.Error("a tenant-key refusal started a re-probe")
		case <-time.After(200 * time.Millisecond):
		}
	})
}
