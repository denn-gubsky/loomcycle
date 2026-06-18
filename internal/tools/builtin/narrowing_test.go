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
		&Read{},
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
	r := &Read{}
	w := &Write{}
	b := &Bash{Enabled: true}
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
		{
			"strips loopback host:port",
			[]string{"localhost:3000", "127.0.0.1:8080", "[::1]:443", "good.example:443"},
			[]string{"good.example:443"},
		},
		{
			"strips *.localhost:port",
			[]string{"api.localhost:9000", "service.localhost:80"},
			[]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripLocalhostAliases(tc.in, nil)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripLocalhostAliases(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Exempt parameter: entries listed there survive the strip even if
// they're loopback. Use case: operator opts in to localhost callbacks
// via LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST; without this exemption,
// caller-supplied "localhost" gets stripped and Phase B agents fail.
func TestStripLocalhostAliasesExempt(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		exempt []string
		want   []string
	}{
		{
			"localhost on exempt survives",
			[]string{"localhost", "127.0.0.1", "real.example"},
			[]string{"localhost"},
			[]string{"localhost", "real.example"},
		},
		{
			"both loopbacks on exempt survive",
			[]string{"localhost", "127.0.0.1", "::1", "real.example"},
			[]string{"localhost", "127.0.0.1", "::1"},
			[]string{"localhost", "127.0.0.1", "::1", "real.example"},
		},
		{
			"exempt is case-insensitive",
			[]string{"LocalHost", "real.example"},
			[]string{"localhost"},
			[]string{"LocalHost", "real.example"},
		},
		{
			"exempt with trailing dot still matches",
			[]string{"localhost.", "real.example"},
			[]string{"localhost"},
			[]string{"localhost.", "real.example"},
		},
		{
			"exempt only protects what's listed; non-exempt loopback still stripped",
			[]string{"localhost", "127.0.0.1"},
			[]string{"localhost"},
			[]string{"localhost"},
		},
		{
			"empty exempt is identical to nil exempt",
			[]string{"localhost", "real.example"},
			[]string{},
			[]string{"real.example"},
		},
		{
			"exempt that doesn't appear in input is a no-op",
			[]string{"localhost", "real.example"},
			[]string{"127.0.0.1"},
			[]string{"real.example"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripLocalhostAliases(tc.in, tc.exempt)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripLocalhostAliases(%v, exempt=%v) = %v, want %v", tc.in, tc.exempt, got, tc.want)
			}
		})
	}
}

// End-to-end Phase B semantics: caller supplies allowed_hosts:
// ["localhost", "127.0.0.1"], operator has PrivateHostAllowlist with
// the same entries, CALLER_AUTHORITATIVE=true. Result: the wrapped
// HTTP tool gets HostAllowlist=["localhost","127.0.0.1"] (preserved
// through the strip) — making jobs-search-agent's localhost callback
// reachable.
func TestNarrowHostsPhaseBLocalhostCallback(t *testing.T) {
	httpTool := &HTTP{
		HostAllowlist:        nil, // operator opts out of static list
		PrivateHostAllowlist: []string{"localhost", "127.0.0.1"},
	}
	caller := []string{"localhost", "127.0.0.1"}
	out := NarrowHosts([]tools.Tool{httpTool}, caller, "", true)
	wrapped := out[0].(*HTTP)
	got := append([]string(nil), wrapped.HostAllowlist...)
	sort.Strings(got)
	want := []string{"127.0.0.1", "localhost"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Phase B localhost callback: got HostAllowlist=%v, want %v", got, want)
	}
}

// Reviewer-flagged edge case: agent allowed WebFetch but NOT a
// standalone HTTP. WebFetch wraps an inner *HTTP that carries the
// same operator-configured PrivateHostAllowlist. Without unwrapping
// *WebFetch, findHTTPPrivateAllowlist would return nil and Phase B's
// promise breaks for that agent shape (caller-supplied "localhost"
// gets stripped even though the operator opted in).
func TestNarrowHostsWebFetchOnlyHonoursPrivateAllowlist(t *testing.T) {
	innerHTTP := &HTTP{
		HostAllowlist:        nil,
		PrivateHostAllowlist: []string{"localhost"},
	}
	wf := &WebFetch{HTTP: innerHTTP}
	caller := []string{"localhost"}
	out := NarrowHosts([]tools.Tool{wf}, caller, "", true)
	wrapped := out[0].(*WebFetch)
	if wrapped.HTTP == nil {
		t.Fatal("wrapped WebFetch lost its HTTP backend")
	}
	if !reflect.DeepEqual(wrapped.HTTP.HostAllowlist, []string{"localhost"}) {
		t.Errorf("WebFetch-only run should preserve operator-exempt localhost; got %v", wrapped.HTTP.HostAllowlist)
	}
}

