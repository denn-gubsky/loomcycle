package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
)

// git.go — op tickets for git workspaces (RFC GW §6.1, P11 plan §3.3, G2 as
// the owner confirmed it on 2026-11-26).
//
// A git request reaches the core on the worker's stream, outside the run
// that asked for it, while the checks that decide it (git_remote, the host
// lists, the access-control rules, the credential's own resolution) need
// the run. So the tool call runs them, in the run, and mints a ticket: 32
// random bytes, good for one workspace of one tenant, for the shapes of one
// op, until the op ends or its deadline passes. The resolved credential
// stays on the ticket, in this replica's memory; the worker holds only the
// ticket's bytes, which open nothing but this op's way to the forge.

// ticketBytes is a ticket's length.
const ticketBytes = 32

// errNoTicket is every refusal of a ticket, whichever check failed: the
// worker learns nothing of which.
var errNoTicket = &egress.Refusal{Status: http.StatusForbidden, Reason: "no live git ticket of this workspace covers the request"}

// GitTicketSpec is what an op mints a ticket for.
type GitTicketSpec struct {
	// Row is the workspace's row as the op read it: a git workspace's.
	Row store.WorkspaceRow
	// Grant is what the op may do: the repository (the row's), the resolved
	// credential, the shapes, the push policy and the read budget.
	Grant egress.GitGrant
	// ForOpen marks the open's own ticket, the one that may reach the
	// workspace while its row is still opening.
	ForOpen bool
	// TTL bounds the ticket's life: the op's timeout.
	TTL time.Duration
}

// GitTicket is a live ticket. End it when the op returns.
type GitTicket struct {
	secret      []byte
	workspaceID string
	tenantID    string
	repoURL     string
	forOpen     bool
	grant       egress.GitGrant
	deadline    time.Time
	ended       atomic.Bool
	end         func()
	advertised  atomic.Bool
	uploaded    atomic.Bool
	defaultRef  atomic.Pointer[string]
}

// Secret is the ticket's bytes, for the op's arguments to the worker.
func (t *GitTicket) Secret() []byte { return slices.Clone(t.secret) }

// End ends the ticket: no request is accepted under it afterwards.
// Idempotent.
func (t *GitTicket) End() {
	if t.ended.CompareAndSwap(false, true) && t.end != nil {
		t.end()
	}
}

// Proved reports whether the forge answered both an advertisement and an
// upload-pack with 200 under this ticket: an open's proof of access, read
// from the forge's own answers, never from the worker's word.
func (t *GitTicket) Proved() bool { return t.advertised.Load() && t.uploaded.Load() }

// DefaultBranch is the forge's default branch as an advertisement under this
// ticket named it, "" if none did.
func (t *GitTicket) DefaultBranch() string {
	if p := t.defaultRef.Load(); p != nil {
		return *p
	}
	return ""
}

// gitTickets are the live tickets, by workspace id.
type gitTickets struct {
	mu sync.Mutex
	by map[string][]*GitTicket
}

func (g *gitTickets) add(t *GitTicket) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.by == nil {
		g.by = map[string][]*GitTicket{}
	}
	g.by[t.workspaceID] = append(g.by[t.workspaceID], t)
}

func (g *gitTickets) remove(t *GitTicket) {
	g.mu.Lock()
	defer g.mu.Unlock()
	list := slices.DeleteFunc(g.by[t.workspaceID], func(x *GitTicket) bool { return x == t })
	if len(list) == 0 {
		delete(g.by, t.workspaceID)
		return
	}
	g.by[t.workspaceID] = list
}

