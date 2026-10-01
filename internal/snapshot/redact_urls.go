package snapshot

import (
	"regexp"
	"strconv"
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

// quotedURLRe finds a Go %q-quoted string that starts with scheme://. The
// quotes, not white space, bound such a URL: the URLs validators refuse are
// exactly the ones holding a space, a ' or an escaped " — in the userinfo,
// say — and urlInTextRe alone would stop there and leave the rest of the
// credential in the message.
var quotedURLRe = regexp.MustCompile(`"[A-Za-z][A-Za-z0-9+.-]*://(?:[^"\\]|\\.)*"`)

// urlInTextRe finds scheme://... runs in free text. The run ends at white
// space or a quote, which is where a %v-formatted URL ends in an error
// message.
var urlInTextRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>` + "`" + `]*`)

// redactedURLPart replaces a removed URL part.
const redactedURLPart = "REDACTED"

// redactURLSecrets returns msg with the userinfo, query and fragment of every
// URL in it replaced by REDACTED. A quoted URL is redacted whole first; the
// unquoted pass then leaves it as it is, since redactURL is idempotent.
func redactURLSecrets(msg string) string {
	if !strings.Contains(msg, "://") {
		return msg
	}
	msg = quotedURLRe.ReplaceAllStringFunc(msg, func(q string) string {
		u, err := strconv.Unquote(q)
		if err != nil {
			return q // not a Go-quoted string after all; the unquoted pass takes it
		}
		return strconv.Quote(redactURL(u))
	})
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
