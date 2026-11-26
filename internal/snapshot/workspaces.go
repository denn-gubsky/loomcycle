package snapshot

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
)

// The workspaces section (RFC GW P12 plan §3.6, §5): a workspace's row, its
// pin, travels; its files never do. A run restored on another instance keeps
// its workspace's name, and the files are wherever the worker's disk is.
//
// A restored row is made to point only at what is its own (§5.2):
//   - its worker id must carry its own tenant's grant (tenant:<T>/,
//     local:<T>/) or be a shared worker's (shared/); any other is skipped;
//   - a pinned row's run must be a run of its own tenant and user on the
//     target (one restored with it, or already there); otherwise a durable
//     row is parked and an ephemeral one skipped;
//   - its shares keep only runs of its own tenant and user on the target;
//   - its egress keeps only the routes the target's configuration defines;
//   - a git record travels with its credential as a reference only.
//
// Nothing is trusted until a worker proves it (§5.3): every restored row is
// lost, and only an attach of its worker that lists its id makes it ready.
// A row the target already has, by id or by durable name, is skipped, never
// merged. Principal-owned rows (their token definitions never travel),
// opening and releasing rows, exposures, templates and checkout records are
// never captured.

// WorkspacesSection carries workspace rows.
type WorkspacesSection struct {
	Version string           `json:"version"`
	Entries []WorkspaceEntry `json:"entries"`
}

