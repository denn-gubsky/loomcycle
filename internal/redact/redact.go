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
	// Tier C: mask signed-URL credential query values. Set by
	// WithSignedURLParams, never by New, so only an at-rest redactor has it.
	signedURLs bool

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
//
// The whitespace a rule allows between a key and its value is [ \t], never \s:
// no rule may match across a line break onto a line that starts at column 0. Untrusted text reaches the model as
// "<kind>\n…\n</kind>" and the redact context plugin runs over it after that
// fence is built; a separator that crossed the newline turned a body ending
// "password=" into "password=\n[redacted]" — the closing tag consumed as the
// value, leaving the block open over everything after it.
var patternRules = []patternRule{
	// HTTP auth header — the F32 repro shape (token/bearer inlined on a curl
	// cmdline). Keep the scheme, drop the credential.
	{regexp.MustCompile(`(?i)(Authorization[ \t]*:[ \t]*(?:token|Bearer)[ \t]+)[^\s"',}\[\]\\]+`), "${1}[redacted]"},
	// OpenAI-style keys: sk- (incl. project keys sk-proj-…) + >=16 of base62/-/_.
	// Distinctive prefix → low FP.
	{regexp.MustCompile(`\bsk-[A-Za-z0-9][A-Za-z0-9_-]{15,}\b`), "[redacted-key]"},
	// AWS access key id: AKIA + 16 upper/digit. Fixed, unmistakable shape.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[redacted-aws-key]"},
	// Slack tokens: xoxb-/xoxp-/xoxa-/xoxr-/xoxs- prefix.
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), "[redacted-slack]"},
	// GitHub tokens: ghp_ (personal), gho_ (OAuth), ghu_ (user-to-server),
	// ghs_ (an App installation's, which is what a git source clones with),
	// ghr_ (refresh) + >=36 base62. Distinctive prefixes. We do NOT match a
	// bare 40-hex string — that collides with git commit SHAs (a real false
	// positive in dev transcripts); env-sourced 40-hex PATs are caught by
	// Tier A and inline ones by the Authorization rule above.
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), "[redacted-token]"},
	// GitHub fine-grained personal token: github_pat_ + base62 and _.
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{50,}\b`), "[redacted-token]"},
	// GitLab personal, project and group token: glpat- + base62, - and _.
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`), "[redacted-token]"},
	// key=value / key: "value" assignments for secret-named keys (the *_API_KEY=…
	// family). Masks only the value; the key + delimiter are preserved so the
	// structure stays legible.
	// The value may sit on the next line when that line is indented (YAML's
	// `password:\n  value`): a fence tag always starts its line, so an
	// indented continuation can never be one.
	{regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password)\b([ \t]*["']?[ \t]*[:=](?:[ \t]*\r?\n[ \t]+|[ \t]*)["']?)[^\s"',}\[\]\\]+`), "${1}${2}[redacted]"},
}

// signedURLMarker replaces the value of a signed-URL credential parameter.
// The parameter name stays, so a stored transcript still shows which URL it
// was and what kind of credential it carried.
const signedURLMarker = "[redacted:signed-url]"

// signedParamRe is Tier C: a query parameter whose NAME marks a credential
// that grants access by itself — a pre-signed URL works for anyone holding
// it, with no sign-in — followed by its value. Only the value group is
// replaced.
//
// The parameter must open a query component: after ? & # ; or their stored
// encodings (\u0026 as Go's JSON writes &, %3F and %26 inside another URL;
// &amp; ends in ;). So the same word in prose, or a key=value pair outside a
// URL, is left alone — the name list includes generic words (token, code)
// that are credentials only in a URL. # covers an implicit-grant redirect
// (#access_token=…).
//
// The value ends only where a query value or the string holding the URL
// ends: & (also the start of &amp;), #, whitespace, a quote, < or >, a
// backslash (\u0026, and an escaped quote, so the mask never runs past the
// end of a JSON string), and the escape %26 — any other %XX, such as %2B in
// an Azure signature, stays inside the value. Any other character, however
// unusual in a value (| ; , ( ) [ ] { }), is masked with it: stopping there
// would store the rest of the value in clear, and the pattern tier that
// runs next cannot take it either, since it does not match after a marker.
//
// Go's regexp is linear-time (RE2), so no input can make this backtrack.
var signedParamRe = regexp.MustCompile(`(?i)(?:[?&#;]|\\u0026|%3f|%26)` +
	`(` + strings.Join(signedNames, "|") + `)` +
	`(?:=|%3d)` +
	`((?:[^%&\s"'<>\\#]|%[^2%&\s"'<>\\#]|%2[^6&\s"'<>\\#])+)`)

// signedCompanion holds the names that are a credential only beside another
// parameter in the same URL. "Signature" is generic; CloudFront's is the one
// next to Key-Pair-Id or Policy. "code" is an OAuth authorization code only
// in a redirect that also carries state; elsewhere ?code= is a country,
// promo or error code, and masking it would cost a later turn the URL.
var signedCompanion = map[string]*regexp.Regexp{
	"signature": regexp.MustCompile(`(?i)(?:[?&#;]|\\u0026|%3f|%26)(?:key-pair-id|policy)(?:=|%3d)`),
	"code":      regexp.MustCompile(`(?i)(?:[?&#;]|\\u0026|%3f|%26)state(?:=|%3d)`),
}

// urlStop ends the URL a companion parameter is looked for in.
const urlStop = " \t\r\n\"'<>`"

// signedNames are the parameter names Tier C masks: signedParamRe's name
// group and the prefilter's list. Regex-safe as written (no metacharacters).
var signedNames = []string{
	"tempauth", "sig", "x-amz-signature", "x-amz-credential", "x-amz-security-token",
	"x-goog-signature", "x-goog-credential", "access_token", "token", "code", "signature",
}

// maxSignedNameLen is the longest name in signedNames.
const maxSignedNameLen = len("x-amz-security-token")

// hasSignedParamCandidate reports whether some = or %3D in s directly
// follows a name in signedNames. Every signedParamRe match has one, so a
// false here skips the regex; it runs on every persisted string, and most
// hold no such name. It is a suffix test, not an exact one, because an
// encoded separator ends in name bytes (\u0026tempauth=, %26sig=).
func hasSignedParamCandidate(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '=' && (s[i] != '%' || i+2 >= len(s) || s[i+1] != '3' || s[i+2]|0x20 != 'd') {
			continue
		}
		j := i
		for j > 0 && i-j < maxSignedNameLen && isParamNameByte(s[j-1]) {
			j--
		}
		run := s[j:i]
		for _, name := range signedNames {
			if len(run) >= len(name) && strings.EqualFold(run[len(run)-len(name):], name) {
				return true
			}
		}
	}
	return false
}

