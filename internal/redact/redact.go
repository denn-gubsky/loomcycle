// Package redact masks secret-shaped substrings before they are persisted at
// rest. F32: agent tool I/O (tool_call inputs + tool_result outputs) is stored
// verbatim in the events.payload BLOB; a secret inlined on a Bash cmdline
// (`curl -H "Authorization: token <TOKEN>"`) or echoed in a tool result would
// otherwise sit in cleartext in the DB — and, because the same blob propagates
// to snapshots and the /v1/_events audit API, in every copy/backup too.
//
// Two tiers, both applied:
//   - Tier A — exact known env-secret values (zero false positives). The caller
//     passes the VALUES of env vars classified as secret (config.IsSecretEnvName);
//     any exact occurrence is masked as [redacted:NAME]. This catches the
//     confirmed repro (LOOMCYCLE_GITEA_TOKEN inlined in a curl cmdline) exactly,
//     because we redact only values we actually hold.
//   - Tier B — conservative pattern heuristics (catches secrets NOT sourced from
//     env, e.g. a token an agent fetched at runtime). Each rule targets a
//     distinctive secret shape; see patternRules for the per-rule rationale.
//
// A Redactor's static tiers are fixed at New; runtime-resolved secret values may
// be added later via Register (RFC AV/AR). It is safe for concurrent
// String/Bytes/Register calls — the registered set is guarded by an RWMutex. The
// zero value / a nil *Redactor is a no-op, so callers may hold a nil redactor
// when redaction is disabled.
package redact

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// minSecretLen is the shortest env value Tier A will mask. A shorter value is
// too generic to be a meaningful credential, and masking a common short word
// (e.g. an env var that happens to be set to "true") would be a false positive.
const minSecretLen = 8

// minDynamicSecretLen is the shortest runtime-registered (Register) value we
// mask. Registered values are EXACT known secrets (zero false positives on the
// exact string), so the floor only guards against masking trivially short
// values that would appear everywhere.
const minDynamicSecretLen = 4

// dynamicMarker replaces a runtime-registered secret value. Register carries no
// env NAME (the value is resolved at run time, e.g. an RFC AR $cred:), so unlike
// Tier A's [redacted:NAME] the marker is generic.
const dynamicMarker = "[redacted:credential]"

// Redactor masks secret-shaped substrings.
type Redactor struct {
	replacer *strings.Replacer // Tier A: exact env-value → [redacted:NAME]; nil if none
	patterns []patternRule     // Tier B: nil when patterns are disabled

	// Tier A' — runtime-registered exact secret values (RFC AV/AR: resolved
	// $cred: values + provider-key overrides). Registered as credentials resolve,
	// so a downstream tool that echoes one is masked in the persisted transcript.
	// dynamic is the deduped value set (bounds the inventory); dynReplacer is its
	// compiled longest-first form, applied in String. Guarded by mu — Register
	// takes Lock, String takes RLock.
	mu          sync.RWMutex
	dynamic     map[string]struct{}
	dynReplacer *strings.Replacer
}

type patternRule struct {
	re   *regexp.Regexp
	repl string
}

