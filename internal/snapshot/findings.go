package snapshot

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// Capture findings (RFC DP §3.2 e).
//
// A header map in a definition body may hold a literal credential where a
// reference ($cred:<name>, ${LOOMCYCLE_*}) was meant. The owner decided such a
// value travels AS AUTHORED — stripping it would break the definition, and the
// capture is not refused — but it is REPORTED, so the operator can find it and
// replace it with a reference. It is the one place a secret value can knowingly
// reach an envelope.
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
	// detectorPendingInterrupt: a paused run the envelope carries has a
	// pending interrupt, which no section carries (see
	// pausedRunInterruptFindings). Not a header finding: Field is
	// "interrupts".
	detectorPendingInterrupt = "pending-interrupt"
)

// referenceRe matches the reference forms a header value may use instead of a
// literal: ${...} (an env or run placeholder, expanded at dial or call time),
// and $cred:<name> / $ghapp:<name> (resolved per run from the credential
// store). The name charset mirrors internal/credential's token grammar.
var referenceRe = regexp.MustCompile(`\$\{[^}]*\}|\$(?:cred|ghapp):[A-Za-z0-9_-]{1,128}`)

// authSchemes are the words that may precede a credential in an
// Authorization-style value. A value that is only a scheme around a reference
// ("Bearer ${LOOMCYCLE_X}") holds no literal.
var authSchemes = map[string]bool{"bearer": true, "basic": true, "token": true, "digest": true, "apikey": true}

// secretPatterns is the redact package's Tier-B heuristics alone: no env
// values are loaded, so a match is about the value's shape, never about what
// this process happens to hold.
var secretPatterns = redact.New(nil, true)

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
	line := name + ": " + literal
	if secretPatterns.String(line) != line {
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

// literalMaps maps the key of a string map a body may hold to the detector
// its entries are judged by.
var literalMaps = map[string]func(name, value string) string{
	"headers": headerDetector,
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
								out = append(out, CaptureFindingEntry{
									Section: sub.section, TenantID: sub.tenantID, Name: sub.name,
									DefID: sub.defID, Field: p + "." + n, Detector: d,
								})
							}
						}
						continue
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

// collectCaptureFindings scans every header-bearing body the envelope carries.
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
	// Memory-backend and document-source bodies hold no header map today;
	// they are walked so one added later is scanned without anyone
	// remembering to add it here.
	for _, e := range sec.MemoryBackendDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"memory_backend_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
	}
	for _, e := range sec.DocSourceDefs.Entries {
		out = append(out, scanLiterals(findingSubject{"document_source_defs", e.TenantID, e.Name, e.DefID}, "", e.Definition)...)
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
	return fmt.Sprintf("%s: %s holds a literal value that looks like a credential (%s); it travels in the snapshot as written — replace it with a $cred: or ${LOOMCYCLE_*} reference",
		where, f.Field, f.Detector)
}
