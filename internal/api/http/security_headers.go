package http

import (
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/webui"
)

// apiCSP is the policy of every response that is not a page: JSON, a
// redirect, a stream. Nothing in it may load anything or be framed, so an API
// response opened in a browser tab, or a reflected value in one, cannot run.
// Routes that serve a page set their own: the Web UI (webui.Handler), the
// API reference (/v1/docs), the app sign-in error page, inbox downloads.
const apiCSP = "default-src 'none'; frame-ancestors 'none'"

// hstsValue is sent when LOOMCYCLE_PUBLIC_URL is https. No includeSubDomains:
// other hosts under the operator's domain are not ours to pin to https.
const hstsValue = "max-age=31536000"

// securityHeaders sets the browser-facing headers on every response (RFC FZ
// A6): no framing, no sniffing, a same-origin Referer, the API policy unless
// the route sets its own, and HSTS when the public URL is https. Set before
// the route runs, so a handler's own Content-Security-Policy replaces apiCSP
// and every route, a sign-in route added later included, is covered without
// being listed.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		webui.SetBaseSecurityHeaders(h)
		h.Set("Content-Security-Policy", apiCSP)
		if c := s.cfg(); c != nil && strings.HasPrefix(strings.ToLower(c.Env.PublicURL), "https://") {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}