func isParamNameByte(c byte) bool {
	return c == '-' || c == '_' || ('0' <= c && c <= '9') || ('a' <= c|0x20 && c|0x20 <= 'z')
}

// maskSignedURLParams applies Tier C to s.
func maskSignedURLParams(s string) string {
	if !hasSignedParamCandidate(s) {
		return s
	}
	ms := signedParamRe.FindAllStringSubmatchIndex(s, -1)
	if ms == nil {
		return s
	}
	var b strings.Builder
	last := 0
	// The URL around a match, and each companion verdict in it, is found once
	// and reused for the matches after it in the same URL: a long URL with
	// many matches costs one scan, not one per match.
	type verdict struct {
		re *regexp.Regexp
		ok bool
	}
	var verdicts []verdict
	urlLo, urlHi := -1, -1
	for _, m := range ms {
		if companion := signedCompanion[strings.ToLower(s[m[2]:m[3]])]; companion != nil {
			if m[0] >= urlHi {
				urlLo = strings.LastIndexAny(s[:m[0]], urlStop) + 1
				urlHi = len(s)
				if i := strings.IndexAny(s[m[1]:], urlStop); i >= 0 {
					urlHi = m[1] + i
				}
				verdicts = verdicts[:0]
			}
			ok, seen := false, false
			for _, v := range verdicts {
				if v.re == companion {
					ok, seen = v.ok, true
					break
				}
			}
			if !seen {
				ok = companion.MatchString(s[urlLo:urlHi])
				verdicts = append(verdicts, verdict{companion, ok})
			}
			if !ok {
				continue
			}
		}
		// A value an exact tier already masked keeps that marker, and so
		// does one this pass masked before (the pass is idempotent).
		if v := s[m[4]:m[5]]; strings.HasPrefix(v, "[redacted") && strings.IndexByte(v, ']') == len(v)-1 {
			continue
		}
		b.WriteString(s[last:m[4]])
		b.WriteString(signedURLMarker)
		last = m[5]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// MatchesPattern reports whether any Tier-B heuristic matches s. It is the
// stateless question a scanner asks about a value's SHAPE ("does this look
// like a credential?") without building a Redactor, which carries the
// runtime-registered set and its lock.
func MatchesPattern(s string) bool {
	for _, p := range patternRules {
		if p.re.MatchString(s) {
			return true
		}
	}
	return false
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

// WithSignedURLParams turns on Tier C and returns r.
func (r *Redactor) WithSignedURLParams() *Redactor {
	if r != nil {
		r.signedURLs = true
	}
	return r
}

// Enabled reports whether the redactor would mask anything. A no-op / nil
// redactor returns false, letting callers skip the copy on the hot path. When
// static tiers are absent it consults the runtime-registered set under RLock;
// the common case (patterns on) short-circuits before the lock.
func (r *Redactor) Enabled() bool {
	if r == nil {
		return false
	}
	if r.replacer != nil || len(r.patterns) > 0 || r.signedURLs {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dynReplacer != nil
}

// String returns s with every recognised secret masked. The exact tiers run
// first — Tier A (env values), then the runtime-registered values (Tier
// A') — then Tier C (signed-URL parameters) and the Tier-B patterns.
func (r *Redactor) String(s string) string {
	if !r.Enabled() || s == "" {
		return s
	}
	// The exact tiers run first: a heuristic that rewrote part of a known
	// value would leave the exact match nothing to find, and the rest of the
	// value would be stored in clear.
	if r.replacer != nil {
		s = r.replacer.Replace(s)
	}
	r.mu.RLock()
	dyn := r.dynReplacer
	r.mu.RUnlock()
	if dyn != nil {
		s = dyn.Replace(s)
	}
	// Before the patterns: their key=value rule would otherwise take a URL's
	// token= value to the end of the URL, parameters after it included.
	if r.signedURLs {
		s = maskSignedURLParams(s)
	}
	for _, p := range r.patterns {
		s = p.re.ReplaceAllString(s, p.repl)
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
