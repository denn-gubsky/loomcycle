package webui

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
)

// SetBaseSecurityHeaders sets the headers every loomcycle response carries
// whatever its content (RFC FZ A6): no framing, no MIME sniffing, and no
// Referer beyond this origin. Content-Security-Policy is per page and set by
// whoever writes the page.
func SetBaseSecurityHeaders(h http.Header) {
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
}

// spaCSP is the Web UI's policy, given the hashes of the inline scripts its
// index.html carries (the theme bootstrap that must run before first paint).
// What each source is for, from the built bundle:
//
//   - script-src: the bundle's own modules, and the shell's inline scripts by
//     hash only — no 'unsafe-inline', no 'unsafe-eval' (nothing in the
//     bundle evaluates strings).
//   - style-src 'unsafe-inline': React style attributes, and the <style>
//     elements CodeMirror and mermaid insert at run time.
//   - img-src data: blob:: CodeMirror's data: SVG backgrounds, and document
//     images, which the UI fetches with the session and shows as blob: URLs.
//     Images from any other origin are refused, so a rendered document or
//     model output cannot make the browser call out.
//   - font-src data:: the bundled fonts, some inlined in the CSS.
//   - connect-src 'self': every API call and stream is same-origin.
//   - frame-ancestors 'none': nothing embeds the UI (the cloud console's
//     sign-in handoff opens it in a window, not a frame).
//
// form-action is left unset: a sign-in that ends in a redirect to an
// identity provider would be refused by browsers that apply it to
// redirects, and the UI submits no native form.
func spaCSP(scriptHashes []string) string {
	script := "script-src 'self'"
	if len(scriptHashes) > 0 {
		script += " " + strings.Join(scriptHashes, " ")
	}
	return strings.Join([]string{
		"default-src 'self'",
		script,
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

var (
	scriptElemRe = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	srcAttrRe    = regexp.MustCompile(`(?i)(?:^|\s)src\s*=`)
)

// InlineScriptHashes returns the CSP hash source ('sha256-…') of every inline
// script in html, in order: the exact text between the tags, which is what a
// browser hashes. A script with a src attribute is skipped.
func InlineScriptHashes(html []byte) []string {
	var out []string
	for _, m := range scriptElemRe.FindAllSubmatch(html, -1) {
		if srcAttrRe.Match(m[1]) {
			continue
		}
		sum := sha256.Sum256(m[2])
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return out
}

// withSecurityHeaders sets the base headers and csp on every response next
// writes.
func withSecurityHeaders(next http.Handler, csp string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		SetBaseSecurityHeaders(h)
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
