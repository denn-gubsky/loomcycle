package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

func TestApplyAllowedToolsFilter(t *testing.T) {
	descs := []mcp.ToolDescriptor{
		{Name: "search"},
		{Name: "fetch"},
		{Name: "summarise"},
	}

	tests := []struct {
		name    string
		allowed []string
		want    []string // tool names
	}{
		{
			name:    "empty allowed = pass through",
			allowed: nil,
			want:    []string{"search", "fetch", "summarise"},
		},
		{
			name:    "exact match keeps named tools",
			allowed: []string{"search", "fetch"},
			want:    []string{"search", "fetch"},
		},
		{
			name:    "non-matching name dropped",
			allowed: []string{"nonexistent"},
			want:    []string{},
		},
		{
			name:    "duplicate in allowed is harmless",
			allowed: []string{"search", "search"},
			want:    []string{"search"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyAllowedToolsFilter(descs, tc.allowed)
			gotNames := make([]string, 0, len(got))
			for _, d := range got {
				gotNames = append(gotNames, d.Name)
			}
			if len(gotNames) == 0 {
				gotNames = []string{} // normalise nil vs empty
			}
			if !reflect.DeepEqual(gotNames, tc.want) {
				t.Errorf("got %v, want %v", gotNames, tc.want)
			}
		})
	}
}

// TestFormatBuildInfo exhaustively covers the version/commit display
// normalisation. Driving the pure formatter with synthetic inputs
// avoids any dependency on whether the test binary itself was built
// with VCS stamping (`go test` doesn't embed VCS info by default).
func TestFormatBuildInfo(t *testing.T) {
	cases := []struct {
		name        string
		mainVersion string
		rev         string
		builtAt     string
		dirty       bool
		wantVersion string
		wantCommit  string
		wantTS      string
	}{
		{
			name:        "release_tag_clean",
			mainVersion: "v0.8.14",
			rev:         "7c2ca2e1f592abcd",
			builtAt:     "2026-05-13T18:04:43Z",
			wantVersion: "v0.8.14",
			wantCommit:  "7c2ca2e1f592", // truncated to 12
			wantTS:      "2026-05-13T18:04:43Z",
		},
		{
			// Go pseudo-versions get stripped to their semver base —
			// the timestamped suffix is redundant with the separate
			// commit + built fields. Operators want "v0.8.14" in the
			// topbar, not "v0.8.14-0.20260513180443-7c2ca2e1f592".
			name:        "pseudo_version_stripped_dirty",
			mainVersion: "v0.8.14-0.20260513180443-7c2ca2e1f592",
			rev:         "7c2ca2e1f592abcd",
			builtAt:     "2026-05-13T18:04:43Z",
			dirty:       true,
			wantVersion: "v0.8.14",
			wantCommit:  "7c2ca2e1f592-dirty",
			wantTS:      "2026-05-13T18:04:43Z",
		},
		{
			name:        "pseudo_version_stripped_clean",
			mainVersion: "v0.8.21-0.20260520061001-1107844c6cf7",
			rev:         "1107844c6cf7",
			builtAt:     "2026-05-20T06:10:01Z",
			wantVersion: "v0.8.21",
			wantCommit:  "1107844c6cf7",
			wantTS:      "2026-05-20T06:10:01Z",
		},
		{
			// `go build` on a dirty working tree appends "+dirty" to
			// the synthesised Main.Version. The strip must tolerate
			// this suffix — the "-dirty" signal still surfaces via
			// commit (driven by the separate vcs.modified setting).
			name:        "pseudo_version_with_plus_dirty_stripped",
			mainVersion: "v0.8.21-0.20260520061001-1107844c6cf7+dirty",
			rev:         "1107844c6cf7",
			builtAt:     "2026-05-20T06:10:01Z",
			dirty:       true,
			wantVersion: "v0.8.21",
			wantCommit:  "1107844c6cf7-dirty",
			wantTS:      "2026-05-20T06:10:01Z",
		},
		{
			// Real semver prerelease tags must NOT be stripped. The
			// pseudo-version regex is intentionally narrow (requires
			// the literal "-0.<14 digits>-<hex>" tail) so legitimate
			// suffixes like "-rc1" pass through unchanged.
			name:        "real_prerelease_tag_preserved",
			mainVersion: "v0.8.14-rc1",
			rev:         "abcdef012345",
			wantVersion: "v0.8.14-rc1",
			wantCommit:  "abcdef012345",
		},
		{
			name:        "devel_marker_normalised",
			mainVersion: "(devel)",
			rev:         "abc123def456",
			wantVersion: "devel",
			wantCommit:  "abc123def456",
		},
		{
			name:        "empty_main_version_normalised",
			mainVersion: "",
			rev:         "abc123",
			wantVersion: "devel",
			wantCommit:  "abc123",
		},
		{
			name:        "short_rev_dirty",
			mainVersion: "v0.8.14",
			rev:         "abc",
			dirty:       true,
			wantVersion: "v0.8.14",
			wantCommit:  "abc-dirty",
		},
		{
			name:        "empty_rev_dirty_no_suffix",
			mainVersion: "v0.8.14",
			rev:         "",
			dirty:       true,
			wantVersion: "v0.8.14",
			wantCommit:  "", // no "-dirty" appended to an empty rev
		},
		{
			name:        "exactly_12_char_rev_untruncated",
			mainVersion: "v0.8.14",
			rev:         "abcdef012345", // exactly 12 chars
			wantVersion: "v0.8.14",
			wantCommit:  "abcdef012345",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotV, gotC, gotT := formatBuildInfo(tc.mainVersion, tc.rev, tc.builtAt, tc.dirty)
			if gotV != tc.wantVersion {
				t.Errorf("version: got %q want %q", gotV, tc.wantVersion)
			}
			if gotC != tc.wantCommit {
				t.Errorf("commit: got %q want %q", gotC, tc.wantCommit)
			}
			if gotT != tc.wantTS {
				t.Errorf("ts: got %q want %q", gotT, tc.wantTS)
			}
		})
	}
}

