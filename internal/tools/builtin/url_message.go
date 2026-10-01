package builtin

import (
	"errors"
	"net/url"
	"strings"
)

// urlForMessage renders a URL a validator refused, for its error message.
//
// Those errors reach tool results, logs and snapshot restore warnings, none of
// which the definition's own access control covers. Quoting the raw value
// with %q would carry a literal credential along with it: the userinfo, a
// credential-shaped query parameter, a fragment token. So the message gets
// the URL with the whole userinfo replaced (not only the password, as
// url.Redacted does: a token is often spelled as the user name), and the
// query and fragment dropped. The scheme, host and path stay, so the message
// still says which endpoint it means.
//
// An unparseable URL is not echoed at all: a *url.Error embeds the raw URL,
// so only its inner Err is reported.
func urlForMessage(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "<unparseable URL: " + err.Error() + ">"
	}
	if u.User != nil {
		u.User = url.User("REDACTED")
	}
	// A URL missing its authority parses the would-be userinfo into the
	// opaque part ("https:u:p@h") or the path ("https:///u:p@h", a gRPC-style
	// "dns:///u:p@h") — exactly the malformed values a validator refuses.
	// An @ that really is in a path over-redacts, which costs a message
	// nothing.
	if u.Opaque != "" {
		u.Opaque = "REDACTED"
	}
	if i := strings.LastIndex(u.Path, "@"); i >= 0 {
		u.Path, u.RawPath = "/REDACTED"+u.Path[i:], ""
	}
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}
