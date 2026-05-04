package builtin

import (
	"reflect"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// nil callerAllowed → no wrapping; the same instances pass through.
// Critical for the default code path where the request omits
// allowed_hosts entirely.
func TestNarrowHostsNilPassThrough(t *testing.T) {
	original := []tools.Tool{
		&HTTP{HostAllowlist: []string{"a.example"}},
		&Read{Root: "/x"},
	}
	out := NarrowHosts(original, nil, "", false)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	// Same pointers.
	if out[0] != original[0] || out[1] != original[1] {
		t.Errorf("nil callerAllowed should pass tools through by reference")
	}
}

// Intersect-only invariant: caller asks for hosts not in the operator
// list — those entries are silently dropped from the effective list.
// Caller can SHRINK; never widen.
func TestNarrowHostsCannotWidenOperatorList(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"allowed.example"}}
	caller := []string{"allowed.example", "EVIL.example"}

	out := NarrowHosts([]tools.Tool{op}, caller, "", false)
	wrapped, ok := out[0].(*HTTP)
	if !ok {
		t.Fatalf("expected *HTTP, got %T", out[0])
	}
	got := append([]string(nil), wrapped.HostAllowlist...)
	sort.Strings(got)
	want := []string{"allowed.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("intersection = %v, want %v (caller cannot widen operator list)", got, want)
	}
	// Operator instance must be untouched (no shared-state mutation).
	if !reflect.DeepEqual(op.HostAllowlist, []string{"allowed.example"}) {
		t.Errorf("wrapper mutated the operator's HTTP instance: %v", op.HostAllowlist)
	}
}

// Narrowing flows through WebFetch's HTTP backend. WebFetch wraps an
// HTTP; the wrapper must narrow the inner HTTP without sharing state
// with the original.
func TestNarrowHostsWebFetchInheritsNarrowing(t *testing.T) {
	innerOrig := &HTTP{HostAllowlist: []string{"a.example", "b.example"}}
	wf := &WebFetch{HTTP: innerOrig}
	caller := []string{"a.example"}

	out := NarrowHosts([]tools.Tool{wf}, caller, "", false)
	wrapped, ok := out[0].(*WebFetch)
	if !ok {
		t.Fatalf("expected *WebFetch, got %T", out[0])
	}
	if wrapped == wf {
		t.Errorf("WebFetch wrapper should be a value copy, not the same pointer")
	}
	if wrapped.HTTP == innerOrig {
		t.Errorf("inner HTTP should be a value copy, not the same pointer")
	}
	if !reflect.DeepEqual(wrapped.HTTP.HostAllowlist, []string{"a.example"}) {
		t.Errorf("inner allowlist = %v, want [a.example]", wrapped.HTTP.HostAllowlist)
	}
	// Original untouched.
	if !reflect.DeepEqual(innerOrig.HostAllowlist, []string{"a.example", "b.example"}) {
		t.Errorf("original HTTP mutated: %v", innerOrig.HostAllowlist)
	}
}

// WebSearch.AllowedHosts is set to (HTTP-floor ∩ caller); FilterMode
// defaults to drop when narrowing. The HTTP tool in the run's slice
// supplies the operator floor — this matches the actual reachability
// (WebFetch, which shares HTTP, is what the model uses to follow up).
func TestNarrowHostsWebSearchDropDefault(t *testing.T) {
	httpTool := &HTTP{HostAllowlist: []string{"x.example", "y.example"}}
	ws := &WebSearch{APIKey: "k"}
	out := NarrowHosts([]tools.Tool{httpTool, ws}, []string{"x.example"}, "", false)
	// out[0] is the wrapped HTTP, out[1] is the wrapped WebSearch.
	wrapped := out[1].(*WebSearch)
	if wrapped == ws {
		t.Errorf("WebSearch wrapper should be a value copy")
	}
	if !reflect.DeepEqual(wrapped.AllowedHosts, []string{"x.example"}) {
		t.Errorf("AllowedHosts = %v, want [x.example] (HTTP floor ∩ caller)", wrapped.AllowedHosts)
	}
	if wrapped.FilterMode != WebSearchFilterDrop {
		t.Errorf("FilterMode = %q, want %q (default when narrowing)", wrapped.FilterMode, WebSearchFilterDrop)
	}
}

