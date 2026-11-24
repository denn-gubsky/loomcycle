package egress

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"strings"
)

// The registries whose answers the core rewrites, a fixed list in code (RFC
// GW D28 as amended): PyPI's index links to its file host, which a session
// cannot reach by name. cargo's index (P11) joins here when its routes do.
const (
	pypiHost      = "pypi.org"
	pypiFilesURL  = "https://files.pythonhosted.org/"
	maxRewriteLen = 32 << 20
)

// pypiRewrite turns PyPI's absolute file links into links to the route that
// serves the file host.
type pypiRewrite struct {
	from, to []byte
}

// rewriteFor is the rewrite an answer through rt gets, or nil. Only an
// operator's route on pypi.org, and only when the workspace also holds an
// operator's route on the file host: a tenant's route is never rewritten,
// whatever its URL, and a link is never pointed at a route a tenant wrote.
func (p *Proxy) rewriteFor(g Grant, rt Route) *pypiRewrite {
	if rt.Tenant {
		return nil
	}
	u, err := checkRoute(rt)
	if err != nil || !strings.EqualFold(u.Host, pypiHost) {
		return nil
	}
	for _, f := range g.Routes {
		if f.Tenant || f.URL != pypiFilesURL {
			continue
		}
		if _, err := checkRoute(f); err != nil {
			continue
		}
		return &pypiRewrite{from: []byte(pypiFilesURL), to: []byte(p.baseFor(g) + f.Name + "/")}
	}
	return nil
}

// rewritable reports whether an answer is an index page the rewrite reads:
// a 200 in HTML or one of PyPI's JSON forms, not still compressed.
func rewritable(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mt == "text/html" || mt == "application/json" ||
		(strings.HasPrefix(mt, "application/vnd.pypi.simple.") && strings.HasSuffix(mt, "+json"))
}

type rewritten struct {
	body io.ReadCloser
	n    int
}

// apply reads a whole index page, at most maxRewriteLen (O9 of the P4 plan),
// and rewrites its file links. The hashes beside the links are untouched, so
// pip still checks what it downloads against what PyPI listed.
func (w *pypiRewrite) apply(body io.ReadCloser) (rewritten, error) {
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(io.LimitReader(body, maxRewriteLen+1))
	if err != nil {
		return rewritten{}, err
	}
	if len(b) > maxRewriteLen {
		return rewritten{}, refuse(http.StatusBadGateway, "the index page is over %d MiB, the most the core rewrites", maxRewriteLen>>20)
	}
	out := bytes.ReplaceAll(b, w.from, w.to)
	return rewritten{body: io.NopCloser(bytes.NewReader(out)), n: len(out)}, nil
}