// find is the live ticket of workspace id whose bytes are secret. Every
// candidate is compared in constant time, and every one is compared, so the
// time taken says nothing of which matched.
func (g *gitTickets) find(id string, secret []byte, now time.Time) *GitTicket {
	g.mu.Lock()
	list := slices.Clone(g.by[id])
	g.mu.Unlock()
	var found *GitTicket
	for _, t := range list {
		if subtle.ConstantTimeCompare(t.secret, secret) == 1 && found == nil {
			found = t
		}
	}
	// ended is checked as well as the registry: a lookup that copied the
	// list just before an End still finds the ticket there.
	if found == nil || found.ended.Load() || !now.Before(found.deadline) {
		return nil
	}
	return found
}

// SetGitProxy gives the manager the proxy git workspaces' requests go out
// through. Called once at boot; without it every git request is refused.
func (m *Manager) SetGitProxy(p *egress.GitProxy) { m.gitProxy = p }

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// MintGitTicket mints a ticket for one op of a git workspace. The grant's
// repository must be the row's.
func (m *Manager) MintGitTicket(s GitTicketSpec) (*GitTicket, error) {
	switch {
	case s.Row.Kind != store.WorkspaceKindGit || s.Row.Git == nil:
		return nil, errors.New("git ticket: the workspace is not a git workspace")
	case s.Grant.RepoURL != s.Row.Git.URL:
		return nil, errors.New("git ticket: the grant's repository is not the workspace's")
	case len(s.Grant.Shapes) == 0:
		return nil, errors.New("git ticket: the grant allows no request")
	case s.TTL <= 0:
		return nil, errors.New("git ticket: a ticket needs a lifetime")
	}
	secret := make([]byte, ticketBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	t := &GitTicket{
		secret: secret, workspaceID: s.Row.ID, tenantID: s.Row.TenantID, repoURL: s.Row.Git.URL,
		forOpen: s.ForOpen, grant: s.Grant, deadline: m.clock().Add(s.TTL),
	}
	t.grant.Shapes = slices.Clone(s.Grant.Shapes)
	report := s.Grant.OnDefaultBranch
	t.grant.OnDefaultBranch = func(ref string) {
		t.defaultRef.Store(&ref)
		if report != nil {
			report(ref)
		}
	}
	t.end = func() { m.gitTickets.remove(t) }
	m.gitTickets.add(t)
	return t, nil
}

// GitEgress answers one git request a git workspace's supervisor sent: the
// dispatcher's GitEgressFunc. row is the store's. The request goes out only
// under a live ticket of this workspace and tenant, for this repository, and,
// while the row is opening, only under the open's own ticket.
func (m *Manager) GitEgress(ctx context.Context, row store.WorkspaceRow, ticket []byte, req egress.GitRequest) (*egress.Response, error) {
	if m.gitProxy == nil {
		return nil, &egress.Refusal{Status: http.StatusForbidden, Reason: "this runtime serves no git workspace"}
	}
	t := m.gitTickets.find(row.ID, ticket, m.clock())
	switch {
	case t == nil:
		return nil, errNoTicket
	case t.tenantID != row.TenantID || t.workspaceID != row.ID:
		return nil, errNoTicket
	case row.Kind != store.WorkspaceKindGit || row.Git == nil || row.Git.URL != t.repoURL:
		return nil, errNoTicket
	case row.State == store.WorkspaceOpening && !t.forOpen:
		return nil, errNoTicket
	case row.State != store.WorkspaceOpening && row.State != store.WorkspaceReady:
		return nil, errNoTicket
	}
	resp, err := m.gitProxy.Do(ctx, t.grant, req)
	if err != nil {
		return nil, err
	}
	if resp.Status == http.StatusOK {
		switch req.Shape {
		case egress.GitAdvertiseUpload:
			t.advertised.Store(true)
		case egress.GitUploadPack:
			resp.Body = &provedBody{ReadCloser: resp.Body, done: &t.uploaded}
		}
	}
	return resp, nil
}

// provedBody marks an upload-pack proved once its answer was read to its end:
// a fetch the forge refused midway proves nothing.
type provedBody struct {
	io.ReadCloser
	done *atomic.Bool
}

func (b *provedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.done.Store(true)
	}
	return n, err
}