func TestNarrowHostsWebSearchKeepExplicit(t *testing.T) {
	httpTool := &HTTP{HostAllowlist: []string{"x.example"}}
	ws := &WebSearch{APIKey: "k"}
	out := NarrowHosts([]tools.Tool{httpTool, ws}, []string{"x.example"}, WebSearchFilterKeep, false)
	wrapped := out[1].(*WebSearch)
	if wrapped.FilterMode != WebSearchFilterKeep {
		t.Errorf("FilterMode = %q, want %q", wrapped.FilterMode, WebSearchFilterKeep)
	}
}

// Security parity: a WebSearch in a run with NO HTTP tool has no floor,
// so the per-request narrowing produces an empty result list.
// Symmetric with HTTP's deny-all default — a caller can't widen what
// isn't there. The model couldn't fetch anything anyway (no WebFetch),
// so this is the right answer.
func TestNarrowHostsWebSearchWithoutHTTPGetsEmpty(t *testing.T) {
	ws := &WebSearch{APIKey: "k"}
	out := NarrowHosts([]tools.Tool{ws}, []string{"x.example"}, "", false)
	wrapped := out[0].(*WebSearch)
	if len(wrapped.AllowedHosts) != 0 {
		t.Errorf("WebSearch with no HTTP floor must produce empty allowed list; got %v", wrapped.AllowedHosts)
	}
}

// Empty caller slice (NOT nil) means deny-all. The wrapped HTTP gets
// an empty allowlist; the existing HTTP refusal path takes over.
func TestNarrowHostsEmptyCallerDeniesAll(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"a.example"}}
	out := NarrowHosts([]tools.Tool{op}, []string{}, "", false)
	wrapped := out[0].(*HTTP)
	if len(wrapped.HostAllowlist) != 0 {
		t.Errorf("empty caller should produce empty allowlist; got %v", wrapped.HostAllowlist)
	}
}

// Non-network tools pass through untouched even when narrowing applies.
func TestNarrowHostsLeavesUnrelatedToolsAlone(t *testing.T) {
	r := &Read{Root: "/x"}
	w := &Write{Root: "/x"}
	b := &Bash{Enabled: true, Cwd: "/x"}
	out := NarrowHosts([]tools.Tool{r, w, b}, []string{"x.example"}, "", false)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[0] != r || out[1] != w || out[2] != b {
		t.Errorf("non-network tools must pass through by reference")
	}
}

// Critical security invariant: an operator with no static allowlist
// (HostAllowlist nil/empty) is in deny-all mode at the HTTP layer.
// A caller supplying allowed_hosts MUST NOT be able to override that
// deny-all by passing arbitrary hosts. This was a real BLOCKING bug
// in an earlier draft — intersectHosts naively returned the caller's
// list when operator was empty, letting a request to evil.com slip
// through any deny-all-by-default deployment. Empirical proof:
// reverting intersectHosts' empty-operator branch back to
// `append([]string(nil), caller...)` makes this test fail.
func TestNarrowHostsOperatorEmptyForcesDenyAll(t *testing.T) {
	op := &HTTP{HostAllowlist: nil} // operator has not set an allowlist
	out := NarrowHosts([]tools.Tool{op}, []string{"evil.example", "anywhere.example"}, "", false)
	wrapped := out[0].(*HTTP)
	if len(wrapped.HostAllowlist) != 0 {
		t.Errorf("operator deny-all must override caller; got allowlist %v, want empty", wrapped.HostAllowlist)
	}
}

// ─── StripLocalhostAliases ────────────────────────────────────────────

