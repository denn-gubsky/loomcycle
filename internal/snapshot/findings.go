package snapshot

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// Capture findings (RFC DP §3.2 e).
//
// A definition body may hold a literal credential where a reference
// ($cred:<name>, ${LOOMCYCLE_*}) was meant: in a header map, in an MCP server
// def's stdio env map, command or args, or in a URL-shaped field (an MCP
// server's url, a memory backend's or document source's config.base_url, an
// A2A peer's agent_card_url or endpoint) as userinfo or a credential-shaped
// query parameter. The owner decided such a value travels AS AUTHORED —
// stripping it would break the definition, and the capture is not refused —
// but it is REPORTED, so the operator can find it and replace it with a
// reference. These are the places a secret value can knowingly reach an
// envelope, and every one of them is reported.
//
// A finding names the LOCATION only. It never records the value, a prefix, its
// length or a hash of it: the report is read by people and written to logs that
// the envelope's own access control does not cover.

// Detector names a finding carries.
const (
	// detectorSecretPattern: the redact package's secret-shape heuristics
	// matched the header line.
	detectorSecretPattern = "secret-pattern"
	// detectorCredentialHeader: the header's NAME says it carries a
	// credential (Authorization, *-Token, *-Key, ...) and its value holds a
	// literal beyond an auth scheme word.
	detectorCredentialHeader = "credential-header-name"
	// detectorCredentialEnv: an env entry's NAME says it carries a
	// credential (*_KEY, *_TOKEN, ...) and its value holds a literal.
	detectorCredentialEnv = "credential-env-name"
	// detectorPendingInterrupt: a paused run the envelope carries has a
	// pending interrupt, which no section carries (see
	// pausedRunInterruptFindings). Not a header finding: Field is
	// "interrupts".
	detectorPendingInterrupt = "pending-interrupt"
	// detectorURLUserinfo: a URL-shaped field's userinfo holds a literal
	// password, or a user name with a secret's shape.
	detectorURLUserinfo = "url-userinfo"
	// detectorURLCredentialQuery: a URL-shaped field has a query parameter
	// named like a credential (credentialQueryNames) with a literal value.
	detectorURLCredentialQuery = "url-credential-query"
)

// credentialQueryNames are the query parameter names, lower-cased, that carry
// a credential in a URL.
var credentialQueryNames = map[string]bool{
	"key": true, "api_key": true, "apikey": true, "token": true, "access_token": true,
	"secret": true, "password": true, "auth": true, "sig": true, "signature": true,
}

// urlField reports whether a body key holds a URL: an MCP server def's or an
// inline webhook's url, a memory backend's or document source's
// config.base_url, an A2A peer's agent_card_url or endpoint — any *_url.
func urlField(key string) bool {
	return key == "url" || key == "endpoint" || strings.HasSuffix(key, "_url")
}

// urlDetectors returns the detectors a URL value trips, at most one of each.
// References are stripped first, so "https://u:${LOOMCYCLE_P}@h" and
// "?api_key=${LOOMCYCLE_K}" hold no literal. A value that does not parse as
// an absolute URL is not judged: nothing dials it.
func urlDetectors(value string) []string {
	s := strings.TrimSpace(referenceRe.ReplaceAllString(value, ""))
	if !strings.Contains(s, "://") {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	var out []string
	if u.User != nil {
		if pw, ok := u.User.Password(); (ok && pw != "") || redact.MatchesPattern(u.User.Username()) {
			out = append(out, detectorURLUserinfo)
		}
	}
	for name, vals := range u.Query() {
		if !credentialQueryNames[strings.ToLower(name)] {
			continue
		}
		if slices.ContainsFunc(vals, func(v string) bool { return strings.TrimSpace(v) != "" }) {
			out = append(out, detectorURLCredentialQuery)
			break
		}
	}
	return out
}

// secretShaped reports whether a stdio command or args element holds a
// literal with a secret's shape once its references are stripped.
func secretShaped(value string) bool {
	literal := strings.TrimSpace(referenceRe.ReplaceAllString(value, ""))
	return literal != "" && redact.MatchesPattern(literal)
}

// referenceRe matches the reference forms a header value may use instead of a
// literal: ${...} (an env or run placeholder, expanded at dial or call time),
// and $cred:<name> / $ghapp:<name> (resolved per run from the credential
// store). The name charset mirrors internal/credential's token grammar.
var referenceRe = regexp.MustCompile(`\$\{[^}]*\}|\$(?:cred|ghapp):[A-Za-z0-9_-]{1,128}`)

// authSchemes are the words that may precede a credential in an
// Authorization-style value. A value that is only a scheme around a reference
// ("Bearer ${LOOMCYCLE_X}") holds no literal.
var authSchemes = map[string]bool{"bearer": true, "basic": true, "token": true, "digest": true, "apikey": true}

// credentialHeaderName reports whether a header's name says it carries a
// credential.
func credentialHeaderName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "authorization", "proxy-authorization", "cookie", "x-api-key", "api-key", "apikey":
		return true
	}
	return strings.HasSuffix(n, "-token") || strings.HasSuffix(n, "-key") ||
		strings.HasSuffix(n, "-secret") || strings.HasSuffix(n, "-password") ||
		strings.HasSuffix(n, "_token") || strings.HasSuffix(n, "_key")
}

