package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// widenedFixture returns a Server with the given static host allowlist and
// private-host exemption, plus an in-memory store holding one user-scope key.
//
// The two lists are SEPARATE parameters on purpose. They are the two layers the
// HTTP tool applies — the NAME allowlist and the private-IP exemption — and a
// test that sets them together cannot tell which one refused a loopback URL. A
// 5d test that thinks it is checking the name layer while the IP guard is doing
// the work proves nothing about 5d.
func widenedFixture(t *testing.T, allow, private []string) (*Server, memInject) {
	t.Helper()
	st, _ := memStoreFixture(t)
	s := &Server{store: st, cfgHolder: config.NewHolder(&config.Config{
		Env: config.Env{
			HTTPHostAllowlist:        allow,
			HTTPPrivateHostAllowlist: private,
		},
	})}
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "a"}
	if err := st.MemorySet(context.Background(), "t1", store.MemoryScopeUser, "u1",
		"launch", json.RawMessage(`"ship on friday"`), 0); err != nil {
		t.Fatalf("MemorySet: %v", err)
	}
	return s, mi
}

// TestWidenedInjection_AuthorshipDecidesAndTheFastPathDoesNotHideIt is the
// end-to-end shape of the guard on the real assembly path.
//
// Two things must both hold, and the second is the one a pure-expander test
// cannot see: an agent-authored def must not get the widened family, AND its
// placeholder must be REMOVED rather than left sitting in the prompt as literal
// text. The fast path is what would leave it there — a prompt whose only
// placeholder is a widened one has no other reason to enter expansion.
func TestWidenedInjection_AuthorshipDecidesAndTheFastPathDoesNotHideIt(t *testing.T) {
	s, mi := widenedFixture(t, nil, nil)

	operator := config.AgentDef{SystemPrompt: "Plan: {{memory:key:launch}}", OperatorAuthored: true}
	got, _ := s.applyMemoryInjection(context.Background(), operator, mi)
	if !strings.Contains(got.SystemPrompt, "ship on friday") {
		t.Fatalf("an operator-authored def did not get the widened memory family:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, `<memory key="launch">`) {
		t.Errorf("the body was not DATA-framed with its source:\n%s", got.SystemPrompt)
	}

	agent := config.AgentDef{SystemPrompt: "Plan: {{memory:key:launch}}", OperatorAuthored: false}
	got, _ = s.applyMemoryInjection(context.Background(), agent, mi)
	if strings.Contains(got.SystemPrompt, "ship on friday") {
		t.Fatalf("an agent-authored def used the widened memory family:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "{{memory:key:launch}}") {
		t.Errorf("the refused placeholder was left LITERAL in the prompt — the fast path "+
			"skipped expansion for a prompt that had a widened reference:\n%s", got.SystemPrompt)
	}
}

// TestWidenedInjection_PreExistingFamiliesAreUntouchedByTheGuard: a legacy row
// reads as not-operator-authored because it predates the column. Gating what it
// already used would strip expansion from every working def on upgrade.
func TestWidenedInjection_PreExistingFamiliesAreUntouchedByTheGuard(t *testing.T) {
	s, mi := widenedFixture(t, nil, nil)
	legacy := config.AgentDef{
		SystemPrompt: "{{memory:consolidation_bands}}\n{{document:/specs/launch}}",
		// OperatorAuthored deliberately unset — the legacy-row value.
	}
	got, _ := s.applyMemoryInjection(context.Background(), legacy, mi)
	if !strings.Contains(got.SystemPrompt, "Duplicate-detection similarity bands") {
		t.Errorf("the variant family stopped working for a legacy row:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "Document op=export_md path=/specs/launch") {
		t.Errorf("the document family stopped working for a legacy row:\n%s", got.SystemPrompt)
	}
}

// TestWidenedFetch_RendersAnAllowlistedHostAndRefusesEveryOther exercises trust
// rule 5d against a REAL fetch: the expander's refusal is what the operator
// sees, and the tool being built against the static list is why the request is
// never made. Both have to hold, so both are checked here rather than only the
// string outcome.
func TestWidenedFetch_RendersAnAllowlistedHostAndRefusesEveryOther(t *testing.T) {
	hit := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		_, _ = w.Write([]byte("THE FETCHED PAGE"))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	s, mi := widenedFixture(t, []string{u.Hostname()}, []string{u.Hostname()})
	def := config.AgentDef{
		SystemPrompt:     "Context: {{tool:WebFetch:" + srv.URL + "/page}}",
		OperatorAuthored: true,
	}
	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if !strings.Contains(got.SystemPrompt, "THE FETCHED PAGE") {
		t.Fatalf("an allowlisted host did not render (hits=%d):\n%s", hit, got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, `<tool-result tool="WebFetch"`) {
		t.Errorf("the fetched body was not DATA-framed:\n%s", got.SystemPrompt)
	}

	// Same live server, still reachable — the private-IP guard is lifted for
	// loopback — but the operator does not LIST its host. So the only thing that
	// can stop the request is the name allowlist, which is what 5d is about.
	before := hit
	s2, mi2 := widenedFixture(t, []string{"docs.example.com"}, []string{u.Hostname()})
	got2, _ := s2.applyMemoryInjection(context.Background(), def, mi2)
	if strings.Contains(got2.SystemPrompt, "THE FETCHED PAGE") {
		t.Fatalf("an off-allowlist host rendered:\n%s", got2.SystemPrompt)
	}
	if hit != before {
		t.Errorf("the request was MADE to an off-allowlist host (%d → %d) — the refusal has to "+
			"stop the dial, not just the rendering", before, hit)
	}
}

// TestWidenedFetch_FailsSoftWhenTheHostIsUnreachable is the cost the network
// family was allowed on the condition it be paid here. Prompt assembly runs at
// every run entry, sub-agent spawn and resume; a page being down renders
// nothing and the run proceeds.
func TestWidenedFetch_FailsSoftWhenTheHostIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("GONE"))
	}))
	u, _ := url.Parse(srv.URL)
	srv.Close() // nothing is listening now

	s, mi := widenedFixture(t, []string{u.Hostname()}, []string{u.Hostname()})
	def := config.AgentDef{
		SystemPrompt:     "before {{tool:WebFetch:" + u.String() + "/page}} after",
		OperatorAuthored: true,
	}
	got, _ := s.applyMemoryInjection(context.Background(), def, mi)
	if !strings.Contains(got.SystemPrompt, "before") || !strings.Contains(got.SystemPrompt, "after") {
		t.Fatalf("the surrounding prompt was damaged by an unreachable host:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "{{") {
		t.Errorf("an unresolved placeholder was left in the prompt:\n%s", got.SystemPrompt)
	}
}

// TestStaticHostAllowed_IsTheOperatorFloor pins what the 5d predicate reads.
// Not the run's caller-authoritative list — that can be widened per call, and
// this fetch happens before any of it is in force.
func TestStaticHostAllowed_IsTheOperatorFloor(t *testing.T) {
	s := &Server{cfgHolder: config.NewHolder(&config.Config{
		Env: config.Env{HTTPHostAllowlist: []string{"example.com"}},
	})}
	if !s.staticHostAllowed("example.com") || !s.staticHostAllowed("api.example.com") {
		t.Errorf("a listed host and its subdomain must be allowed")
	}
	if s.staticHostAllowed("evilexample.com") {
		t.Errorf("suffix matching must be anchored on a dot boundary")
	}
	if s.staticHostAllowed("localhost") {
		t.Errorf("localhost is not on the list and must not be allowed")
	}

	// Localhost is stripped from the floor unless the operator opted it in —
	// the same treatment the runtime's own HTTP tool gets.
	stripped := &Server{cfgHolder: config.NewHolder(&config.Config{
		Env: config.Env{HTTPHostAllowlist: []string{"localhost"}},
	})}
	if stripped.staticHostAllowed("localhost") {
		t.Errorf("a bare localhost entry must be stripped, as it is for the HTTP tool")
	}
	optedIn := &Server{cfgHolder: config.NewHolder(&config.Config{
		Env: config.Env{
			HTTPHostAllowlist:        []string{"localhost"},
			HTTPPrivateHostAllowlist: []string{"localhost"},
		},
	})}
	if !optedIn.staticHostAllowed("localhost") {
		t.Errorf("an explicit private-host opt-in must survive the strip")
	}

	// No config at all must not be permissive.
	if (&Server{}).staticHostAllowed("example.com") {
		t.Errorf("a server with no config allowed a host — an unwired server must fail closed")
	}
}

// TestCallerSegments_RefuseTheWidenedFamilies: a team node's prompt is authored
// by the TeamDef, not by the agent being spawned, and TeamDef carries no
// authorship marker yet. Until it does the widened families are unavailable
// there — and the check that matters is that the seam fails CLOSED, not that it
// happens to be unwired.
func TestCallerSegments_RefuseTheWidenedFamilies(t *testing.T) {
	s, mi := widenedFixture(t, []string{"docs.example.com"}, nil)

	// Observed through the refusal LOG, not through the rendered output.
	// Rendering nothing would prove nothing here: a caller segment supplies no
	// bodies for either widened family, so both render empty whatever the flag
	// says. The refusal is the only signal that the GUARD is what stopped them,
	// and it is the thing that would change if the flag were flipped.
	var captured strings.Builder
	prev := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(prev) })

	system, user := s.expandCallerSegments(context.Background(), mi, map[string]string{"var.x": "launch"},
		"role: {{memory:key:launch}}", "task: {{tool:WebFetch:https://docs.example.com/a}}")
	if strings.Contains(system, "ship on friday") {
		t.Errorf("a caller segment used the widened memory family:\n%s", system)
	}
	if strings.Contains(user, "<tool-result") {
		t.Errorf("a caller segment used the widened network family:\n%s", user)
	}
	for _, out := range []string{system, user} {
		if strings.Contains(out, "{{") {
			t.Errorf("a refused placeholder was left literal:\n%s", out)
		}
	}
	logged := captured.String()
	if !strings.Contains(logged, "memory:key (def is not operator-authored)") {
		t.Errorf("the memory sub-form was not refused BY THE GUARD in a caller segment: %q", logged)
	}
	if !strings.Contains(logged, "tool:WebFetch (def is not operator-authored)") {
		t.Errorf("the network form was not refused BY THE GUARD in a caller segment: %q", logged)
	}
}