func TestStripLocalhostAliases(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{
			"strips literal localhost",
			[]string{"localhost", "example.com"},
			[]string{"example.com"},
		},
		{
			"strips *.localhost (RFC 6761)",
			[]string{"api.localhost", "service.localhost", "example.com"},
			[]string{"example.com"},
		},
		{
			"case-insensitive + trailing dot",
			[]string{"LOCALHOST", "Example.com.", "Localhost."},
			[]string{"Example.com."},
		},
		{
			"strips IPv4 + IPv6 loopback literals",
			[]string{"127.0.0.1", "::1", "[::1]", "0.0.0.0", "[::]", "good.example"},
			[]string{"good.example"},
		},
		{
			"keeps non-loopback IP literals",
			[]string{"8.8.8.8", "1.1.1.1"},
			[]string{"8.8.8.8", "1.1.1.1"},
		},
		{
			"preserves original casing in output",
			[]string{"Example.COM", "ApI.example.com"},
			[]string{"Example.COM", "ApI.example.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripLocalhostAliases(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripLocalhostAliases(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ─── Caller-authoritative mode + iii fallback ─────────────────────────

// CALLER_AUTHORITATIVE + caller has hosts → caller's list replaces
// operator's HostAllowlist on every network tool. Operator's list is
// NOT intersected.
func TestNarrowHostsAuthoritativeReplacesOperator(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"operator-only.example"}}
	caller := []string{"caller-wide.example", "another.example"}
	out := NarrowHosts([]tools.Tool{op}, caller, "", true)
	wrapped := out[0].(*HTTP)
	got := append([]string(nil), wrapped.HostAllowlist...)
	sort.Strings(got)
	want := []string{"another.example", "caller-wide.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("authoritative replace = %v, want %v (operator's list ignored)", got, want)
	}
}

// CALLER_AUTHORITATIVE + caller is nil → option (iii): fall back to
// operator's static list. Tools pass through unchanged so each one's
// existing HostAllowlist applies.
func TestNarrowHostsAuthoritativeNilFallsBackToOperator(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"operator-only.example"}}
	out := NarrowHosts([]tools.Tool{op}, nil, "", true)
	if out[0] != op {
		t.Errorf("nil caller in authoritative mode should pass through (operator's list applies); got wrapped instance")
	}
}

// CALLER_AUTHORITATIVE + caller is empty → also falls back to
// operator (the user's option (iii) explicit choice — different from
// INTERSECT mode where empty caller means deny-all).
func TestNarrowHostsAuthoritativeEmptyFallsBackToOperator(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"operator-only.example"}}
	out := NarrowHosts([]tools.Tool{op}, []string{}, "", true)
	if out[0] != op {
		t.Errorf("empty caller in authoritative mode should pass through; got wrapped instance")
	}
}

// Localhost-strip applies in BOTH modes. Caller passing localhost
// aliases sees them removed before policy evaluation.
func TestNarrowHostsStripsLocalhostFromCallerInAuthoritativeMode(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"some.example"}}
	caller := []string{"localhost", "127.0.0.1", "real.example"}
	out := NarrowHosts([]tools.Tool{op}, caller, "", true)
	wrapped := out[0].(*HTTP)
	if !reflect.DeepEqual(wrapped.HostAllowlist, []string{"real.example"}) {
		t.Errorf("authoritative mode should strip localhost from caller; got %v", wrapped.HostAllowlist)
	}
}

func TestNarrowHostsStripsLocalhostFromCallerInIntersectMode(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"localhost", "example.com"}} // operator should have stripped at startup, but if it didn't:
	caller := []string{"localhost", "127.0.0.1", "example.com"}
	out := NarrowHosts([]tools.Tool{op}, caller, "", false)
	wrapped := out[0].(*HTTP)
	got := append([]string(nil), wrapped.HostAllowlist...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"example.com"}) {
		t.Errorf("intersect mode should strip localhost from caller; got %v", got)
	}
}

// In authoritative mode + caller-only-loopback (becomes empty after
// strip), behaviour equals empty-caller → fall back to operator.
func TestNarrowHostsAuthoritativeAllLoopbackBecomesFallback(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"operator-only.example"}}
	caller := []string{"localhost", "127.0.0.1"} // all stripped
	out := NarrowHosts([]tools.Tool{op}, caller, "", true)
	if out[0] != op {
		t.Errorf("caller of only-loopback in authoritative mode should fall back to operator; got wrapped instance")
	}
}