// Inverse: same setup but operator did NOT set PrivateHostAllowlist.
// Caller's loopback entries get stripped; in CALLER_AUTHORITATIVE mode
// that empties the caller list which falls back to operator's static
// (also empty) → effectively deny-all. This is the v0.3.4 default
// behaviour we want to preserve when the operator hasn't opted in.
func TestNarrowHostsLocalhostStrippedWhenNoPrivateAllowlist(t *testing.T) {
	httpTool := &HTTP{
		HostAllowlist:        nil,
		PrivateHostAllowlist: nil, // operator did NOT opt in
	}
	caller := []string{"localhost", "127.0.0.1"}
	out := NarrowHosts([]tools.Tool{httpTool}, caller, "", true)
	wrapped := out[0].(*HTTP)
	if len(wrapped.HostAllowlist) != 0 {
		t.Errorf("without PrivateHostAllowlist, all loopback should strip and fall back to operator's empty list; got %v", wrapped.HostAllowlist)
	}
}

// ─── Caller-authoritative mode + iii fallback ─────────────────────────

// CALLER_AUTHORITATIVE + caller has hosts → caller's list replaces
// operator's HostAllowlist on every network tool. Operator's list is
// NOT intersected. Order assertion is exact (no sort) so a future
// internal sort/dedupe inside replaceHostsInTools would surface as a
// test failure — caller-supplied order is part of the contract.
func TestNarrowHostsAuthoritativeReplacesOperator(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"operator-only.example"}}
	caller := []string{"caller-wide.example", "another.example"}
	out := NarrowHosts([]tools.Tool{op}, caller, "", true)
	wrapped := out[0].(*HTTP)
	if !reflect.DeepEqual(wrapped.HostAllowlist, caller) {
		t.Errorf("authoritative replace = %v, want %v exactly (caller's order preserved)", wrapped.HostAllowlist, caller)
	}
	// Operator's instance must be untouched.
	if !reflect.DeepEqual(op.HostAllowlist, []string{"operator-only.example"}) {
		t.Errorf("operator instance mutated: %v", op.HostAllowlist)
	}
}

// assertOperatorFallback checks the semantic invariant of option (iii):
// the wrapped tool's HostAllowlist matches the operator's, AND the
// operator's instance was NOT mutated. Catches both "tool dropped
// the operator's list" regressions AND "tool mutated operator's
// shared slice" regressions. Doesn't depend on the implementation
// detail of "is the same pointer returned".
func assertOperatorFallback(t *testing.T, out tools.Tool, op *HTTP, originalOpHosts []string) {
	t.Helper()
	wrapped, ok := out.(*HTTP)
	if !ok {
		t.Fatalf("expected *HTTP, got %T", out)
	}
	if !reflect.DeepEqual(wrapped.HostAllowlist, op.HostAllowlist) {
		t.Errorf("wrapped HostAllowlist = %v, want operator's %v", wrapped.HostAllowlist, op.HostAllowlist)
	}
	// Operator instance must not have been mutated.
	if !reflect.DeepEqual(op.HostAllowlist, originalOpHosts) {
		t.Errorf("operator instance mutated: HostAllowlist now %v, was %v", op.HostAllowlist, originalOpHosts)
	}
}

// CALLER_AUTHORITATIVE + caller is nil → option (iii): fall back to
// operator's static list. Tools pass through unchanged so each one's
// existing HostAllowlist applies.
func TestNarrowHostsAuthoritativeNilFallsBackToOperator(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"operator-only.example"}}
	originalHosts := append([]string(nil), op.HostAllowlist...)
	out := NarrowHosts([]tools.Tool{op}, nil, "", true)
	assertOperatorFallback(t, out[0], op, originalHosts)
}

// CALLER_AUTHORITATIVE + caller is empty → also falls back to
// operator (the user's option (iii) explicit choice — different from
// INTERSECT mode where empty caller means deny-all).
func TestNarrowHostsAuthoritativeEmptyFallsBackToOperator(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"operator-only.example"}}
	originalHosts := append([]string(nil), op.HostAllowlist...)
	out := NarrowHosts([]tools.Tool{op}, []string{}, "", true)
	assertOperatorFallback(t, out[0], op, originalHosts)
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
	originalHosts := append([]string(nil), op.HostAllowlist...)
	caller := []string{"localhost", "127.0.0.1"} // all stripped
	out := NarrowHosts([]tools.Tool{op}, caller, "", true)
	assertOperatorFallback(t, out[0], op, originalHosts)
}
