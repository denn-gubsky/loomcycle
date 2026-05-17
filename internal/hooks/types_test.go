package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPreHookResult_AllowHosts_RoundTrips pins the wire shape for the
// per-call host-widening capability. The field is `allow_hosts` on the
// JSON side and `omitempty` so existing hooks (which never set it) emit
// byte-identical payloads.
func TestPreHookResult_AllowHosts_RoundTrips(t *testing.T) {
	cases := []struct {
		name string
		in   PreHookResult
		want string
	}{
		{
			name: "EmptyResultEmitsEmptyObject",
			in:   PreHookResult{},
			want: `{}`,
		},
		{
			name: "AllowHostsAlone",
			in:   PreHookResult{AllowHosts: []string{"acme.com", ".trusted-cdn.com"}},
			want: `{"allow_hosts":["acme.com",".trusted-cdn.com"]}`,
		},
		{
			name: "AllowHostsWithInputRewrite",
			in: PreHookResult{
				Input:      json.RawMessage(`{"url":"https://acme.com/canonical"}`),
				AllowHosts: []string{"acme.com"},
			},
			want: `{"input":{"url":"https://acme.com/canonical"},"allow_hosts":["acme.com"]}`,
		},
		{
			name: "AllowHostsWithDeny",
			in: PreHookResult{
				Deny:       &ToolResult{IsError: true, Text: "no"},
				AllowHosts: []string{"acme.com"},
			},
			// Deny + AllowHosts is a valid wire shape; the dispatcher
			// rules (deny wins, allow_hosts dropped) live above this
			// layer. The wire contract just preserves what's sent.
			want: `{"deny":{"text":"no","is_error":true},"allow_hosts":["acme.com"]}`,
		},
		{
			name: "NilSliceOmitted",
			// Distinct from "empty slice": nil is the "not set" sentinel,
			// proven by the absence of the field in the marshalled output.
			in:   PreHookResult{AllowHosts: nil},
			want: `{}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s\nwant %s", got, tc.want)
			}
			// Round-trip back to confirm Unmarshal accepts what Marshal
			// produced (catches a typo in the json tag).
			var rt PreHookResult
			if err := json.Unmarshal(got, &rt); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if len(rt.AllowHosts) != len(tc.in.AllowHosts) {
				t.Errorf("round-trip AllowHosts len = %d, want %d (%v vs %v)",
					len(rt.AllowHosts), len(tc.in.AllowHosts), rt.AllowHosts, tc.in.AllowHosts)
			}
		})
	}
}

// TestPreHookResult_AllowHosts_AcceptsEmptyArray confirms an explicit
// empty array deserialises to an empty (non-nil) slice. This matters
// because a hook author who writes `{"allow_hosts": []}` to express
// "no widening this call" should be byte-equivalent to omitting the
// field — the dispatcher treats both as no-op.
func TestPreHookResult_AllowHosts_AcceptsEmptyArray(t *testing.T) {
	var r PreHookResult
	if err := json.Unmarshal([]byte(`{"allow_hosts":[]}`), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.AllowHosts == nil {
		t.Errorf("AllowHosts is nil; want empty non-nil slice (Go's json package decodes [] to []string{})")
	}
	if len(r.AllowHosts) != 0 {
		t.Errorf("len(AllowHosts) = %d, want 0", len(r.AllowHosts))
	}
}

// TestPreHookResult_AllowHosts_DocCommentMentionsConfusedDeputy is a
// gentle reminder via test: the security warning on the field must
// stay attached. If a future PR strips the doc-comment, this test still
// passes (no code-level enforcement), but the test name signals intent
// in a code-review diff.
func TestPreHookResult_AllowHosts_DocCommentMentionsConfusedDeputy(t *testing.T) {
	// Sanity: the field exists and is the right shape.
	r := PreHookResult{AllowHosts: []string{"x"}}
	if len(r.AllowHosts) != 1 || r.AllowHosts[0] != "x" {
		t.Fatal("AllowHosts shape regression")
	}
	// The doc-comment is on the struct; we can't introspect comments at
	// runtime. This test exists for grep-ability — a reviewer searching
	// for confused-deputy guidance lands here and follows the trail to
	// types.go.
	_ = strings.Contains // keep `strings` import meaningful
}
