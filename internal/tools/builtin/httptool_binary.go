package builtin

import (
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// A response body that is a file — a PDF, an image, an Office document — is
// not text, and returned as text it reaches the model as mangled bytes it can
// neither read nor pass on. The HTTP and WebFetch tools keep such a body in
// the run's inbox instead and return the stub that names it; text responses
// go down the text path unchanged.

// httpOutcome is what HTTP.do learned besides the text it returns.
type httpOutcome struct {
	// captured is set when the body was a file: the inbox capture (kept, or
	// the refusal stub saying why not). The result's body is its stub.
	captured *inbox.Result
	// finalURL is the URL the body came from, after redirects; nil when no
	// response was read.
	finalURL string
}

// binaryResponse reports whether a response's body is a file, and its media
// type. The declared Content-Type decides when it is specific; a generic or
// missing one is settled by sniffing the first bytes, and a sniff that is
// still generic by the name's extension (inbox.Sniff's order). A HEAD, or a
// body with no bytes, has nothing to keep.
func binaryResponse(method string, resp *http.Response, head []byte, name string) (string, bool) {
	if method == http.MethodHead || len(head) == 0 {
		return "", false
	}
	mt := inbox.Sniff(resp.Header.Get("Content-Type"), head, name)
	return mt, binaryMediaType(mt)
}

// binaryMediaType is the set of types kept as files: images, audio, video,
// PDF, Office documents, zip archives and opaque bytes. Everything else —
// HTML, JSON, XML, CSV, plain text — stays text.
func binaryMediaType(mt string) bool {
	for _, p := range []string{"image/", "audio/", "video/", "application/vnd.openxmlformats-officedocument.", "application/vnd.ms-"} {
		if strings.HasPrefix(mt, p) {
			return true
		}
	}
	switch mt {
	case "application/pdf", "application/zip", "application/x-zip-compressed",
		"application/octet-stream", "application/msword", "application/x-ole-storage":
		return true
	}
	return false
}

// responseFileName names a response's file: the Content-Disposition filename
// when the server gave one, else the last segment of the URL's path (escaped;
// the inbox decodes and sanitises it).
func responseFileName(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if fn := strings.TrimSpace(params["filename"]); fn != "" {
			return fn
		}
	}
	if resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	if base := path.Base(resp.Request.URL.EscapedPath()); base != "/" && base != "." {
		return base
	}
	return ""
}
