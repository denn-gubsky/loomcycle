package modelver

import "testing"

// TestCompare_DottedVersions is the operator's headline case: dotted versions
// must compare per-segment numerically, so 3.10 is newer than 3.6 (NOT the
// lexical/decimal answer where "3.6" > "3.10").
func TestCompare_DottedVersions(t *testing.T) {
	if Compare("qwen3.6", "qwen3.10", false) >= 0 {
		t.Error("qwen3.6 should be OLDER than qwen3.10 (3.10 > 3.6 per-segment)")
	}
	if Compare("qwen3.10", "qwen3.6", false) <= 0 {
		t.Error("qwen3.10 should be NEWER than qwen3.6")
	}
	if Compare("qwen3.6", "qwen3.6", false) != 0 {
		t.Error("equal ids should rank equal")
	}
}

// TestCompare_TimestampSuffix: an 8-digit YYYYMMDD suffix is a trailing integer,
// so a later snapshot of the same version is newer, and a minor-version bump
// beats an older minor's later date.
func TestCompare_TimestampSuffix(t *testing.T) {
	if Compare("claude-haiku-4-5-20251001", "claude-haiku-4-5-20260101", false) >= 0 {
		t.Error("…-20251001 should be OLDER than …-20260101 (later date newer)")
	}
	// A minor bump (4-6) beats an older minor's later date (4-5-<date>).
	if Compare("claude-haiku-4-6", "claude-haiku-4-5-20251001", false) <= 0 {
		t.Error("claude-haiku-4-6 should be NEWER than claude-haiku-4-5-20251001 (minor beats old minor's date)")
	}
}

// TestCompare_HyphenatedNumeric: hyphen-separated components compare numerically
// (4-10 > 4-5), and a higher major wins.
func TestCompare_HyphenatedNumeric(t *testing.T) {
	if Compare("claude-x-4-5", "claude-x-4-10", false) >= 0 {
		t.Error("…-4-5 should be OLDER than …-4-10 (10 > 5 numerically)")
	}
	if Compare("claude-sonnet-4-6", "claude-sonnet-5", false) >= 0 {
		t.Error("claude-sonnet-4-6 should be OLDER than claude-sonnet-5 (major 5 > 4)")
	}
}

// TestCompare_BareVsDated exercises the one genuine ambiguity: a bare major vs a
// dated snapshot at the same major. bareIsNewer flips which wins.
func TestCompare_BareVsDated(t *testing.T) {
	// Anthropic convention (bareIsNewer=true): the rolling bare alias wins.
	if Compare("claude-sonnet-5", "claude-sonnet-5-20260401", true) <= 0 {
		t.Error("bareIsNewer=true: bare claude-sonnet-5 should outrank the dated snapshot")
	}
	// Default convention (bareIsNewer=false): the concrete dated id wins.
	if Compare("claude-sonnet-5", "claude-sonnet-5-20260401", false) >= 0 {
		t.Error("bareIsNewer=false: the dated snapshot should outrank the bare stem")
	}
}

// TestCompare_DigitlessNeverOutranksVersioned: a digit-less stem (empty vector)
// must ALWAYS rank below a versioned id, even under bareIsNewer=true (it is not
// a bare major — it has no version at all).
func TestCompare_DigitlessNeverOutranksVersioned(t *testing.T) {
	for _, bare := range []bool{false, true} {
		if Compare("gpt-oss", "gpt-5.4", bare) >= 0 {
			t.Errorf("bareIsNewer=%v: digit-less gpt-oss must rank below versioned gpt-5.4", bare)
		}
		if Compare("gpt-5.4", "gpt-oss", bare) <= 0 {
			t.Errorf("bareIsNewer=%v: versioned gpt-5.4 must rank above digit-less gpt-oss", bare)
		}
	}
}

// TestCompare_OllamaTagStripped: an Ollama ":tag" suffix is stripped before
// comparison, so the embedded version drives the order.
func TestCompare_OllamaTagStripped(t *testing.T) {
	if Compare("qwen3.6:latest", "qwen3.10:latest", false) >= 0 {
		t.Error("qwen3.6:latest should be OLDER than qwen3.10:latest (tag stripped, 3.10 > 3.6)")
	}
	if Compare("qwen3.6:latest", "qwen3.6", false) != 0 {
		t.Error("a :tag must not change the version rank")
	}
}

// TestNewest_AnthropicHaiku: over the live Anthropic haiku family, Newest picks
// the (only) dated haiku — the exact fix the config needed.
func TestNewest_AnthropicHaiku(t *testing.T) {
	got, ok := Newest([]string{"claude-haiku-4-5-20251001"}, true)
	if !ok || got != "claude-haiku-4-5-20251001" {
		t.Errorf("Newest = (%q, %v), want claude-haiku-4-5-20251001", got, ok)
	}
}

// TestNewest_AnthropicSonnet: with the Anthropic bare-rolling-alias rule, the
// bare claude-sonnet-5 is newest of its family over 4-6 and a dated 4-5.
func TestNewest_AnthropicSonnet(t *testing.T) {
	ids := []string{"claude-sonnet-4-6", "claude-sonnet-4-5-20250929", "claude-sonnet-5"}
	got, ok := Newest(ids, true)
	if !ok || got != "claude-sonnet-5" {
		t.Errorf("Newest = (%q, %v), want claude-sonnet-5", got, ok)
	}
}

// TestNewest_MovesWithCatalog: injecting a newer id changes the winner (the
// change-detection the resolver logs on).
func TestNewest_MovesWithCatalog(t *testing.T) {
	before, _ := Newest([]string{"claude-haiku-4-5-20251001"}, true)
	after, _ := Newest([]string{"claude-haiku-4-5-20251001", "claude-haiku-5"}, true)
	if before == after {
		t.Errorf("Newest should move when a newer id appears: before=%q after=%q", before, after)
	}
	if after != "claude-haiku-5" {
		t.Errorf("after = %q, want claude-haiku-5 (major 5 > 4)", after)
	}
}

// TestNewest_SingleAndEmpty: a single match (even digit-less) resolves; an empty
// set does not.
func TestNewest_SingleAndEmpty(t *testing.T) {
	if got, ok := Newest([]string{"gpt-oss:latest"}, false); !ok || got != "gpt-oss:latest" {
		t.Errorf("single digit-less match should resolve: got (%q,%v)", got, ok)
	}
	if _, ok := Newest(nil, false); ok {
		t.Error("empty set must not resolve")
	}
}

// TestNewest_DigitlessAmbiguous: several digit-less ids can't be ranked → the
// caller is told to fall through (RFC BG "narrow the glob or pin").
func TestNewest_DigitlessAmbiguous(t *testing.T) {
	if got, ok := Newest([]string{"gpt-oss", "gpt-oss-preview"}, false); ok {
		t.Errorf("ambiguous digit-less set must not resolve, got %q", got)
	}
}

// TestNewest_Mixed confirms the generic scheme across provider families (each
// scoped by its own glob in practice).
func TestNewest_Mixed(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		want string
	}{
		{"gemini", []string{"gemini-2.5-flash-lite", "gemini-2.0-flash"}, "gemini-2.5-flash-lite"},
		{"gpt", []string{"gpt-5.4-mini", "gpt-5.5-mini", "gpt-4.1-mini"}, "gpt-5.5-mini"},
		{"deepseek", []string{"deepseek-v4-flash", "deepseek-v3-flash"}, "deepseek-v4-flash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := Newest(tc.ids, false); !ok || got != tc.want {
				t.Errorf("Newest = (%q, %v), want %q", got, ok, tc.want)
			}
		})
	}
}
