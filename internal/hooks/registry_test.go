package hooks

import (
	"errors"
	"testing"
)

func mustRegister(t *testing.T, r *Registry, h *Hook) string {
	t.Helper()
	id, err := r.Register(h)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return id
}

func TestRegistry_RegisterAndDelete(t *testing.T) {
	r := NewRegistry()
	id := mustRegister(t, r, &Hook{
		Owner: "app1", Name: "h1", Phase: PhasePre,
		CallbackURL: "https://example/x",
	})
	if id == "" {
		t.Fatal("Register returned empty id")
	}
	if got := r.List(); len(got) != 1 || got[0].ID != id {
		t.Errorf("List = %v, want one entry with id %s", got, id)
	}
	if err := r.Delete(id); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("after delete List = %v, want empty", got)
	}
	if err := r.Delete(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete err = %v, want ErrNotFound", err)
	}
}

// TestRegistry_ReplaceOnOwnerName_NameKey pins the headline contract:
// re-registering the same (owner, name) MUST replace the prior entry,
// not stack a duplicate. Cascading-on-app-restart is the exact failure
// mode this prevents.
func TestRegistry_ReplaceOnOwnerName_NameKey(t *testing.T) {
	r := NewRegistry()
	first := mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", Name: "scan", Phase: PhasePost,
		CallbackURL: "https://a/v1", Tools: []string{"WebFetch"},
	})
	second := mustRegister(t, r, &Hook{
		Owner: "jobs-search-web", Name: "scan", Phase: PhasePost,
		CallbackURL: "https://b/v2", Tools: []string{"WebFetch"},
	})

	if first == second {
		t.Errorf("re-register returned same id %s; expected fresh id on replace", first)
	}
	got := r.List()
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1 (re-register must replace, not append)", len(got))
	}
	if got[0].ID != second {
		t.Errorf("List[0].ID = %q, want %q (latest registration)", got[0].ID, second)
	}
	if got[0].CallbackURL != "https://b/v2" {
		t.Errorf("CallbackURL = %q, want b/v2 (replacement should carry the new callback)", got[0].CallbackURL)
	}
}

// TestRegistry_ReplacePreservesChainPosition pins that re-registration
// keeps the slot in `order` so chain ordering doesn't shuffle when an
// app restarts. Hooks A → B → C, then re-register B; chain must
// remain A → B → C, not A → C → B.
func TestRegistry_ReplacePreservesChainPosition(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "A", Phase: PhasePre, CallbackURL: "https://x/a", Tools: []string{"T"}})
	mustRegister(t, r, &Hook{Owner: "x", Name: "B", Phase: PhasePre, CallbackURL: "https://x/b", Tools: []string{"T"}})
	mustRegister(t, r, &Hook{Owner: "x", Name: "C", Phase: PhasePre, CallbackURL: "https://x/c", Tools: []string{"T"}})

	mustRegister(t, r, &Hook{Owner: "x", Name: "B", Phase: PhasePre, CallbackURL: "https://x/b2", Tools: []string{"T"}})

	got := r.Match("agent", "T", PhasePre)
	if len(got) != 3 {
		t.Fatalf("Match returned %d hooks, want 3", len(got))
	}
	want := []string{"A", "B", "C"}
	for i, h := range got {
		if h.Name != want[i] {
			t.Errorf("Match[%d].Name = %q, want %q (re-register reshuffled chain order)", i, h.Name, want[i])
		}
	}
	// And B's callback IS the new one.
	if got[1].CallbackURL != "https://x/b2" {
		t.Errorf("B CallbackURL = %q, want b2 after re-register", got[1].CallbackURL)
	}
}

// TestRegistry_MatchAgentTool covers the selector matrix. Empty list =
// "*", explicit lists are exact-match, prefix-glob with trailing-* is
// supported.
func TestRegistry_MatchAgentTool(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "all-agents-webfetch", Phase: PhasePost,
		CallbackURL: "https://x/1", Tools: []string{"WebFetch"},
	})
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "qa-only-everything", Phase: PhasePost,
		CallbackURL: "https://x/2", Agents: []string{"qa-agent"},
	})
	mustRegister(t, r, &Hook{
		Owner: "x", Name: "mcp-glob", Phase: PhasePost,
		CallbackURL: "https://x/3", Tools: []string{"mcp__jobs__*"},
	})

	cases := []struct {
		agent    string
		tool     string
		wantCnt  int
		wantHits []string
	}{
		{"qa-agent", "WebFetch", 2, []string{"all-agents-webfetch", "qa-only-everything"}},
		{"qa-agent", "Read", 1, []string{"qa-only-everything"}},
		{"company-researcher", "WebFetch", 1, []string{"all-agents-webfetch"}},
		{"company-researcher", "mcp__jobs__getResearch", 1, []string{"mcp-glob"}},
		{"company-researcher", "mcp__other__do", 0, nil},
	}
	for _, tc := range cases {
		got := r.Match(tc.agent, tc.tool, PhasePost)
		if len(got) != tc.wantCnt {
			t.Errorf("Match(%q,%q): %d hits, want %d (got=%v)", tc.agent, tc.tool, len(got), tc.wantCnt, hookNames(got))
			continue
		}
		// Match returns Post in REVERSE registration order; for assertion
		// readability flip back to registration order.
		gotNames := hookNames(got)
		// Reverse to compare to registration-order want list.
		for i, j := 0, len(gotNames)-1; i < j; i, j = i+1, j-1 {
			gotNames[i], gotNames[j] = gotNames[j], gotNames[i]
		}
		for i, name := range gotNames {
			if name != tc.wantHits[i] {
				t.Errorf("Match(%q,%q)[%d]=%q, want %q", tc.agent, tc.tool, i, name, tc.wantHits[i])
			}
		}
	}
}