// patternRules are the Tier-B heuristics. Each value-capturing class deliberately
// EXCLUDES the JSON structural characters " ' , } and backslash so a match can
// never cross a JSON string boundary — Bytes redacts string leaves directly, but
// String is also applied to raw tool-result text, and keeping matches inside the
// token guarantees we never corrupt surrounding structure. The classes also
// exclude [ and ] so a Tier-A [redacted:NAME] marker (Tier A runs first) is
// inert to Tier B — the tiers compose instead of clobbering each other's output.
var patternRules = []patternRule{
	// HTTP auth header — the F32 repro shape (token/bearer inlined on a curl
	// cmdline). Keep the scheme, drop the credential.
	{regexp.MustCompile(`(?i)(Authorization\s*:\s*(?:token|Bearer)\s+)[^\s"',}\[\]\\]+`), "${1}[redacted]"},
	// OpenAI-style keys: sk- (incl. project keys sk-proj-…) + >=16 of base62/-/_.
	// Distinctive prefix → low FP.
	{regexp.MustCompile(`\bsk-[A-Za-z0-9][A-Za-z0-9_-]{15,}\b`), "[redacted-key]"},
	// AWS access key id: AKIA + 16 upper/digit. Fixed, unmistakable shape.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[redacted-aws-key]"},
	// Slack tokens: xoxb-/xoxp-/xoxa-/xoxr-/xoxs- prefix.
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), "[redacted-slack]"},
	// GitHub PAT: ghp_ + >=36 base62. Distinctive prefix. We do NOT match a bare
	// 40-hex string — that collides with git commit SHAs (a real false positive
	// in dev transcripts); env-sourced 40-hex PATs are caught by Tier A and
	// inline ones by the Authorization rule above.
	{regexp.MustCompile(`\bghp_[A-Za-z0-9]{36,}\b`), "[redacted-token]"},
	// key=value / key: "value" assignments for secret-named keys (the *_API_KEY=…
	// family). Masks only the value; the key + delimiter are preserved so the
	// structure stays legible.
	{regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password)\b(\s*["']?\s*[:=]\s*["']?)[^\s"',}\[\]\\]+`), "${1}${2}[redacted]"},
}

// New builds a Redactor. secrets maps an env-var NAME to its secret VALUE; each
// value is masked as [redacted:NAME] on an exact match (values shorter than
// minSecretLen are skipped). withPatterns enables the Tier-B heuristics.
// New(nil, false) is a no-op Redactor, so callers can build unconditionally.
func New(secrets map[string]string, withPatterns bool) *Redactor {
	r := &Redactor{}
	// Tier A: exact env-value replacer, LONGEST value first so a value that is a
	// substring of another is replaced as part of the longer match first (avoids
	// a partial mask leaving a fragment of the longer secret behind).
	type kv struct{ name, val string }
	kvs := make([]kv, 0, len(secrets))
	for name, val := range secrets {
		if len(val) < minSecretLen {
			continue
		}
		kvs = append(kvs, kv{name, val})
	}
	if len(kvs) > 0 {
		sort.Slice(kvs, func(i, j int) bool { return len(kvs[i].val) > len(kvs[j].val) })
		pairs := make([]string, 0, len(kvs)*2)
		for _, e := range kvs {
			pairs = append(pairs, e.val, "[redacted:"+e.name+"]")
		}
		r.replacer = strings.NewReplacer(pairs...)
	}
	if withPatterns {
		r.patterns = patternRules
	}
	return r
}

// Register adds a runtime-resolved secret VALUE so a later tool_call/tool_result
// that echoes it is masked in the persisted transcript (RFC AV/AR). Thread-safe
// and safe to call concurrently with String/Bytes. Deduped by value (re-
// registering is a no-op) so the set stays bounded by the distinct-credential
// inventory, not per call. Values shorter than minDynamicSecretLen and a nil
// redactor are ignored.
func (r *Redactor) Register(value string) {
	if r == nil || len(value) < minDynamicSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.dynamic[value]; dup {
		return // already registered — keep the set bounded
	}
	if r.dynamic == nil {
		r.dynamic = make(map[string]struct{})
	}
	r.dynamic[value] = struct{}{}
	// Rebuild the replacer longest-value-first so a value that is a substring of
	// another is masked as part of the longer match first (mirrors New's Tier A).
	vals := make([]string, 0, len(r.dynamic))
	for v := range r.dynamic {
		vals = append(vals, v)
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	pairs := make([]string, 0, len(vals)*2)
	for _, v := range vals {
		pairs = append(pairs, v, dynamicMarker)
	}
	r.dynReplacer = strings.NewReplacer(pairs...)
}

// Enabled reports whether the redactor would mask anything. A no-op / nil
// redactor returns false, letting callers skip the copy on the hot path. When
// static tiers are absent it consults the runtime-registered set under RLock;
// the common case (patterns on) short-circuits before the lock.
func (r *Redactor) Enabled() bool {
	if r == nil {
		return false
	}
	if r.replacer != nil || len(r.patterns) > 0 {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dynReplacer != nil
}

// String returns s with every recognised secret masked. Tier A (exact env
// values) runs first, then the Tier-B patterns, then the runtime-registered
// values (Tier A').
func (r *Redactor) String(s string) string {
	if !r.Enabled() || s == "" {
		return s
	}
	if r.replacer != nil {
		s = r.replacer.Replace(s)
	}
	for _, p := range r.patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	r.mu.RLock()
	dyn := r.dynReplacer
	r.mu.RUnlock()
	if dyn != nil {
		s = dyn.Replace(s)
	}
	return s
}

// Bytes redacts a JSON tool input (json.RawMessage). It walks the JSON and
// redacts every string LEAF via String, then re-marshals — so the output is
// always valid JSON (re-marshal handles escaping) and the secret, which is most
// often a bare string value (a token inlined in a Bash command string), is
// masked without risk of breaking structure. A non-JSON input (should not occur
// for tool inputs) is returned unchanged rather than risk corrupting the event.
func (r *Redactor) Bytes(b []byte) []byte {
	if !r.Enabled() || len(b) == 0 {
		return b
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b // not the expected JSON shape — leave it rather than corrupt it
	}
	out, err := json.Marshal(r.redactValue(v))
	if err != nil {
		return b // never drop the event over a redaction failure
	}
	return out
}

// redactValue walks a decoded JSON value, redacting string leaves in place.
// Object KEYS are not redacted (a key named "password" is not itself a secret;
// its string value is redacted as a leaf).
func (r *Redactor) redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return r.String(t)
	case map[string]any:
		for k, val := range t {
			t[k] = r.redactValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = r.redactValue(val)
		}
		return t
	default:
		return v
	}
}
