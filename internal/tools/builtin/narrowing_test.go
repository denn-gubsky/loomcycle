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
	out := NarrowHosts(original, nil, "")
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

	out := NarrowHosts([]tools.Tool{op}, caller, "")
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

	out := NarrowHosts([]tools.Tool{wf}, caller, "")
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
	out := NarrowHosts([]tools.Tool{httpTool, ws}, []string{"x.example"}, "")
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
	out := NarrowHosts([]tools.Tool{httpTool, ws}, []string{"x.example"}, WebSearchFilterKeep)
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
	out := NarrowHosts([]tools.Tool{ws}, []string{"x.example"}, "")
	wrapped := out[0].(*WebSearch)
	if len(wrapped.AllowedHosts) != 0 {
		t.Errorf("WebSearch with no HTTP floor must produce empty allowed list; got %v", wrapped.AllowedHosts)
	}
}

// Empty caller slice (NOT nil) means deny-all. The wrapped HTTP gets
// an empty allowlist; the existing HTTP refusal path takes over.
func TestNarrowHostsEmptyCallerDeniesAll(t *testing.T) {
	op := &HTTP{HostAllowlist: []string{"a.example"}}
	out := NarrowHosts([]tools.Tool{op}, []string{}, "")
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
	out := NarrowHosts([]tools.Tool{r, w, b}, []string{"x.example"}, "")
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
	out := NarrowHosts([]tools.Tool{op}, []string{"evil.example", "anywhere.example"}, "")
	wrapped := out[0].(*HTTP)
	if len(wrapped.HostAllowlist) != 0 {
		t.Errorf("operator deny-all must override caller; got allowlist %v, want empty", wrapped.HostAllowlist)
	}
}
