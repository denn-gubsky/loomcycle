package http

import (
	"context"
	"errors"
	"sync/atomic"
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

// probingProvider is a provider whose Probe — the request-independent check
// the periodic loop runs with the operator's key — answers probeErr. gate,
// when set, holds every Probe until it is closed.
type probingProvider struct {
	failingCallProvider
	probeErr error
	gate     chan struct{}
	probes   atomic.Int32
}

func (p *probingProvider) Probe(context.Context) error {
	p.probes.Add(1)
	if p.gate != nil {
		<-p.gate
	}
	return p.probeErr
}

// keyRefusalFixture is a bedrock-claude provider with two models, plus an
// openai provider, behind a real resolver and the server's real feedback.
func keyRefusalFixture(t *testing.T, bedrock *probingProvider) (*Server, *resolve.Resolver) {
	t.Helper()
	r := resolve.NewResolver([]string{"bedrock-claude", "openai"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "bedrock-claude", Model: "eu.anthropic.claude-sonnet-5-5"}, {Provider: "openai", Model: "gpt-5"}},
		"solo":   {{Provider: "bedrock-claude", Model: "eu.anthropic.claude-sonnet-5-5"}},
		"low":    {{Provider: "bedrock-claude", Model: "eu.anthropic.claude-haiku-5-5"}},
	})
	r.SetReachable("bedrock-claude", true, []string{"eu.anthropic.claude-sonnet-5-5", "eu.anthropic.claude-haiku-5-5"}, "")
	r.SetReachable("openai", true, []string{"gpt-5"}, "")
	srv := &Server{
		resolver:  r,
		cfgHolder: config.NewHolder(&config.Config{}),
		providers: &routingResolver{known: map[string]providers.Provider{"bedrock-claude": bedrock}},
	}
	return srv, r
}

func runRefused(srv *Server, prov providers.Provider, model string) {
	_, _ = loop.Run(context.Background(), loop.RunOptions{
		Provider:                 prov,
		Model:                    model,
		Segments:                 []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "x"}}}},
		MarkStalled:              srv.markStalledFn("bedrock-claude", model),
		ReportOperatorKeyRefusal: srv.operatorKeyRefusalFn(),
	})
}

// waitKeyProbeDone waits until no confirming probe is in flight for p.
func waitKeyProbeDone(t *testing.T, srv *Server, p string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, busy := srv.keyProbes.Load(p); !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the confirming key probe for %s did not finish", p)
}

// A 403 for one model — no access to it, no Marketplace subscription — is not
// about the key: Haiku on the same credentials still works. The shared
// provider must stay in routing for every tenant, and no probe is needed.
func TestOperatorKey403ForOneModel_LeavesTheProviderInRouting(t *testing.T) {
	for _, body := range []string{
		`{"message":"AccessDeniedException: You don't have access to the model with the specified model ID."}`,
		`{"message":"Model access is denied due to IAM user or service role is not authorized to perform the required AWS Marketplace actions (aws-marketplace:ViewSubscriptions, aws-marketplace:Subscribe)"}`,
	} {
		bedrock := &probingProvider{failingCallProvider: failingCallProvider{id: "bedrock-claude",
			err: providers.WithKeySource(errors.New("anthropic 403: "+body), "operator")}}
		srv, r := keyRefusalFixture(t, bedrock)
		runRefused(srv, bedrock, "eu.anthropic.claude-sonnet-5-5")
		waitKeyProbeDone(t, srv, "bedrock-claude")

		if d, err := r.Resolve(resolve.AgentRequest{Name: "next", Tier: "low"}); err != nil || d.Model != "eu.anthropic.claude-haiku-5-5" {
			t.Errorf("after a model 403 another model of the provider resolved (%s/%s, %v), want bedrock-claude haiku", d.Provider, d.Model, err)
		}
		if !r.Snapshot()["bedrock-claude"].Reachable {
			t.Errorf("a model 403 (%s) took the whole provider out of routing", body)
		}
		if n := bedrock.probes.Load(); n != 0 {
			t.Errorf("a model 403 started %d key probes, want 0", n)
		}
	}
}

