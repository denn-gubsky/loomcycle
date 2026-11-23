package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Sharing (RFC GW §5.2, P8). A workspace is its owner's alone until the
// owner shares it with another run of the same user, which then names it
// "<name>@<owner agent id>". The share is on the workspace's row, so every
// replica checks it where it routes an op, and it holds across a resume.
//
// What a share lets a peer do is decided here and in the tools: here, which
// workspace a shared name reaches and whether the share covers a write; in
// the tools, which ops take a shared name at all (never a command, a
// release or a share of it). The peer's own workspace grant still applies on
// top: a share never gives an agent more than its definition does.

// MaxSharesPerWorkspace bounds a workspace's shares, and with them the row.
const MaxSharesPerWorkspace = 16

// The errors sharing adds.
var (
	// ErrShareTarget: with names no live run of the caller's user in its
	// tenant. One answer for a run of another user, an ended run and an
	// agent id nobody holds, so a share cannot probe for runs.
	ErrShareTarget = errors.New("no live run of this user has that agent id")
	// ErrShareNoUser: a run with no user id neither shares nor is shared
	// with, since "the same user" then joins callers nothing identifies.
	ErrShareNoUser = errors.New("a workspace is shared only between runs of a user, and this run has none")
	// ErrShareLimit: the workspace holds MaxSharesPerWorkspace shares.
	ErrShareLimit = fmt.Errorf("a workspace is shared with at most %d runs; unshare one first", MaxSharesPerWorkspace)
	// ErrShareReadOnly: a write through a read share.
	ErrShareReadOnly = errors.New("this workspace is shared with this run read-only")
	// ErrShareBusy: the shares kept changing under a share or an unshare.
	ErrShareBusy = errors.New("the workspace's shares changed while they were written; try again")
)

// SplitShared splits a shared workspace's name, "<name>@<agent id>". ok is
// false for a name without '@' — a workspace of the caller's own. A
// workspace name never holds '@' (ValidName), so the first one splits.
func SplitShared(ref string) (name, ownerAgentID string, ok bool) {
	name, ownerAgentID, ok = strings.Cut(ref, "@")
	return name, ownerAgentID, ok
}

// SharedName is how a peer names a workspace shared with it.
func SharedName(name, ownerAgentID string) string { return name + "@" + ownerAgentID }

// ValidShareAccess reports whether access is one a share may give.
func ValidShareAccess(access string) bool {
	return access == store.WorkspaceShareRead || access == store.WorkspaceShareReadWrite
}

// Peer is the run a share is made with.
type Peer struct {
	RunID, AgentID string
}

// SetAudit sets where shares, unshares and uses through a share are
// recorded. Called once at boot, before any run; nil records nothing.
func (m *Manager) SetAudit(s audit.Sink) { m.audit = s }

// ResolvePeer finds the run a share names by agent id: the newest run
// carrying it, accepted only when it is of the caller's tenant and user, has
// not ended, and is not the caller itself (D19).
func (m *Manager) ResolvePeer(ctx context.Context, o Owner, agentID string) (Peer, error) {
	if o.UserID == "" {
		return Peer{}, ErrShareNoUser
	}
	if agentID == "" || agentID == o.AgentID {
		return Peer{}, fmt.Errorf("%w (a run does not share with itself)", ErrShareTarget)
	}
	run, err := m.store.GetRunByAgentID(ctx, agentID)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return Peer{}, ErrShareTarget
	}
	if err != nil {
		return Peer{}, err
	}
	if run.TenantID != o.TenantID || run.UserID != o.UserID || store.IsTerminalRunStatus(run.Status) {
		return Peer{}, ErrShareTarget
	}
	if run.ID == o.RunID {
		return Peer{}, fmt.Errorf("%w (a run does not share with itself)", ErrShareTarget)
	}
	return Peer{RunID: run.ID, AgentID: agentID}, nil
}