// TestRegistry_MatchPostIsLIFO pins the middleware ordering: hooks
// registered A → B → C should run as Pre A → B → C, but Post C → B → A.
func TestRegistry_MatchPostIsLIFO(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, &Hook{Owner: "x", Name: "A", Phase: PhasePost, CallbackURL: "https://x/a", Tools: []string{"T"}})
	mustRegister(t, r, &Hook{Owner: "x", Name: "B", Phase: PhasePost, CallbackURL: "https://x/b", Tools: []string{"T"}})
	mustRegister(t, r, &Hook{Owner: "x", Name: "C", Phase: PhasePost, CallbackURL: "https://x/c", Tools: []string{"T"}})

	got := hookNames(r.Match("agent", "T", PhasePost))
	want := []string{"C", "B", "A"}
	if len(got) != len(want) {
		t.Fatalf("Match Post returned %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Post chain[%d] = %q, want %q (LIFO middleware order)", i, got[i], want[i])
		}
	}
}

func TestRegistry_Validate(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name string
		h    *Hook
	}{
		{"missing owner", &Hook{Name: "x", Phase: PhasePre, CallbackURL: "https://e/x"}},
		{"missing name", &Hook{Owner: "x", Phase: PhasePre, CallbackURL: "https://e/x"}},
		{"bad phase", &Hook{Owner: "x", Name: "x", Phase: "during", CallbackURL: "https://e/x"}},
		{"missing callback", &Hook{Owner: "x", Name: "x", Phase: PhasePre}},
		{"bad scheme", &Hook{Owner: "x", Name: "x", Phase: PhasePre, CallbackURL: "ftp://e/x"}},
		{"bad fail mode", &Hook{Owner: "x", Name: "x", Phase: PhasePre, CallbackURL: "https://e/x", FailMode: "explosive"}},
		{"negative timeout", &Hook{Owner: "x", Name: "x", Phase: PhasePre, CallbackURL: "https://e/x", TimeoutMs: -1}},
	}
	for _, tc := range cases {
		if _, err := r.Register(tc.h); !errors.Is(err, ErrInvalidRegistration) {
			t.Errorf("%s: err=%v, want ErrInvalidRegistration", tc.name, err)
		}
	}
}

// TestRegistry_ResolveDefaults pins that registration fills in the
// FailMode default ("open") and TimeoutMs default (5s) so the hot
// path can trust well-formed values.
func TestRegistry_ResolveDefaults(t *testing.T) {
	r := NewRegistry()
	id := mustRegister(t, r, &Hook{
		Owner: "x", Name: "y", Phase: PhasePre, CallbackURL: "https://e/x",
	})
	got := r.List()
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("registry state unexpected: %v", got)
	}
	if got[0].FailMode != FailOpen {
		t.Errorf("FailMode = %q, want %q (default)", got[0].FailMode, FailOpen)
	}
	if got[0].Timeout == 0 {
		t.Errorf("Timeout = 0, want non-zero default")
	}
}

func hookNames(hs []*Hook) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name
	}
	return out
}

// TestRegistry_HostWidenPermit_ExactMatch pins the trust-boundary
// contract: only exact-match owner UIDs in the operator yaml are
// permitted to widen the host allowlist via Pre-hook allow_hosts.
// Globs / prefix-matches must NOT match — the operator names each
// app explicitly. Whitespace is trimmed so a yaml entry with
// surrounding spaces still matches.
func TestRegistry_HostWidenPermit_ExactMatch(t *testing.T) {
	r := NewRegistryWithPermissions([]string{
		"jobs-search-web",
		"  company-research  ", // surrounding whitespace tolerated
		"",                     // empty entry silently dropped
	})

	cases := []struct {
		owner string
		want  bool
	}{
		{"jobs-search-web", true},          // exact match
		{"company-research", true},         // trimmed match
		{"jobs-search-web-staging", false}, // prefix is NOT a match (no globs)
		{"jobs-search", false},             // partial prefix not a match
		{"web", false},                     // suffix not a match
		{"", false},                        // empty owner never matches
		{"unknown", false},                 // not in list
		{"  jobs-search-web  ", false},     // lookup doesn't trim — operator names canonicalise on registration
	}
	for _, tc := range cases {
		t.Run(tc.owner, func(t *testing.T) {
			got := r.IsHostWidenPermitted(tc.owner)
			if got != tc.want {
				t.Errorf("IsHostWidenPermitted(%q) = %v, want %v", tc.owner, got, tc.want)
			}
		})
	}
}

// TestRegistry_HostWidenPermit_DefaultDeny verifies the default
// stance: a registry constructed without a permit list (NewRegistry)
// returns false for every owner — preserving the "operator yaml is
// the trust-boundary floor" invariant for back-compat installs that
// don't opt in.
func TestRegistry_HostWidenPermit_DefaultDeny(t *testing.T) {
	r := NewRegistry()
	if r.IsHostWidenPermitted("any-owner") {
		t.Error("NewRegistry() should default-deny host widening for every owner")
	}
	if r.IsHostWidenPermitted("") {
		t.Error("NewRegistry() must default-deny the empty-string owner")
	}
}

// TestRegistry_HostWidenPermit_NilReceiver pins defensive behavior:
// a nil *Registry must return false rather than panic. The dispatcher
// is wired with a non-nil registry by Server.New, but defensive
// programming on a hot-path check costs nothing and removes a
// latent crash class.
func TestRegistry_HostWidenPermit_NilReceiver(t *testing.T) {
	var r *Registry
	if r.IsHostWidenPermitted("anyone") {
		t.Error("nil receiver should return false")
	}
}