// A 401 on the operator key is only a suspicion until the request-independent
// probe agrees. When the probe succeeds the key works: nothing shared changes.
func TestOperatorKey401WithAPassingProbe_LeavesTheProviderInRouting(t *testing.T) {
	bedrock := &probingProvider{failingCallProvider: failingCallProvider{id: "bedrock-claude",
		err: providers.WithKeySource(errors.New(`anthropic 401: {"type":"authentication_error"}`), "operator")}}
	srv, r := keyRefusalFixture(t, bedrock)
	runRefused(srv, bedrock, "eu.anthropic.claude-sonnet-5-5")
	waitKeyProbeDone(t, srv, "bedrock-claude")

	if n := bedrock.probes.Load(); n != 1 {
		t.Fatalf("key probes = %d, want 1 confirming probe", n)
	}
	if !r.Snapshot()["bedrock-claude"].Reachable {
		t.Error("a 401 the probe did not confirm took the provider out of routing")
	}
}

// Confirmed: the probe is refused the key too. The provider leaves routing, so
// the next run on a tier with another provider falls to it, and a tier with
// none gets the unavailable error.
func TestOperatorKey401ConfirmedByTheProbe_TakesTheProviderOut(t *testing.T) {
	bedrock := &probingProvider{
		failingCallProvider: failingCallProvider{id: "bedrock-claude",
			err: providers.WithKeySource(errors.New(`anthropic 401: {"type":"authentication_error"}`), "operator")},
		probeErr: errors.New(`bedrock model listing: /inference-profiles: status 403 ({"message":"The security token included in the request is invalid."})`),
	}
	srv, r := keyRefusalFixture(t, bedrock)
	runRefused(srv, bedrock, "eu.anthropic.claude-sonnet-5-5")
	waitKeyProbeDone(t, srv, "bedrock-claude")

	if d, err := r.Resolve(resolve.AgentRequest{Name: "next", Tier: "middle"}); err != nil || d.Provider != "openai" {
		t.Errorf("next run on the tier resolved (%s, %v), want openai", d.Provider, err)
	}
	if d, err := r.Resolve(resolve.AgentRequest{Name: "next", Tier: "solo"}); err == nil {
		t.Errorf("next run on a bedrock-only tier resolved %s/%s, want the unavailable error", d.Provider, d.Model)
	}
}

// A burst of runs hitting the same refused key probes once.
func TestOperatorKey401Burst_ProbesOnce(t *testing.T) {
	gate := make(chan struct{})
	bedrock := &probingProvider{
		failingCallProvider: failingCallProvider{id: "bedrock-claude",
			err: providers.WithKeySource(errors.New(`anthropic 401: {"type":"authentication_error"}`), "operator")},
		gate: gate,
	}
	srv, _ := keyRefusalFixture(t, bedrock)
	for i := 0; i < 8; i++ {
		runRefused(srv, bedrock, "eu.anthropic.claude-sonnet-5-5")
	}
	close(gate)
	waitKeyProbeDone(t, srv, "bedrock-claude")
	if n := bedrock.probes.Load(); n != 1 {
		t.Errorf("8 refusals started %d key probes, want 1", n)
	}
}

// A tenant's own key refused touches nothing shared and probes nothing.
func TestTenantKey401_LeavesTheProviderInRouting(t *testing.T) {
	bedrock := &probingProvider{
		failingCallProvider: failingCallProvider{id: "bedrock-claude",
			err: providers.WithKeySource(errors.New(`anthropic 401: {"type":"authentication_error"}`), "tenant")},
		probeErr: errors.New("anthropic /v1/models: status 401 (authentication_error)"),
	}
	srv, r := keyRefusalFixture(t, bedrock)
	runRefused(srv, bedrock, "eu.anthropic.claude-sonnet-5-5")
	waitKeyProbeDone(t, srv, "bedrock-claude")
	if !r.Snapshot()["bedrock-claude"].Reachable || bedrock.probes.Load() != 0 {
		t.Errorf("tenant-key 401: reachable=%v probes=%d, want true and 0",
			r.Snapshot()["bedrock-claude"].Reachable, bedrock.probes.Load())
	}
}
