package builtin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// The requests that move a file — File op=download and op=upload, WebFetch's
// side-downloaded assets — go out on the HTTP tool's policy: its host check
// (the operator's list as narrowed for the run, or a permitted hook's grant)
// and its guarded dialer (private, loopback and metadata addresses refused
// unless the operator exempted the host). Only the redirect rule and the
// timeout differ by caller.

// redirectRule is what a transfer does with a redirect.
type redirectRule int

const (
	// redirectAllowlisted follows a redirect to any permitted host, as the
	// HTTP tool does.
	redirectAllowlisted redirectRule = iota
	// redirectSameHost follows a redirect only to the same host, never from
	// https down to http.
	redirectSameHost
	// redirectNone does not follow; the 3xx is the answer.
	redirectNone
)

// transferClient is an http.Client on the HTTP tool's dialer. Its response
// header timeout is the HTTP tool's; the body may take longer, bounded by the
// caller's context. seen, when not nil, is shown every redirect target before
// it is judged: a Location often carries a token of its own.
func (h *HTTP) transferClient(ctx context.Context, rule redirectRule, seen func(*url.URL)) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           h.dialContext,
			ResponseHeaderTimeout: h.timeout(),
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if seen != nil {
				seen(req.URL)
			}
			if rule == redirectNone {
				return http.ErrUseLastResponse
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if rule == redirectSameHost {
				first := via[0].URL
				if !strings.EqualFold(req.URL.Hostname(), first.Hostname()) {
					return fmt.Errorf("redirect to another host (%s) refused", req.URL.Hostname())
				}
				if first.Scheme == "https" && req.URL.Scheme != "https" {
					return errors.New("redirect from https to http refused")
				}
			}
			if !h.hostPermitted(ctx, req.URL.Hostname()) {
				return fmt.Errorf("redirect host %q not in allowlist", req.URL.Hostname())
			}
			return nil
		},
	}
}

// parseTransferURL parses an absolute http(s) URL. A URL carrying a user and
// password is refused: a credential goes in a pre-authenticated URL, not
// beside it. The host is checked by the caller, against hostPermitted.
func parseTransferURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if err := checkURLChars(raw); err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("url must be an absolute http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("url scheme must be http or https")
	}
	if u.User != nil {
		return nil, errors.New("url must not carry a user or password")
	}
	return u, nil
}

// checkURLChars refuses a character a URL never holds unencoded. url.Parse
// lets these through in a path or query, and each one is a copying slip:
// a model that takes an upload session's uploadUrl from JSON may keep the
// string's closing quote, and SharePoint then answers 401 "invalid
// signature", which reads as an expired session rather than a typo. Naming
// the character lets the call be repeated with the same, still valid, URL.
// The error never repeats the URL: its query may be the credential.
func checkURLChars(s string) error {
	for i, r := range s {
		var name string
		switch {
		case r == '"':
			name = `a double quote (")`
		case r == '<' || r == '>' || r == '`':
			name = fmt.Sprintf("%q", r)
		case r == '\\':
			name = "a backslash"
		case r == ' ':
			name = "a space"
		case r < 0x20 || r == 0x7f:
			name = "a control character"
		default:
			continue
		}
		return fmt.Errorf("url contains %s at byte %d of %d, which a URL never holds unencoded; pass the URL value alone, without the quote marks around it in JSON or anything after it", name, i+1, len(s))
	}
	return nil
}

// transferError renders a request error with its URL masked. net/http's
// *url.Error quotes the whole URL, query and all — the request's, or a
// redirect target's — and a pre-authenticated URL's query is its credential.
func transferError(err error) string {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err.Error()
	}
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		return fmt.Sprintf("%s %q: %v", ue.Op, displayURL(u), ue.Err)
	}
	return fmt.Sprintf("%s: %v", ue.Op, ue.Err)
}

// displayURL is a URL as results and logs show it: the query, which in a
// pre-authenticated URL is the credential, reads "?…", and the fragment is
// dropped.
func displayURL(u *url.URL) string {
	v := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	s := v.String()
	if u.RawQuery != "" || u.ForceQuery {
		s += "?…"
	}
	return s
}

// urlQueryRe finds the query of an http(s) URL in free text.
var urlQueryRe = regexp.MustCompile(`(https?://[^\s"'<>?]*)\?[^\s"'<>]*`)

// redactURLQueries replaces the query of every URL in s with "?…": a reply
// from an upload endpoint can carry fresh pre-authenticated URLs (Microsoft
// Graph returns the item's download URL).
func redactURLQueries(s string) string {
	return urlQueryRe.ReplaceAllString(s, "${1}?…")
}

// requestHeaderReserved are headers a transfer sets itself.
var requestHeaderReserved = map[string]bool{
	"Host": true, "Content-Length": true, "Content-Range": true, "Transfer-Encoding": true,
	"Connection": true, "Te": true, "Trailer": true, "Upgrade": true, "Expect": true,
}

// checkTransferHeaders refuses a header that carries, or looks like it
// carries, a credential: the request headers of a download or upload are
// written by the model and land in the transcript. A pre-authenticated URL is
// how such a request is authorised.
func checkTransferHeaders(headers map[string]string) error {
	for k, v := range headers {
		ck := http.CanonicalHeaderKey(strings.TrimSpace(k))
		if ck == "" || strings.ContainsAny(k, "\r\n:") || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %q is not a valid header", k)
		}
		if requestHeaderReserved[ck] {
			return fmt.Errorf("header %s is set by the runtime", ck)
		}
		if ck == "Authorization" || ck == "Proxy-Authorization" || ck == "Cookie" || credentialHeader(ck) || credentialValue(v) {
			return fmt.Errorf("header %s looks like a credential, which is not accepted here", ck)
		}
	}
	return nil
}

// credentialValueRe matches a value that is an authorisation scheme's
// credential whatever header it is under.
var credentialValueRe = regexp.MustCompile(`(?i)^\s*(bearer|basic|digest|negotiate|ntlm|token)\s+\S`)

func credentialValue(v string) bool {
	return credentialValueRe.MatchString(v) || redact.MatchesPattern(v)
}