// Share shares the run's workspace name with p. A second share with the
// same run replaces the first's access. The caller has checked that the run
// may share and holds what access hands over; p comes from ResolvePeer, or
// is a child the runtime just made (spawn-time shares).
func (m *Manager) Share(ctx context.Context, o Owner, name string, p Peer, access string) (store.WorkspaceRow, error) {
	if !ValidShareAccess(access) {
		return store.WorkspaceRow{}, fmt.Errorf("%w: access %q (want read or read_write)", ErrInvalidName, access)
	}
	if o.UserID == "" {
		return store.WorkspaceRow{}, ErrShareNoUser
	}
	if p.RunID == "" || p.RunID == o.RunID {
		return store.WorkspaceRow{}, fmt.Errorf("%w (a run does not share with itself)", ErrShareTarget)
	}
	row, err := m.updateShares(ctx, o, name, func(cur []store.WorkspaceShare) ([]store.WorkspaceShare, error) {
		next := slices.DeleteFunc(slices.Clone(cur), func(s store.WorkspaceShare) bool { return s.RunID == p.RunID })
		if len(next) >= MaxSharesPerWorkspace {
			return nil, ErrShareLimit
		}
		return append(next, store.WorkspaceShare{RunID: p.RunID, AgentID: p.AgentID, Access: access, CreatedAt: m.now().UTC()}), nil
	})
	if err != nil {
		return store.WorkspaceRow{}, err
	}
	m.record(o, row, "workspace_share", p, access, "")
	return row, nil
}

// Unshare ends the shares of the run's workspace name with runs carrying
// agentID, and reports whether there was one. The agent id is matched as
// stored, so a share with a run that has since ended is removed too.
func (m *Manager) Unshare(ctx context.Context, o Owner, name, agentID string) (bool, error) {
	var gone []store.WorkspaceShare
	row, err := m.updateShares(ctx, o, name, func(cur []store.WorkspaceShare) ([]store.WorkspaceShare, error) {
		gone = gone[:0]
		next := slices.DeleteFunc(slices.Clone(cur), func(s store.WorkspaceShare) bool {
			if s.AgentID == agentID {
				gone = append(gone, s)
				return true
			}
			return false
		})
		if len(gone) == 0 {
			return nil, errNoChange
		}
		return next, nil
	})
	if errors.Is(err, errNoChange) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, s := range gone {
		m.record(o, row, "workspace_unshare", Peer{RunID: s.RunID, AgentID: s.AgentID}, s.Access, "")
	}
	return true, nil
}

var errNoChange = errors.New("no change")

// updateShares rewrites the shares of the run's own ready workspace by
// compare-and-set, reading again when another call moved them first.
func (m *Manager) updateShares(ctx context.Context, o Owner, name string, edit func([]store.WorkspaceShare) ([]store.WorkspaceShare, error)) (store.WorkspaceRow, error) {
	for range 5 {
		row, err := m.Get(ctx, o, name)
		if err != nil {
			return store.WorkspaceRow{}, err
		}
		if row.State != store.WorkspaceReady {
			return store.WorkspaceRow{}, fmt.Errorf("%w: %q is %s", ErrNotReady, name, row.State)
		}
		next, err := edit(row.Shares)
		if err != nil {
			return store.WorkspaceRow{}, err
		}
		ok, err := m.store.WorkspaceSetShares(ctx, row.TenantID, row.ID, row.Shares, next)
		if err != nil {
			return store.WorkspaceRow{}, err
		}
		if ok {
			row.Shares = next
			return row, nil
		}
	}
	return store.WorkspaceRow{}, ErrShareBusy
}

// resolveShared is the workspace a shared name reaches for the run, and the
// share that lets it. Every refusal but a write through a read share is
// ErrNotFound, the answer for a workspace nobody opened: a run learns
// nothing of workspaces not shared with it.
func (m *Manager) resolveShared(ctx context.Context, o Owner, ref string, write bool) (store.WorkspaceRow, store.WorkspaceShare, error) {
	name, ownerAgent, _ := SplitShared(ref)
	if o.RunID == "" || o.UserID == "" || ownerAgent == "" || !ValidName(name) {
		return store.WorkspaceRow{}, store.WorkspaceShare{}, ErrNotFound
	}
	rows, err := m.store.WorkspaceListByOwnerAgent(ctx, o.TenantID, ownerAgent, name)
	if err != nil {
		return store.WorkspaceRow{}, store.WorkspaceShare{}, err
	}
	for _, r := range rows {
		// The query asks for the tenant already; the rest is the share.
		if r.TenantID != o.TenantID || r.UserID != o.UserID || r.OwnerAgentID != ownerAgent || r.OwnerRunID == o.RunID {
			continue
		}
		i := slices.IndexFunc(r.Shares, func(s store.WorkspaceShare) bool { return s.RunID == o.RunID })
		if i < 0 {
			continue
		}
		sh := r.Shares[i]
		// A share ends when its owner does, before the sweeper removes the
		// row of an owner that ended without its release.
		owner, err := m.store.GetRun(ctx, r.OwnerRunID)
		var nf *store.ErrNotFound
		if errors.As(err, &nf) || (err == nil && store.IsTerminalRunStatus(owner.Status)) {
			continue
		}
		if err != nil {
			return store.WorkspaceRow{}, store.WorkspaceShare{}, err
		}
		if write && sh.Access != store.WorkspaceShareReadWrite {
			return store.WorkspaceRow{}, store.WorkspaceShare{}, ErrShareReadOnly
		}
		return r, sh, nil
	}
	return store.WorkspaceRow{}, store.WorkspaceShare{}, ErrNotFound
}

