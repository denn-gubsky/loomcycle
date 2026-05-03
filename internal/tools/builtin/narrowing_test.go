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

// WebSearch.AllowedHosts is set to the intersection; FilterMode
// defaults to drop when AllowedHosts is being set for the first time.
func TestNarrowHostsWebSearchDropDefault(t *testing.T) {
	ws := &WebSearch{APIKey: "k"}
	out := NarrowHosts([]tools.Tool{ws}, []string{"x.example"}, "")
	wrapped := out[0].(*WebSearch)
	if wrapped == ws {
		t.Errorf("WebSearch wrapper should be a value copy")
	}
	if !reflect.DeepEqual(wrapped.AllowedHosts, []string{"x.example"}) {
		t.Errorf("AllowedHosts = %v", wrapped.AllowedHosts)
	}
	if wrapped.FilterMode != WebSearchFilterDrop {
		t.Errorf("FilterMode = %q, want %q (default when narrowing)", wrapped.FilterMode, WebSearchFilterDrop)
	}
}

func TestNarrowHostsWebSearchKeepExplicit(t *testing.T) {
	ws := &WebSearch{APIKey: "k"}
	out := NarrowHosts([]tools.Tool{ws}, []string{"x.example"}, WebSearchFilterKeep)
	wrapped := out[0].(*WebSearch)
	if wrapped.FilterMode != WebSearchFilterKeep {
		t.Errorf("FilterMode = %q, want %q", wrapped.FilterMode, WebSearchFilterKeep)
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

// Operator's static list empty + caller list set → caller list passes
// through. Useful for test configs where no operator allowlist exists
// but the caller still wants to constrain.
func TestNarrowHostsOperatorEmptyCallerPassesThrough(t *testing.T) {
	op := &HTTP{HostAllowlist: nil}
	out := NarrowHosts([]tools.Tool{op}, []string{"x.example", "y.example"}, "")
	wrapped := out[0].(*HTTP)
	got := append([]string(nil), wrapped.HostAllowlist...)
	sort.Strings(got)
	want := []string{"x.example", "y.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with no operator list, caller list passes through; got %v", got)
	}
}