// WorkspaceEntry is one workspace row as it travels.
type WorkspaceEntry struct {
	ID           string                `json:"id"`
	TenantID     string                `json:"tenant_id"`
	UserID       string                `json:"user_id"`
	Name         string                `json:"name"`
	WorkerID     string                `json:"worker_id"`
	Kind         string                `json:"kind"`
	Lifetime     string                `json:"lifetime"`
	Engine       string                `json:"engine"`
	OwnerKind    string                `json:"owner_kind"`
	OwnerRunID   string                `json:"owner_run_id"`
	OwnerAgentID string                `json:"owner_agent_id"`
	RootRunID    string                `json:"root_run_id"`
	State        string                `json:"state"`
	Root         string                `json:"root,omitempty"`
	Egress       []string              `json:"egress"`
	Shares       []WorkspaceShareEntry `json:"shares"`
	Git          *WorkspaceGitEntry    `json:"git,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
}

// WorkspaceShareEntry is one share of a workspace as it travels.
type WorkspaceShareEntry struct {
	RunID     string    `json:"run_id"`
	AgentID   string    `json:"agent_id"`
	Access    string    `json:"access"`
	Connect   bool      `json:"connect,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkspaceGitEntry is a git workspace's record as it travels: the
// credential is a reference, never a value.
type WorkspaceGitEntry struct {
	URL           string `json:"url"`
	Branch        string `json:"branch"`
	BaseRef       string `json:"base_ref,omitempty"`
	Base          string `json:"base,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Credential    string `json:"credential,omitempty"`
	Mirror        string `json:"mirror"`
}

func sharesOut(in []store.WorkspaceShare) []WorkspaceShareEntry {
	out := make([]WorkspaceShareEntry, 0, len(in))
	for _, s := range in {
		out = append(out, WorkspaceShareEntry{RunID: s.RunID, AgentID: s.AgentID, Access: s.Access, Connect: s.Connect, CreatedAt: s.CreatedAt.UTC()})
	}
	return out
}

func gitOut(g *store.WorkspaceGit) *WorkspaceGitEntry {
	if g == nil {
		return nil
	}
	return &WorkspaceGitEntry{URL: g.URL, Branch: g.Branch, BaseRef: g.BaseRef, Base: g.Base, DefaultBranch: g.DefaultBranch, Credential: g.Credential, Mirror: g.Mirror}
}

func gitIn(g *WorkspaceGitEntry) *store.WorkspaceGit {
	if g == nil {
		return nil
	}
	return &store.WorkspaceGit{URL: g.URL, Branch: g.Branch, BaseRef: g.BaseRef, Base: g.Base, DefaultBranch: g.DefaultBranch, Credential: g.Credential, Mirror: g.Mirror}
}

// captureWorkspaces keeps the run-owned and parked rows that are ready or
// lost.
func captureWorkspaces(ctx context.Context, s store.Store, out *WorkspacesSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadWorkspaces(ctx)
	if err != nil {
		return fmt.Errorf("snapshot workspaces: %w", err)
	}
	out.Entries = make([]WorkspaceEntry, 0, len(rows))
	for _, r := range rows {
		if r.OwnerKind != store.WorkspaceOwnerRun && r.OwnerKind != store.WorkspaceOwnerParked {
			continue
		}
		if r.State != store.WorkspaceReady && r.State != store.WorkspaceLost {
			continue
		}
		out.Entries = append(out.Entries, WorkspaceEntry{
			ID: r.ID, TenantID: r.TenantID, UserID: r.UserID, Name: r.Name, WorkerID: r.WorkerID,
			Kind: r.Kind, Lifetime: r.Lifetime, Engine: r.Engine, OwnerKind: r.OwnerKind,
			OwnerRunID: r.OwnerRunID, OwnerAgentID: r.OwnerAgentID, RootRunID: r.RootRunID,
			State: string(r.State), Root: r.Root, Egress: r.Egress, Shares: sharesOut(r.Shares), Git: gitOut(r.Git),
			CreatedAt: r.CreatedAt.UTC(),
		})
	}
	return nil
}

func restoreWorkspacesSection(ctx context.Context, rs *restoreRun) error {
	raw, ok := rs.sections[migrations.SectionWorkspaces]
	if !ok {
		return nil
	}
	var sec WorkspacesSection
	if err := decodeWithMigration(migrations.SectionWorkspaces, raw, &sec); err != nil {
		return err
	}
	restoreWorkspaces(ctx, rs.s, &sec, rs.opts, rs.result)
	return nil
}

// restoreWorkspaces writes each row that points only at its own, as lost.
func restoreWorkspaces(ctx context.Context, s store.Store, sec *WorkspacesSection, opts RestoreOptions, result *RestoreResult) {
	for _, e := range sec.Entries {
		where := fmt.Sprintf("workspace %s (%s)", qualifiedName(e.TenantID, e.Name), e.ID)
		row, why := restorableWorkspace(ctx, s, e, opts, where, result)
		if why != "" {
			result.WorkspacesSkipped++
			if why != whyValidated {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %s", where, why))
			}
			continue
		}
		if _, err := s.WorkspaceCreate(ctx, row); err != nil {
			result.WorkspacesSkipped++
			var conflict *store.ErrConflict
			if errors.As(err, &conflict) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: the target holds a workspace of that name", where))
			} else {
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %v", where, err))
			}
			continue
		}
		result.WorkspacesRestored++
	}
	if result.WorkspacesRestored > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"workspaces: %d workspace row(s) restored as lost; each is ready again only when its worker attaches holding its files (they never travel)", result.WorkspacesRestored))
	}
}

// whyValidated marks a skip the validator already warned about.
const whyValidated = "validated"

// restorableWorkspace is the row e restores as, or why it does not.
func restorableWorkspace(ctx context.Context, s store.Store, e WorkspaceEntry, opts RestoreOptions, where string, result *RestoreResult) (store.WorkspaceRow, string) {
	body, err := json.Marshal(e)
	if err != nil {
		return store.WorkspaceRow{}, err.Error()
	}
	if !validRestoredBody(opts, migrations.SectionWorkspaces, where, body, result) {
		return store.WorkspaceRow{}, whyValidated
	}
	switch {
	case e.State != string(store.WorkspaceReady) && e.State != string(store.WorkspaceLost):
		return store.WorkspaceRow{}, fmt.Sprintf("state %q is not one a snapshot carries", e.State)
	case e.Lifetime != store.WorkspaceLifetimeEphemeral && e.Lifetime != store.WorkspaceLifetimeDurable:
		return store.WorkspaceRow{}, fmt.Sprintf("unknown lifetime %q", e.Lifetime)
	case !workerOfOwner(e.WorkerID, e.TenantID, e.UserID):
		return store.WorkspaceRow{}, "its worker is not one its tenant's workspaces may be on"
	case e.Git != nil && e.Git.Credential != "" && !strings.HasPrefix(e.Git.Credential, "$cred:") && !strings.HasPrefix(e.Git.Credential, "$ghapp:"):
		return store.WorkspaceRow{}, "its git credential is not a reference"
	}
	row := store.WorkspaceRow{
		ID: e.ID, TenantID: e.TenantID, UserID: e.UserID, Name: e.Name, WorkerID: e.WorkerID,
		Kind: e.Kind, Lifetime: e.Lifetime, Engine: e.Engine, Root: e.Root, Git: gitIn(e.Git),
		State: store.WorkspaceLost, Egress: keepRoutes(e.Egress, opts.WorkspaceEgressRoutes),
	}
	if _, err := s.WorkspaceGet(ctx, e.TenantID, e.ID); err == nil {
		return store.WorkspaceRow{}, "the target holds a workspace of that id"
	}
	if e.Lifetime == store.WorkspaceLifetimeDurable {
		if _, err := s.WorkspaceGetDurable(ctx, e.TenantID, e.UserID, e.Name); err == nil {
			return store.WorkspaceRow{}, "the target holds a durable workspace of that user and name"
		}
	}
	switch {
	case e.OwnerKind == store.WorkspaceOwnerRun && runOf(ctx, s, e.OwnerRunID, e.TenantID, e.UserID):
		row.OwnerKind, row.OwnerRunID, row.OwnerAgentID, row.RootRunID = store.WorkspaceOwnerRun, e.OwnerRunID, e.OwnerAgentID, e.RootRunID
		for _, sh := range e.Shares {
			if runOf(ctx, s, sh.RunID, e.TenantID, e.UserID) {
				row.Shares = append(row.Shares, store.WorkspaceShare{RunID: sh.RunID, AgentID: sh.AgentID, Access: sh.Access, Connect: sh.Connect, CreatedAt: sh.CreatedAt})
			}
		}
	case e.Lifetime == store.WorkspaceLifetimeDurable && (e.OwnerKind == store.WorkspaceOwnerRun || e.OwnerKind == store.WorkspaceOwnerParked):
		// Its run is not here: it waits, parked, for a run of its user.
		row.OwnerKind, row.OwnerRunID = store.WorkspaceOwnerParked, store.WorkspaceParkedKey(e.ID)
	case e.OwnerKind == store.WorkspaceOwnerRun:
		return store.WorkspaceRow{}, "its run is not of its tenant and user on this host, and an ephemeral workspace is its run's alone"
	default:
		return store.WorkspaceRow{}, fmt.Sprintf("owner kind %q is not one a snapshot carries", e.OwnerKind)
	}
	return row, ""
}

// workerOfOwner reports whether a row of tenant and user may point at
// workerID: a worker of its tenant's grant or a shared one, or a personal
// worker (RFC GW P13, user:<tenant>/<first 16 hex of sha256(user)>/<id>) of
// that very tenant and user. A row with no user points at no personal
// worker (P13 D-P13-24, the restore rule of its plan §5.2).
func workerOfOwner(workerID, tenant, user string) bool {
	if !strings.HasPrefix(workerID, "user:") {
		return workerOfTenant(workerID, tenant)
	}
	if user == "" {
		return false
	}
	// The row's own key and a declared id after it, nothing else.
	key := workspace.PersonalGrantKey(tenant, user) + "/"
	if len(workerID) <= len(key) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(workerID[:len(key)]), []byte(key)) == 1
}

// workerOfTenant reports whether a worker id carries tenant's own grant, or
// is a shared worker's (a grant the operator minted for every tenant).
func workerOfTenant(workerID, tenant string) bool {
	for _, prefix := range []string{"tenant:" + tenant + "/", "local:" + tenant + "/", "shared/"} {
		if strings.HasPrefix(workerID, prefix) && len(workerID) > len(prefix) {
			return true
		}
	}
	return false
}

// runOf reports whether runID is a run of tenant and user on this host.
func runOf(ctx context.Context, s store.Store, runID, tenant, user string) bool {
	if runID == "" {
		return false
	}
	run, err := s.GetRun(ctx, runID)
	return err == nil && run.TenantID == tenant && run.UserID == user
}

// keepRoutes is those of names the host defines, in their order, once each.
func keepRoutes(names, defined []string) []string {
	out := []string{}
	for _, n := range names {
		if slices.Contains(defined, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
