// Package modelver ranks provider model ids by recency using a generic
// numeric-run scheme (RFC BG — model-pattern / latest resolution). It splits a
// model id into its runs of digits and compares those runs element-wise as
// integers, so it orders every id scheme loomcycle sees where naive lexical
// sorting fails:
//
//   - dotted        "qwen3.6"  <  "qwen3.10"          ([3,6]  < [3,10])
//   - hyphenated    "…-4-5"    <  "…-4-6"             ([4,5]  < [4,6])
//   - timestamped   "…-4-5-20251001" < "…-4-5-20260101"
//   - mixed         "gemini-2.5-flash-lite" → [2,5], "gpt-5.4-mini" → [5,4]
//
// The comparator is family-agnostic: it ranks only the numeric components, so
// callers must scope the candidate set to one model family first (the RFC BG
// glob's literal prefix, e.g. "claude-haiku-*", does this). Comparing ids from
// different families is meaningless.
//
// The one genuine ambiguity — a bare stem vs. a dated snapshot at the same
// version (e.g. "claude-sonnet-5" vs "claude-sonnet-5-20260401") — is provider
// convention, not arithmetic, and is resolved by the bareIsNewer flag the caller
// supplies (Anthropic publishes a bare major as a rolling alias for the newest
// snapshot of its generation → bareIsNewer=true).
package modelver

// vector tokenizes a model id into its ordered numeric components. An
// Ollama-style ":tag" suffix ("qwen3.6:latest") is stripped first; then every
// maximal run of ASCII digits becomes one int and all non-digit separators
// (".", "-", "_", letters) are discarded. A model id with no digits ("gpt-oss")
// yields an empty vector — it is not version-comparable.
func vector(id string) []int {
	// Strip an Ollama ":tag" (name:tag) so the tag never enters the version
	// comparison; the selected id keeps its tag when it is actually called.
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			id = id[:i]
			break
		}
	}
	var out []int
	for i := 0; i < len(id); {
		if id[i] < '0' || id[i] > '9' {
			i++
			continue
		}
		j := i
		n := 0
		overflow := false
		for j < len(id) && id[j] >= '0' && id[j] <= '9' {
			// Manual accumulation (avoids strconv + bounds a pathological
			// digit run). Real components are small (versions) or 8-digit
			// dates, so overflow is unreachable in practice; guard anyway.
			if n > (maxInt-9)/10 {
				overflow = true
			} else {
				n = n*10 + int(id[j]-'0')
			}
			j++
		}
		if overflow {
			n = maxInt // a pathological run sorts last-resort high, never panics
		}
		out = append(out, n)
		i = j
	}
	return out
}

const maxInt = int(^uint(0) >> 1)

// Compare orders model ids a and b by recency. Returns -1 when a is OLDER than
// b, +1 when NEWER, 0 when equal-rank. Ranking is element-wise numeric over each
// id's version vector; the first differing component decides (larger = newer).
//
// When one vector is a strict prefix of the other (all shared components equal,
// one has extra trailing components — typically a date), the tie is broken by
// bareIsNewer:
//   - bareIsNewer=true  → the SHORTER (bare) vector is newer. Anthropic's rolling
//     major alias ("claude-sonnet-5") tracks the newest snapshot of its
//     generation, so it outranks any dated "claude-sonnet-5-<date>".
//   - bareIsNewer=false → the LONGER (dated snapshot) vector is newer — the
//     provider-agnostic default: a concrete dated id is newer than a bare stem.
func Compare(a, b string, bareIsNewer bool) int {
	va, vb := vector(a), vector(b)
	n := len(va)
	if len(vb) < n {
		n = len(vb)
	}
	for i := 0; i < n; i++ {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(va) == len(vb):
		return 0
	case len(va) < len(vb): // a is the shorter one
		if len(va) == 0 {
			// a has NO version (a digit-less stem), not a bare major — it always
			// ranks below a versioned id, regardless of bareIsNewer.
			return -1
		}
		// a is a genuine bare version (a prefix of b, e.g. [5] vs [5,date]).
		if bareIsNewer {
			return 1
		}
		return -1
	default: // b is the shorter one
		if len(vb) == 0 {
			return 1 // b digit-less → a (versioned) is newer
		}
		if bareIsNewer {
			return -1
		}
		return 1
	}
}

// Newest returns the id in ids that ranks newest under Compare, and whether a
// unique newest was found. Returns ("", false) when:
//   - ids is empty, or
//   - the match is AMBIGUOUS: more than one id is present and the winner has no
//     version vector (a set of digit-less ids like "gpt-oss" / "gpt-oss-preview"
//     the comparator cannot order). The caller should then WARN and fall through
//     (RFC BG "narrow the glob or pin").
//
// A single id — even a digit-less one — is always returned (a unique glob match
// needs no ranking). Ties among version-comparable ids (identical vectors) break
// by lexical order (greatest wins) for deterministic selection.
func Newest(ids []string, bareIsNewer bool) (string, bool) {
	switch len(ids) {
	case 0:
		return "", false
	case 1:
		return ids[0], true
	}
	best := ids[0]
	for _, id := range ids[1:] {
		if c := Compare(id, best, bareIsNewer); c > 0 || (c == 0 && id > best) {
			best = id
		}
	}
	// A non-empty vector always outranks an empty one (Compare treats the
	// digit-bearing id as "longer" → newer under the default), so `best` has an
	// empty vector only when EVERY candidate does — an unrankable digit-less set.
	if len(vector(best)) == 0 {
		return "", false
	}
	return best, true
}
