package egress

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// The registries whose answers the core rewrites, a fixed list in code (RFC
// GW D28 as amended): PyPI's index links to its file host, and the crates
// index's config.json names its download host, neither of which a session
// can reach by name.
const (
	pypiHost      = "pypi.org"
	pypiFilesURL  = "https://files.pythonhosted.org/"
	maxRewriteLen = 32 << 20

	cratesIndexHost = "index.crates.io"
	cratesDLURL     = "https://static.crates.io/"
	// cratesConfig is the one answer of the crates index the core rewrites,
	// the file at the index's root; maxCratesConfig bounds it (the real one
	// is under 100 bytes).
	cratesConfig    = "config.json"
	maxCratesConfig = 64 << 10
)

// answerRewrite is a rewrite of one route's answers.
type answerRewrite interface {
	// applies reports whether resp is an answer this rewrite reads.
	applies(resp *http.Response) bool
	// apply reads the whole answer and returns it rewritten.
	apply(body io.ReadCloser) (rewritten, error)
}

// pypiRewrite turns PyPI's absolute file links into links to the route that
// serves the file host.
type pypiRewrite struct {
	from, to []byte
}

// rewriteFor is the rewrite an answer to path through rt gets, or nil. Only
// an operator's route is rewritten, never a tenant's, and a link is never
// pointed at a route a tenant wrote.
//
// On pypi.org the target is the operator's route on the file host that the
// workspace holds, else the one the operator declares (PyPIFilesRoute) even
// though the workspace does not hold it: the download is then refused naming
// that route, where an unrewritten link would send the session to a host it
// cannot resolve (P4 plan O34, which widens D28's "when the workspace also
// holds the paired route"). The crates index's config.json is pointed at the
// download route the same way (RFC GW P11).
func (p *Proxy) rewriteFor(g Grant, rt Route, base, path string) answerRewrite {
	if rt.Tenant {
		return nil
	}
	u, err := checkRoute(rt)
	if err != nil {
		return nil
	}
	switch {
	case strings.EqualFold(u.Host, pypiHost):
		name := PyPIFilesRoute(g.Routes)
		if name == "" {
			name = p.filesRoute
		}
		if name == "" {
			return nil
		}
		return &pypiRewrite{from: []byte(pypiFilesURL), to: []byte(base + name + "/")}
	case strings.EqualFold(u.Host, cratesIndexHost):
		// cargo takes no relative dl, so a grant whose links are relative
		// (the host engine's, whose address the core does not know) is left
		// as the registry wrote it: that engine has the host's network.
		if g.RelativeBase {
			return nil
		}
		if file, _, _ := strings.Cut(path, "?"); file != cratesConfig {
			return nil
		}
		name := CratesDLRoute(g.Routes)
		if name == "" {
			name = p.dlRoute
		}
		if name == "" {
			return nil
		}
		return &cratesRewrite{to: base + name + "/"}
	}
	return nil
}

// PyPIFilesRoute is the name of the operator's route to PyPI's file host
// among routes, the first by name when there are several; "" for none.
func PyPIFilesRoute(routes []Route) string { return operatorRouteTo(routes, pypiFilesURL) }

// CratesDLRoute is the name of the operator's route to the crates download
// host among routes, the first by name when there are several; "" for none.
func CratesDLRoute(routes []Route) string { return operatorRouteTo(routes, cratesDLURL) }

// operatorRouteTo is the first by name of the operator's routes whose URL is
// exactly url; "" for none.
func operatorRouteTo(routes []Route, url string) string {
	name := ""
	for _, f := range routes {
		if f.Tenant || f.URL != url {
			continue
		}
		if _, err := checkRoute(f); err != nil {
			continue
		}
		if name == "" || f.Name < name {
			name = f.Name
		}
	}
	return name
}

func (w *pypiRewrite) applies(resp *http.Response) bool { return rewritable(resp) }

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

// cratesRewrite points the crates index's download host at a route: the dl
// field of its config.json. The api field goes, since nothing is published
// through a route. cargo checks each crate against the checksum the index
// lists beside it, which is untouched.
type cratesRewrite struct {
	to string
}

// applies reads a 200 JSON answer that is not still compressed.
func (w *cratesRewrite) applies(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

func (w *cratesRewrite) apply(body io.ReadCloser) (rewritten, error) {
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(io.LimitReader(body, maxCratesConfig+1))
	if err != nil {
		return rewritten{}, err
	}
	if len(b) > maxCratesConfig {
		return rewritten{}, refuse(http.StatusBadGateway, "the crates index's config.json is over %d KiB, the most the core rewrites", maxCratesConfig>>10)
	}
	// RawMessage keeps every other field byte for byte.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return rewritten{}, refuse(http.StatusBadGateway, "the crates index's config.json is not a JSON object the core can read")
	}
	var dl string
	if raw, ok := m["dl"]; ok && json.Unmarshal(raw, &dl) == nil && strings.HasPrefix(dl, cratesDLURL) {
		to, err := json.Marshal(w.to + strings.TrimPrefix(dl, cratesDLURL))
		if err != nil {
			return rewritten{}, err
		}
		m["dl"] = to
	}
	delete(m, "api")
	out, err := json.Marshal(m)
	if err != nil {
		return rewritten{}, err
	}
	return rewritten{body: io.NopCloser(bytes.NewReader(out)), n: len(out)}, nil
}