// literalDetector returns the detector a name/value entry trips, or "" when
// the value is a reference (or a scheme around one) or looks like no
// credential. credentialName says whether the entry's name marks it as a
// credential; nameDetector is the detector that rule reports.
func literalDetector(name, value string, credentialName func(string) bool, nameDetector string) string {
	literal := strings.TrimSpace(referenceRe.ReplaceAllString(value, ""))
	if literal == "" {
		return ""
	}
	// The redact package's Tier-B heuristics alone: a match is about the
	// value's shape, never about what this process happens to hold.
	if redact.MatchesPattern(name + ": " + literal) {
		return detectorSecretPattern
	}
	if !credentialName(name) {
		return ""
	}
	for _, w := range strings.Fields(literal) {
		if !authSchemes[strings.ToLower(w)] {
			return nameDetector
		}
	}
	return ""
}

// headerDetector returns the detector a header trips.
func headerDetector(name, value string) string {
	return literalDetector(name, value, credentialHeaderName, detectorCredentialHeader)
}

// credentialEnvName reports whether an env var's name says it carries a
// credential, by the repo's secret-name convention.
func credentialEnvName(name string) bool {
	n := strings.ToUpper(strings.TrimSpace(name))
	for _, suffix := range []string{"_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_AUTH", "_CREDENTIAL"} {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

// envDetector returns the detector an env entry (an MCP server def's stdio
// env) trips.
func envDetector(name, value string) string {
	return literalDetector(name, value, credentialEnvName, detectorCredentialEnv)
}

// literalMaps maps the key of a string map a body may hold to the detector
// its entries are judged by.
var literalMaps = map[string]func(name, value string) string{
	"headers": headerDetector,
	"env":     envDetector,
}

// isEnvField reports whether a finding's field is an env entry. Assumes no
// header name contains ".env." — a misread only changes the advice wording.
func isEnvField(field string) bool {
	return strings.HasPrefix(field, "env.") || strings.Contains(field, ".env.")
}

// findingSubject says which row a body belongs to.
type findingSubject struct {
	section  string
	tenantID string
	name     string
	defID    string
}

// scanLiterals walks a JSON body and returns a finding for every entry of a
// literalMaps map that looks like a literal credential. A header map is any
// object under a "headers" key whose values are all strings — the shape of an MCP server
// def's headers, an http hook body's headers and an inline hook's headers,
// wherever they are nested (an agent's hooks and tool_hooks, a team state's
// handler, a channel's hooks, a run's recorded hooks). Walking the shape
// rather than decoding each definition type means a hook block added to a
// definition later is scanned without anyone remembering to add it here.
// An env map is any object under an "env" key whose values are all strings —
// in def bodies today, only an MCP server def's stdio env. The same walk
// judges every string under a URL-shaped key (urlField) for userinfo and
// credential-shaped query parameters, and every string under a "command" key
// or in an "args" array for a secret's shape.
//
// prefix is the path of the body itself ("" for a whole definition, "hooks"
// for a channel's hooks column). The walk is deterministic: keys sorted.
func scanLiterals(sub findingSubject, prefix string, body json.RawMessage) []CaptureFindingEntry {
	if len(body) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil // not ours to judge; the section carries it as authored
	}
	var out []CaptureFindingEntry
	add := func(field, detector string) {
		out = append(out, CaptureFindingEntry{
			Section: sub.section, TenantID: sub.tenantID, Name: sub.name,
			DefID: sub.defID, Field: field, Detector: detector,
		})
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := joinPath(path, k)
				if detect, ok := literalMaps[k]; ok {
					if entries, ok := stringMap(t[k]); ok {
						names := make([]string, 0, len(entries))
						for n := range entries {
							names = append(names, n)
						}
						sort.Strings(names)
						for _, n := range names {
							if d := detect(n, entries[n]); d != "" {
								add(p+"."+n, d)
							}
						}
						continue
					}
				}
				switch val := t[k].(type) {
				case string:
					if urlField(k) {
						for _, d := range urlDetectors(val) {
							add(p, d)
						}
					} else if k == "command" && secretShaped(val) {
						add(p, detectorSecretPattern)
					}
				case []any:
					if k == "args" {
						for i, e := range val {
							if s, ok := e.(string); ok && secretShaped(s) {
								add(fmt.Sprintf("%s[%d]", p, i), detectorSecretPattern)
							}
						}
					}
				}
				walk(p, t[k])
			}
		case []any:
			for i, e := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk(prefix, v)
	return out
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// stringMap returns v as a map of strings when it is an object whose values
// are all strings.
func stringMap(v any) (map[string]string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, e := range m {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[k] = s
	}
	return out, true
}

// collectCaptureFindings scans every definition body the envelope carries
// that can hold a literal credential.
// Runs over the captured sections rather than the store, so it scans exactly
// what the envelope holds.
func collectCaptureFindings(sec *Sections) []CaptureFindingEntry {
	var out []CaptureFindingEntry
	for _, e := range sec.AgentDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"agent_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	for _, e := range sec.TeamDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"team_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	for _, e := range sec.HookDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"hook_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	for _, e := range sec.MCPServerDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"mcp_server_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	// Memory-backend and document-source bodies hold no header map today,
	// but their config.base_url is judged as a URL, and a header map added
	// later is scanned without anyone remembering to add it here.
	for _, e := range sec.MemoryBackendDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"memory_backend_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	for _, e := range sec.DocSourceDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"document_source_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	// An A2A peer is dialed at its agent_card_url and endpoint.
	for _, e := range sec.A2AAgentDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"a2a_agent_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	for _, e := range sec.ChannelDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"channel_defs", e.TenantID, e.Name, ""}, "hooks", e.Hooks)...)
	}
	// A paused run's recorded configuration carries the hooks its caller added
	// and the hooks it resolved at start, inline webhook headers included.
	for _, e := range sec.PausedRuns.Entries {
		out = append(out, scanLiterals(findingSubject{"paused_runs", e.TenantID, e.RunID, ""}, "run_config", e.RunConfig)...)
	}
	return out
}

// Warning renders a finding for a capture response, a log line or a restore
// warning. Location only.
func (f CaptureFindingEntry) Warning() string {
	where := f.Section + " " + f.Name
	if f.TenantID != "" {
		where += " (tenant " + f.TenantID + ")"
	}
	if f.DefID != "" {
		where += " def " + f.DefID
	}
	if f.Detector == detectorPendingInterrupt {
		return fmt.Sprintf("%s: the paused run has a pending interrupt, which a snapshot does not carry — "+
			"on a restored copy nothing can answer it; resolve or cancel the interrupt and capture again", where)
	}
	switch {
	case f.Detector == detectorURLUserinfo || f.Detector == detectorURLCredentialQuery:
		// A URL is not where a credential belongs: move it to where the
		// definition sends one, rather than trading it for a reference that
		// would still ride in the URL (and in every log line that prints it).
		return fmt.Sprintf("%s: %s holds a literal credential in its URL (%s); it travels in the snapshot as written — "+
			"move it out of the URL, into a header or the definition's own credential setting, as a reference", where, f.Field, f.Detector)
	case isEnvField(f.Field) || isStdioLaunchField(f.Field):
		// A stdio server's env, command and args are expanded at spawn by
		// the allowlisted env expander only; nothing resolves a $cred:
		// there, so advising one would break the server.
		return fmt.Sprintf("%s: %s holds a literal value that looks like a credential (%s); it travels in the snapshot as written — replace it with a ${LOOMCYCLE_*} reference",
			where, f.Field, f.Detector)
	}
	return fmt.Sprintf("%s: %s holds a literal value that looks like a credential (%s); it travels in the snapshot as written — replace it with a $cred: or ${LOOMCYCLE_*} reference",
		where, f.Field, f.Detector)
}

// isStdioLaunchField reports whether a finding's field is a stdio server's
// command or an args element.
func isStdioLaunchField(field string) bool {
	return field == "command" || strings.HasSuffix(field, ".command") ||
		strings.HasPrefix(field, "args[") || strings.Contains(field, ".args[")
}
