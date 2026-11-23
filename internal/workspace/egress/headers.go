package egress

import (
	"net/http"
	"net/textproto"
	"strings"
)

// hopByHop are the headers that describe one connection, not the message
// (RFC 9110 §7.6.1). Passed through, cargo's upgrade headers made its
// registry refuse every request in spike C.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// requestDeny are dropped from the session's request besides the hop-by-hop
// ones: the core sets Host and the credential itself, and a session may not
// pass on credentials, cookies or claims about where the request came from.
var requestDeny = []string{"Host", "Authorization", "Cookie", "Forwarded", "X-Real-Ip", "Content-Length"}

// responseDeny are dropped from the upstream's answer besides the hop-by-hop
// ones, so an API cannot hand the session a token or start an authentication
// exchange with it.
var responseDeny = []string{"Set-Cookie", "Set-Cookie2", "Www-Authenticate", "Authentication-Info", "Proxy-Authentication-Info", "Authorization"}

// requestHeader is the header the core sends upstream: the session's, minus
// what it may not pass (O6 of the P4 plan: a deny list, because a script
// calling an API needs its own headers).
func requestHeader(in http.Header, rt Route) http.Header {
	out := canonical(in)
	stripHopByHop(out)
	for _, k := range requestDeny {
		out.Del(k)
	}
	for k := range out {
		if strings.HasPrefix(k, "X-Forwarded-") || strings.HasPrefix(k, "Proxy-") {
			delete(out, k)
		}
	}
	if rt.Auth == AuthHeader && rt.Header != "" {
		out.Del(rt.Header)
	}
	return out
}

// responseHeader is the header the session gets: the upstream's, minus the
// connection's own headers, cookies, authentication and the route's own
// credential header.
func responseHeader(in http.Header, rt Route) http.Header {
	out := canonical(in)
	stripHopByHop(out)
	for _, k := range responseDeny {
		out.Del(k)
	}
	for k := range out {
		if strings.HasPrefix(k, "Proxy-") {
			delete(out, k)
		}
	}
	if rt.Auth == AuthHeader && rt.Header != "" {
		out.Del(rt.Header)
	}
	return out
}

// stripHopByHop drops the hop-by-hop headers and every header a Connection
// header names.
func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(textproto.CanonicalMIMEHeaderKey(name))
			}
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

// canonical copies h with every key in canonical form, so a key the worker
// wrote as "authorization" is dropped by Del("Authorization") like any other.
func canonical(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}