// UseShared checks that the run may reach the shared workspace ref (for a
// write when write) and records the use: once per tool call, not per file
// of a folder copy, which the ops it then makes check again without a
// record. op names the use ("read", "write", "remove"), via the tool.
func (m *Manager) UseShared(ctx context.Context, o Owner, ref string, write bool, op, via string) error {
	row, sh, err := m.resolveShared(ctx, o, ref, write)
	if err != nil {
		return err
	}
	_, ownerAgent, _ := SplitShared(ref)
	// The record names the owner as the actor's counterpart: the peer is
	// the caller here.
	m.recordUse(o, row, ownerAgent, sh.Access, op+" via "+via)
	return nil
}

// SharedWithRun is a workspace shared with a run, as its list shows it.
type SharedWithRun struct {
	Row   store.WorkspaceRow
	Share store.WorkspaceShare
}

// SharedWith lists the ready workspaces shared with the run, of its user,
// whose owners have not ended.
func (m *Manager) SharedWith(ctx context.Context, o Owner) ([]SharedWithRun, error) {
	out := []SharedWithRun{}
	if o.RunID == "" || o.UserID == "" {
		return out, nil
	}
	rows, err := m.store.WorkspaceListSharedWith(ctx, o.TenantID, o.RunID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.State != store.WorkspaceReady || r.UserID != o.UserID || r.TenantID != o.TenantID {
			continue
		}
		if _, sh, err := m.resolveShared(ctx, o, SharedName(r.Name, r.OwnerAgentID), false); err == nil {
			out = append(out, SharedWithRun{Row: r, Share: sh})
		}
	}
	return out, nil
}

// record writes a share's or an unshare's audit event: the owner is the
// caller, the peer the other run.
func (m *Manager) record(o Owner, r store.WorkspaceRow, action string, p Peer, access, op string) {
	if m.audit == nil {
		return
	}
	_ = m.audit.Record(audit.Event{
		Action: action, ActorTenant: o.TenantID, ActorSubject: o.UserID,
		TargetTenant: r.TenantID, TargetName: "workspace:" + r.Name,
		WorkspaceID: r.ID, WorkspaceOwnerRunID: r.OwnerRunID, WorkspaceOwnerAgentID: r.OwnerAgentID,
		WorkspacePeerRunID: p.RunID, WorkspacePeerAgentID: p.AgentID, WorkspaceAccess: access, WorkspaceOp: op,
	}) // best effort: an audit sink that fails does not refuse a share
}

// recordUse writes the audit event of a use through a share: the caller is
// the peer.
func (m *Manager) recordUse(o Owner, r store.WorkspaceRow, ownerAgent, access, op string) {
	if m.audit == nil {
		return
	}
	_ = m.audit.Record(audit.Event{
		Action: "workspace_share_use", ActorTenant: o.TenantID, ActorSubject: o.UserID,
		TargetTenant: r.TenantID, TargetName: "workspace:" + r.Name,
		WorkspaceID: r.ID, WorkspaceOwnerRunID: r.OwnerRunID, WorkspaceOwnerAgentID: ownerAgent,
		WorkspacePeerRunID: o.RunID, WorkspacePeerAgentID: o.AgentID, WorkspaceAccess: access, WorkspaceOp: op,
	})
}