// TestResolveBuildInfo_DoesNotPanic is a smoke test for the
// runtime/debug.ReadBuildInfo() integration. The exact returned values
// depend on the build mode — `go test` typically doesn't stamp VCS
// info, while `go build` does — so we only assert the helper returns
// without panicking and that any non-empty commit obeys the formatter's
// shape contract (≤12 hex chars, possibly "-dirty" suffix).
func TestResolveBuildInfo_DoesNotPanic(t *testing.T) {
	_, commit, _ := resolveBuildInfo()
	if commit == "" {
		t.Logf("commit empty (no VCS info embedded in test binary — expected for go test)")
		return
	}
	bare := strings.TrimSuffix(commit, "-dirty")
	if len(bare) > 12 {
		t.Errorf("commit %q exceeds 12-char truncation", commit)
	}
	for _, r := range bare {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			t.Errorf("commit %q contains non-hex char %q", commit, r)
			break
		}
	}
}

// TestExpandedStdioEnv is the F39 regression: a dynamic stdio MCP server's env
// values must be ExpandEnv'd at spawn (the static yaml path arrives
// pre-expanded; the dynamic def stores raw ${...}). Allowlisted LOOMCYCLE_*
// expands; literals and non-allowlisted ${...} pass through verbatim; output
// is sorted by key. Fail-before: the value was emitted literally
// (BOT_TOKEN=${LOOMCYCLE_F39_TOKEN}).
func TestExpandedStdioEnv(t *testing.T) {
	t.Setenv("LOOMCYCLE_F39_TOKEN", "s3cret")

	got := expandedStdioEnv(map[string]string{
		"BOT_TOKEN": "${LOOMCYCLE_F39_TOKEN}", // allowlisted → expands
		"CHAT_ID":   "12345",                  // literal → unchanged
		"PASSTHRU":  "${NOT_ALLOWLISTED}",     // off-allowlist → verbatim
	})
	want := []string{
		"BOT_TOKEN=s3cret",
		"CHAT_ID=12345",
		"PASSTHRU=${NOT_ALLOWLISTED}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expandedStdioEnv:\n got %q\nwant %q", got, want)
	}
}
