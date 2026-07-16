package resolve

import (
	"errors"
	"testing"
)

// RFC BG model-pattern resolution. These lock the resolver's expansion of a
// `model_pattern` glob against the live catalog: it must pick the NEWEST listed
// model (numeric-run ranking via internal/modelver), honour the Anthropic
// bare-rolling-alias convention, fall through on a no-match rather than
// hard-fail, and surface a silent model bump on the log.

// patternReq builds a tier request whose single "low" candidate is a pattern on
// the given provider — the minimal shape that exercises the tier path without
// leaning on the library fixture.
func patternReq(name, provider, glob string) AgentRequest {
	return AgentRequest{
		Name:      name,
		Tier:      "low",
		Providers: []string{provider},
		Models:    map[string][]Candidate{"low": {{Provider: provider, ModelPattern: glob}}},
	}
}

func TestResolve_PatternCandidateResolvesToNewestListed(t *testing.T) {
	r := NewResolver([]string{"anthropic"}, nil)
	// Only a dated snapshot is listed — the operator's config had the bare
	// `claude-haiku-4-5`, which would 404; the pattern tracks the real catalog.
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5-20251001"}, "")

	dec, err := r.Resolve(patternReq("doc-agent", "anthropic", "claude-haiku-*"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("resolved model = %q, want claude-haiku-4-5-20251001", dec.Model)
	}
}

func TestResolve_PatternPicksHighestVersionAcrossGenerations(t *testing.T) {
	r := NewResolver([]string{"anthropic"}, nil)
	// The bare major claude-sonnet-5 ([5]) outranks the older minors regardless
	// of bareIsNewer — [5] > [4,6] > [4,5,…] at the first differing component.
	r.SetReachable("anthropic", true, []string{
		"claude-sonnet-5",
		"claude-sonnet-4-6",
		"claude-sonnet-4-5-20250929",
	}, "")

	dec, err := r.Resolve(patternReq("chat", "anthropic", "claude-sonnet-*"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Model != "claude-sonnet-5" {
		t.Errorf("resolved model = %q, want claude-sonnet-5", dec.Model)
	}
}

func TestResolve_PatternAnthropicBareMajorOutranksDatedSnapshot(t *testing.T) {
	// The genuine bare-vs-dated tie-break: [5] vs [5,20260101]. Anthropic
	// publishes the bare major as a rolling alias for the newest snapshot, so
	// bareIsNewer=true (provider=="anthropic") picks the bare stem.
	r := NewResolver([]string{"anthropic"}, nil)
	r.SetReachable("anthropic", true, []string{
		"claude-sonnet-5",
		"claude-sonnet-5-20260101",
	}, "")

	dec, err := r.Resolve(patternReq("chat", "anthropic", "claude-sonnet-*"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Model != "claude-sonnet-5" {
		t.Errorf("resolved model = %q, want claude-sonnet-5 (bare rolling alias)", dec.Model)
	}
}

func TestResolve_PatternNonAnthropicPrefersDatedSnapshot(t *testing.T) {
	// Same bare-vs-dated shape on a NON-anthropic provider: bareIsNewer=false, so
	// the concrete dated snapshot ([5,4,20260101]) outranks the bare stem
	// ([5,4]). Proves the hardcoded provider=="anthropic" discriminator.
	r := NewResolver([]string{"openai"}, nil)
	r.SetReachable("openai", true, []string{
		"gpt-5.4",
		"gpt-5.4-20260101",
	}, "")

	dec, err := r.Resolve(patternReq("chat", "openai", "gpt-5.4*"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Model != "gpt-5.4-20260101" {
		t.Errorf("resolved model = %q, want gpt-5.4-20260101 (dated wins for non-anthropic)", dec.Model)
	}
}

func TestResolve_PatternNoMatchFallsThroughToNextCandidate(t *testing.T) {
	// A pattern that matches nothing available is an unavailable candidate — the
	// resolver walks on to the next candidate rather than hard-failing (§4.2.5).
	r := NewResolver([]string{"anthropic", "deepseek"}, nil)
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5-20251001"}, "")
	r.SetReachable("deepseek", true, []string{"deepseek-v4-flash"}, "")

	dec, err := r.Resolve(AgentRequest{
		Name:      "doc-agent",
		Tier:      "low",
		Providers: []string{"anthropic", "deepseek"},
		Models: map[string][]Candidate{"low": {
			{Provider: "anthropic", ModelPattern: "claude-nonexistent-*"},
			{Provider: "deepseek", Model: "deepseek-v4-flash"},
		}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "deepseek" || dec.Model != "deepseek-v4-flash" {
		t.Errorf("decision = %+v, want deepseek/deepseek-v4-flash", dec)
	}
}

func TestResolve_PatternNoMatchOnlyCandidateIsTierUnavailable(t *testing.T) {
	// A pattern miss with no fallback candidate is a plain availability outage,
	// not a policy refusal — ErrTierUnavailable so a client may retry.
	r := NewResolver([]string{"anthropic"}, nil)
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5-20251001"}, "")

	_, err := r.Resolve(patternReq("doc-agent", "anthropic", "claude-nonexistent-*"))
	if !errors.Is(err, ErrTierUnavailable) {
		t.Fatalf("err = %v, want ErrTierUnavailable", err)
	}
}

func TestResolve_PatternMoveLogsWarn(t *testing.T) {
	r := NewResolver([]string{"anthropic"}, nil)
	var logs []string
	r.SetLogf(func(format string, args ...any) {
		logs = append(logs, format)
	})
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5-20251001"}, "")

	// First resolution records the concrete model — nothing to compare against,
	// so it must NOT warn.
	if _, err := r.Resolve(patternReq("doc-agent", "anthropic", "claude-haiku-*")); err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("first-seen resolution logged %d line(s), want 0", len(logs))
	}

	// A newer snapshot appears in the catalog — the pattern now resolves to a
	// DIFFERENT concrete model, which must WARN so the operator sees the bump.
	r.SetReachable("anthropic", true, []string{
		"claude-haiku-4-5-20251001",
		"claude-haiku-5-20260101",
	}, "")
	dec, err := r.Resolve(patternReq("doc-agent", "anthropic", "claude-haiku-*"))
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if dec.Model != "claude-haiku-5-20260101" {
		t.Errorf("resolved model = %q, want claude-haiku-5-20260101", dec.Model)
	}
	if len(logs) != 1 {
		t.Fatalf("moved resolution logged %d line(s), want 1", len(logs))
	}
}

func TestResolvePin_PatternResolvesToNewestListed(t *testing.T) {
	// A pinned agent naming a model_pattern alias: the resolver expands the glob
	// on the pinned provider's catalog, once, at admission.
	r := NewResolver(nil, nil)
	r.SetReachable("anthropic", true, []string{
		"claude-haiku-4-5-20251001",
		"claude-haiku-5-20260101",
	}, "")

	dec, err := r.Resolve(AgentRequest{
		Name:            "pinned",
		PinProvider:     "anthropic",
		PinModelPattern: "claude-haiku-*",
		Effort:          "high",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Provider != "anthropic" || dec.Model != "claude-haiku-5-20260101" || dec.Effort != "high" {
		t.Errorf("decision = %+v, want anthropic/claude-haiku-5-20260101/high", dec)
	}
}

func TestResolvePin_PatternNoMatchIsPinUnavailable(t *testing.T) {
	// A pin has no fallthrough, so a pattern that matches nothing available fails
	// with ErrPinUnavailable — same shape as a stalled concrete pin.
	r := NewResolver(nil, nil)
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5-20251001"}, "")

	_, err := r.Resolve(AgentRequest{
		Name:            "pinned",
		PinProvider:     "anthropic",
		PinModelPattern: "claude-nonexistent-*",
	})
	if !errors.Is(err, ErrPinUnavailable) {
		t.Fatalf("err = %v, want ErrPinUnavailable", err)
	}
}

func TestCascade_ExpandsPatternToConcreteModel(t *testing.T) {
	// The routing view calls Cascade — a pattern candidate must show the concrete
	// model it would resolve to now, so the availability dot is meaningful.
	r := NewResolver([]string{"anthropic"}, nil)
	r.SetReachable("anthropic", true, []string{
		"claude-sonnet-5",
		"claude-sonnet-4-6",
	}, "")

	casc := r.Cascade(patternReq("routing-view", "anthropic", "claude-sonnet-*"))
	if len(casc) != 1 {
		t.Fatalf("cascade len = %d, want 1", len(casc))
	}
	if casc[0].Model != "claude-sonnet-5" {
		t.Errorf("cascade model = %q, want claude-sonnet-5", casc[0].Model)
	}
}

func TestCascade_UnresolvablePatternKeepsGlobForDisplay(t *testing.T) {
	// When the pattern resolves to nothing (empty/renamed catalog), Cascade keeps
	// the raw glob in Model so the routing view shows the pattern with an
	// unavailable dot rather than an empty cell.
	r := NewResolver([]string{"anthropic"}, nil)
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5-20251001"}, "")

	casc := r.Cascade(patternReq("routing-view", "anthropic", "claude-nonexistent-*"))
	if len(casc) != 1 {
		t.Fatalf("cascade len = %d, want 1", len(casc))
	}
	if casc[0].Model != "claude-nonexistent-*" {
		t.Errorf("cascade model = %q, want the raw glob claude-nonexistent-*", casc[0].Model)
	}
}
