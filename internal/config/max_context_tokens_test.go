package config

import "testing"

// TestMergeMaxContextTokens pins the per-run-wins-when-set scalar merge (RFC CJ
// phase 2). Unlike sampling, 0 is "unset" (a window is never meaningfully 0), so
// only a positive override wins; 0 and a nonsensical negative both defer to the
// agent's own value.
func TestMergeMaxContextTokens(t *testing.T) {
	cases := []struct {
		name           string
		base, override int
		want           int
	}{
		{"override wins over base", 8192, 16384, 16384},
		{"zero override inherits base", 8192, 0, 8192},
		{"negative override inherits base", 8192, -1, 8192},
		{"override on unset base", 0, 16384, 16384},
		{"both unset stays zero", 0, 0, 0},
		{"override can lower base", 131072, 8192, 8192},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeMaxContextTokens(tc.base, tc.override); got != tc.want {
				t.Errorf("MergeMaxContextTokens(%d, %d) = %d, want %d", tc.base, tc.override, got, tc.want)
			}
		})
	}
}
