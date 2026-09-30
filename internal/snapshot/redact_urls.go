package snapshot

import (
	"regexp"
	"strings"
)

// Restore warnings are read by people and written to logs and API responses
// that the envelope's own access control does not cover. They quote the
// definitions they concern, and an authoring validator's error repeats the
// value it refused — so a refused base_url or endpoint such as
// https://user:pass@host/?token=... would carry its credential into the
// warning verbatim. redactURLSecrets removes the parts of any URL in a
// message that can hold a credential: the userinfo, the query and the
// fragment. The scheme, host and path stay, so the warning still says which
// endpoint it means.

// urlInTextRe finds scheme://... runs in free text. The run ends at white
// space or a quote, which is where a %q- or %v-formatted URL ends in an error
// message.
var urlInTextRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>` + "`" + `]*`)

// redactedURLPart replaces a removed URL part.
const redactedURLPart = "REDACTED"

// redactURLSecrets returns msg with the userinfo, query and fragment of every
// URL in it replaced by REDACTED.
func redactURLSecrets(msg string) string {
	if !strings.Contains(msg, "://") {
		return msg
	}
	return urlInTextRe.ReplaceAllStringFunc(msg, redactURL)
}

func redactURL(u string) string {
	i := strings.Index(u, "://")
	scheme, rest := u[:i+3], u[i+3:]
	var tail string
	if j := strings.IndexAny(rest, "?#"); j >= 0 {
		// Everything from the first ? or # on is query or fragment.
		tail = rest[j:j+1] + redactedURLPart
		rest = rest[:j]
	}
	// Userinfo is what precedes the last @ before the query. That also
	// catches a credential a gRPC-style target spells after its slashes
	// (dns:///user:pass@host); an @ that is really in a path over-redacts,
	// which costs a warning nothing.
	if k := strings.LastIndex(rest, "@"); k >= 0 {
		rest = redactedURLPart + rest[k:]
	}
	return scheme + rest + tail
}

// redactWarnings applies redactURLSecrets to every warning.
func redactWarnings(ws []string) []string {
	for i, w := range ws {
		ws[i] = redactURLSecrets(w)
	}
	return ws
}
