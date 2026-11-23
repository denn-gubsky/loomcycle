package egress

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// auditor writes the one line a request through a credentialed or
// tenant-written route leaves (RFC GW §6.3): when the core refuses it, when
// the upstream fails, or when the session closes the answer.
type auditor struct {
	p      *Proxy
	g      Grant
	rt     Route
	method string
	path   string
	usage  *usage
	status int
	bytes  atomic.Int64
	once   sync.Once
}

// audited reports whether the route's requests are audited: those that carry
// a credential, and those through a route a tenant wrote.
func (a *auditor) audited() bool { return a.rt.Credential != "" || a.rt.Tenant }

func (a *auditor) emit(status int, refused string) {
	if a.p.audit == nil || !a.audited() {
		return
	}
	a.once.Do(func() {
		a.p.audit(Line{
			TenantID: a.g.TenantID, RunID: a.g.RunID, WorkspaceID: a.g.WorkspaceID,
			Route: a.rt.Name, Method: safeToken(a.method), Path: a.path,
			Status: status, Bytes: a.bytes.Load(), Refused: refused,
		})
	})
}

func (a *auditor) refused(r *Refusal) { a.emit(r.Status, r.Reason) }

func (a *auditor) failed() { a.emit(http.StatusBadGateway, "the upstream request failed") }

// auditedBody writes the line when the session is done with the answer, so it
// counts the bytes that passed.
type auditedBody struct {
	io.ReadCloser
	a *auditor
}

func (b *auditedBody) Close() error {
	err := b.ReadCloser.Close()
	b.a.emit(b.a.status, "")
	return err
}
