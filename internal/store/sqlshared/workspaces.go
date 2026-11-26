package sqlshared

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Workspace* implements store.WorkspaceStore (RFC GW) for both stores. See
// internal/store/workspaces.go for the contract.

// workspaceColNames is the column order scanWorkspace reads.
var workspaceColNames = []string{"id", "tenant_id", "user_id", "name", "worker_id", "kind", "lifetime", "engine",
	"owner_run_id", "owner_agent_id", "root_run_id", "state", "shares", "owner_kind", "opened_token", "last_used_at", "egress", "root", "git", "created_at", "updated_at"}

var workspaceCols = workspaceColList("")

// workspaceColList is the column list, each qualified by alias when one is
// given (for a select that joins a table with columns of the same names).
func workspaceColList(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return prefix + strings.Join(workspaceColNames, ", "+prefix)
}

func scanWorkspace(d Dialect, sc Row) (store.WorkspaceRow, error) {
	var (
		r      store.WorkspaceRow
		state  string
		shares string
		egress string
		git    string
	)
	if err := sc.Scan(&r.ID, &r.TenantID, &r.UserID, &r.Name, &r.WorkerID, &r.Kind, &r.Lifetime, &r.Engine,
		&r.OwnerRunID, &r.OwnerAgentID, &r.RootRunID, &state, &shares, &r.OwnerKind, &r.OpenedToken,
		d.TimeDest(&r.LastUsedAt), &egress, &r.Root, &git, d.TimeDest(&r.CreatedAt), d.TimeDest(&r.UpdatedAt)); err != nil {
		return store.WorkspaceRow{}, err
	}
	r.State = store.WorkspaceState(state)
	// A column that does not decode gives no shares: closed, never a share
	// read wider than it was written.
	if err := json.Unmarshal([]byte(shares), &r.Shares); err != nil || r.Shares == nil {
		r.Shares = []store.WorkspaceShare{}
	}
	// Likewise a column that does not decode gives no route: no way out,
	// never a wider one.
	if err := json.Unmarshal([]byte(egress), &r.Egress); err != nil || r.Egress == nil {
		r.Egress = []string{}
	}
	r.Git = decodeWorkspaceGit(git)
	r.CreatedAt, r.UpdatedAt, r.LastUsedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC(), r.LastUsedAt.UTC()
	return r, nil
}

func collectWorkspaces(d Dialect, rows Rows) ([]store.WorkspaceRow, error) {
	defer rows.Close()
	out := []store.WorkspaceRow{}
	for rows.Next() {
		r, err := scanWorkspace(d, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WorkspaceCreate inserts a new row. It never upserts: a second open of one
// name in one run is the caller's error to report, not a row to overwrite.
func WorkspaceCreate(ctx context.Context, q Querier, row store.WorkspaceRow) (store.WorkspaceRow, error) {
	if row.ID == "" || row.OwnerRunID == "" || row.Name == "" || row.WorkerID == "" {
		return store.WorkspaceRow{}, fmt.Errorf("workspace create: id, owner_run_id, name and worker_id are required")
	}
	if row.State == "" {
		row.State = store.WorkspaceOpening
	}
	if !row.State.IsValid() {
		return store.WorkspaceRow{}, fmt.Errorf("workspace create: unknown state %q", row.State)
	}
	if row.Kind == "" {
		row.Kind = store.WorkspaceKindGeneral
	}
	if row.Lifetime == "" {
		row.Lifetime = store.WorkspaceLifetimeEphemeral
	}
	if row.OwnerKind == "" {
		row.OwnerKind = store.WorkspaceOwnerRun
	}
	if row.OwnerKind != store.WorkspaceOwnerRun && row.OwnerKind != store.WorkspaceOwnerPrincipal {
		return store.WorkspaceRow{}, fmt.Errorf("workspace create: unknown owner kind %q", row.OwnerKind)
	}
	// A durable row is kept under its user (RFC GW §4.2): one with none
	// would share the durable name of every user-less run.
	if row.Lifetime == store.WorkspaceLifetimeDurable && row.UserID == "" {
		return store.WorkspaceRow{}, fmt.Errorf("workspace create: a durable workspace needs a user")
	}
	// A root is one user's own folder (RFC GW P13): a row with no user would
	// match every user-less run. It is kept on that user's disk already, so
	// never durable.
	if row.Root != "" && row.UserID == "" {
		return store.WorkspaceRow{}, fmt.Errorf("workspace create: a root workspace needs a user")
	}
	if row.Root != "" && row.Lifetime == store.WorkspaceLifetimeDurable {
		return store.WorkspaceRow{}, fmt.Errorf("workspace create: a root workspace is not durable")
	}
	d := q.Dialect()
	now := time.Now().UTC()
	_, err := q.Exec(ctx, `INSERT INTO workspaces (`+workspaceCols+`)
		VALUES (`+Placeholders(len(workspaceColNames))+`)`,
		row.ID, row.TenantID, row.UserID, row.Name, row.WorkerID, row.Kind, row.Lifetime, row.Engine,
		row.OwnerRunID, row.OwnerAgentID, row.RootRunID, string(row.State), encodeShares(row.Shares),
		row.OwnerKind, row.OpenedToken, d.TimeArg(now), encodeEgress(row.Egress), row.Root, encodeWorkspaceGit(row.Git), d.TimeArg(now), d.TimeArg(now))
	if IsUniqueViolation(err) {
		return store.WorkspaceRow{}, &store.ErrConflict{Kind: "workspace", ID: row.Name}
	}
	if err != nil {
		return store.WorkspaceRow{}, fmt.Errorf("workspace create: %w", err)
	}
	return WorkspaceGet(ctx, q, row.TenantID, row.ID)
}

func WorkspaceGet(ctx context.Context, q Querier, tenantID, id string) (store.WorkspaceRow, error) {
	d := q.Dialect()
	r, err := scanWorkspace(d, q.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE tenant_id = ? AND id = ?`, tenantID, id))
	if err != nil {
		return store.WorkspaceRow{}, NotFound(err, "workspace", id)
	}
	return r, nil
}

func WorkspaceGetByName(ctx context.Context, q Querier, tenantID, ownerRunID, name string) (store.WorkspaceRow, error) {
	d := q.Dialect()
	r, err := scanWorkspace(d, q.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE owner_run_id = ? AND name = ? AND tenant_id = ?`,
		ownerRunID, name, tenantID))
	if err != nil {
		return store.WorkspaceRow{}, NotFound(err, "workspace", name)
	}
	return r, nil
}

func WorkspaceListByOwnerRun(ctx context.Context, q Querier, tenantID, ownerRunID string) ([]store.WorkspaceRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE owner_run_id = ? AND tenant_id = ? ORDER BY name`,
		ownerRunID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("workspace list by owner run: %w", err)
	}
	return collectWorkspaces(d, rows)
}

func WorkspaceListByWorker(ctx context.Context, q Querier, workerID string, states ...store.WorkspaceState) ([]store.WorkspaceRow, error) {
	d := q.Dialect()
	query := `SELECT ` + workspaceCols + ` FROM workspaces WHERE worker_id = ?`
	args := []any{workerID}
	if len(states) > 0 {
		query += ` AND state IN (` + Placeholders(len(states)) + `)`
		args = append(args, stateArgs(states)...)
	}
	rows, err := q.Query(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("workspace list by worker: %w", err)
	}
	return collectWorkspaces(d, rows)
}

// WorkspaceSetState is a compare-and-set on state: the WHERE names the state
// the caller read, so a row another replica moved first is left alone.
func WorkspaceSetState(ctx context.Context, q Querier, tenantID, id string, from, to store.WorkspaceState) (bool, error) {
	if !to.IsValid() {
		return false, fmt.Errorf("workspace set state: unknown state %q", to)
	}
	d := q.Dialect()
	n, err := q.Exec(ctx,
		`UPDATE workspaces SET state = ?, updated_at = ? WHERE tenant_id = ? AND id = ? AND state = ?`,
		string(to), d.TimeArg(time.Now().UTC()), tenantID, id, string(from))
	if err != nil {
		return false, fmt.Errorf("workspace set state: %w", err)
	}
	return n > 0, nil
}

// WorkspaceDelete removes the row and its exposures in one transaction: an
// exposure never outlives its workspace (RFC GW §5.3).
func WorkspaceDelete(ctx context.Context, db DB, tenantID, id string) (bool, error) {
	var n int64
	err := db.InTx(ctx, func(tx Querier) error {
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_exposures WHERE tenant_id = ? AND workspace_id = ?`, tenantID, id); err != nil {
			return err
		}
		var err error
		n, err = tx.Exec(ctx, `DELETE FROM workspaces WHERE tenant_id = ? AND id = ?`, tenantID, id)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("workspace delete: %w", err)
	}
	return n > 0, nil
}

func WorkspaceCountActiveByWorker(ctx context.Context, q Querier, workerID string) (int, error) {
	return countActiveWorkspaces(ctx, q, "worker_id", workerID)
}

func WorkspaceCountActiveByTenant(ctx context.Context, q Querier, tenantID string) (int, error) {
	return countActiveWorkspaces(ctx, q, "tenant_id", tenantID)
}

// countActiveWorkspaces counts rows in store.WorkspaceActiveStates where col
// (a fixed column name, never caller text) equals v. A parked durable row
// holds no place: a worker's capacity counts pinned workspaces (RFC GW §8).
func countActiveWorkspaces(ctx context.Context, q Querier, col, v string) (int, error) {
	args := append([]any{v, store.WorkspaceOwnerParked}, stateArgs(store.WorkspaceActiveStates)...)
	var n int
	if err := q.QueryRow(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE `+col+` = ? AND owner_kind <> ? AND state IN (`+Placeholders(len(store.WorkspaceActiveStates))+`)`,
		args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("workspace count by %s: %w", col, err)
	}
	return n, nil
}

// WorkspaceSweepCandidates is modelled on EphemeralVolumeSweepCandidates but
// keyed on the OWNER run, not the root run: a workspace is pinned to the run
// that opened it (RFC GW §4.2), so a run beneath it still running holds
// nothing, and the walk up parent_run_id that query needs is not here. The
// paused/pausing filter is the same, for the same reason: a paused run is
// parked, not ended, and resumes into its workspaces. A row whose owner run
// row is gone (a deleted run tree) has an owner that can never run again, so
// it is a candidate too: the LEFT JOIN finds no run for it.
func WorkspaceSweepCandidates(ctx context.Context, q Querier) ([]store.WorkspaceRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx,
		`SELECT `+workspaceColList("w")+`
		 FROM workspaces w
		 LEFT JOIN runs r ON r.id = w.owner_run_id
		 WHERE w.owner_kind = 'run'
		   AND (r.id IS NULL
		    OR (r.status IN (`+Placeholders(len(store.TerminalRunStatuses))+`)
		        AND COALESCE(r.pause_state, 'running') NOT IN ('paused', 'pausing')))
		 ORDER BY w.created_at, w.id`, store.TerminalRunStatusArgs()...)
	if err != nil {
		return nil, fmt.Errorf("workspace sweep candidates: %w", err)
	}
	return collectWorkspaces(d, rows)
}

// WorkspaceTouchByWorker stamps updated_at on a worker's active rows: the
// dispatcher's mark, every replica it is attached to, that the worker is
// there (WorkspaceListStale).
func WorkspaceTouchByWorker(ctx context.Context, q Querier, workerID string) (int, error) {
	d := q.Dialect()
	args := append([]any{d.TimeArg(time.Now().UTC()), workerID}, stateArgs(store.WorkspaceActiveStates)...)
	n, err := q.Exec(ctx,
		`UPDATE workspaces SET updated_at = ? WHERE worker_id = ? AND state IN (`+Placeholders(len(store.WorkspaceActiveStates))+`)`,
		args...)
	if err != nil {
		return 0, fmt.Errorf("workspace touch by worker: %w", err)
	}
	return int(n), nil
}

// WorkspaceListStale returns the active rows not touched since before, oldest
// first.
func WorkspaceListStale(ctx context.Context, q Querier, before time.Time) ([]store.WorkspaceRow, error) {
	d := q.Dialect()
	args := append([]any{d.TimeArg(before.UTC())}, stateArgs(store.WorkspaceActiveStates)...)
	rows, err := q.Query(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE updated_at < ? AND state IN (`+Placeholders(len(store.WorkspaceActiveStates))+`)
		 ORDER BY updated_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("workspace list stale: %w", err)
	}
	return collectWorkspaces(d, rows)
}

// encodeShares is the shares column's text. A nil or empty list is "[]", the
// column's default, so a row never written to compares equal to an empty
// list read back.
func encodeShares(shares []store.WorkspaceShare) string {
	if len(shares) == 0 {
		return "[]"
	}
	b, err := json.Marshal(shares)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// encodeEgress is the egress column's text: "[]", the column's default, for
// none.
func encodeEgress(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// WorkspaceSetShares is a compare-and-set on the shares column: the WHERE
// names the list the caller read, so a share made meanwhile by another call
// is not overwritten. Times are stored in UTC so a list read back encodes to
// the same text it was written as.
func WorkspaceSetShares(ctx context.Context, q Querier, tenantID, id string, from, to []store.WorkspaceShare) (bool, error) {
	d := q.Dialect()
	for i := range to {
		to[i].CreatedAt = to[i].CreatedAt.UTC()
	}
	n, err := q.Exec(ctx,
		`UPDATE workspaces SET shares = ?, updated_at = ? WHERE tenant_id = ? AND id = ? AND shares = ?`,
		encodeShares(to), d.TimeArg(time.Now().UTC()), tenantID, id, encodeShares(from))
	if err != nil {
		return false, fmt.Errorf("workspace set shares: %w", err)
	}
	return n > 0, nil
}

func WorkspaceListByOwnerAgent(ctx context.Context, q Querier, tenantID, ownerAgentID, name string) ([]store.WorkspaceRow, error) {
	if ownerAgentID == "" {
		return []store.WorkspaceRow{}, nil
	}
	d := q.Dialect()
	rows, err := q.Query(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE tenant_id = ? AND owner_agent_id = ? AND name = ? ORDER BY created_at, id`,
		tenantID, ownerAgentID, name)
	if err != nil {
		return nil, fmt.Errorf("workspace list by owner agent: %w", err)
	}
	return collectWorkspaces(d, rows)
}

// WorkspaceListSharedWith narrows by a LIKE on the shares text and then keeps
// only rows whose decoded shares name the run exactly: the LIKE only has to
// over-match, never under-match, so its escaping is belt and braces.
func WorkspaceListSharedWith(ctx context.Context, q Querier, tenantID, runID string) ([]store.WorkspaceRow, error) {
	if runID == "" {
		return []store.WorkspaceRow{}, nil
	}
	d := q.Dialect()
	needle, _ := json.Marshal(runID)
	pattern := "%" + likeEscape(`"run_id":`+string(needle)) + "%"
	rows, err := q.Query(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE tenant_id = ? AND shares LIKE ? ESCAPE '!' ORDER BY name, id`,
		tenantID, pattern)
	if err != nil {
		return nil, fmt.Errorf("workspace list shared with: %w", err)
	}
	all, err := collectWorkspaces(d, rows)
	if err != nil {
		return nil, err
	}
	out := []store.WorkspaceRow{}
	for _, r := range all {
		for _, sh := range r.Shares {
			if sh.RunID == runID {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

// likeEscape escapes LIKE's wildcards and the escape character '!'.
func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

func stateArgs(states []store.WorkspaceState) []any {
	out := make([]any, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

func WorkspaceListPrincipalOwned(ctx context.Context, q Querier) ([]store.WorkspaceRow, error) {
	d := q.Dialect()
	rows, err := q.Query(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE owner_kind = ? ORDER BY created_at, id`, store.WorkspaceOwnerPrincipal)
	if err != nil {
		return nil, fmt.Errorf("workspace list principal owned: %w", err)
	}
	return collectWorkspaces(d, rows)
}

func WorkspaceTouchUsed(ctx context.Context, q Querier, tenantID, id string, at time.Time) error {
	d := q.Dialect()
	if _, err := q.Exec(ctx, `UPDATE workspaces SET last_used_at = ? WHERE tenant_id = ? AND id = ?`,
		d.TimeArg(at.UTC()), tenantID, id); err != nil {
		return fmt.Errorf("workspace touch used: %w", err)
	}
	return nil
}

// WorkspaceGetDurable reads the durable row of (tenant, user, name), which
// the partial unique index keeps to one. A row with no user is never
// durable (WorkspaceCreate), so an empty user finds nothing.
func WorkspaceGetDurable(ctx context.Context, q Querier, tenantID, userID, name string) (store.WorkspaceRow, error) {
	if userID == "" || name == "" {
		return store.WorkspaceRow{}, &store.ErrNotFound{Kind: "workspace", ID: name}
	}
	d := q.Dialect()
	r, err := scanWorkspace(d, q.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE tenant_id = ? AND user_id = ? AND name = ? AND lifetime = ?`,
		tenantID, userID, name, store.WorkspaceLifetimeDurable))
	if err != nil {
		return store.WorkspaceRow{}, NotFound(err, "workspace", name)
	}
	return r, nil
}

// WorkspacePin is a compare-and-set on owner_run_id: the WHERE names the
// owner the caller read (the parked key, or a run that ended), so of two
// pins racing on two replicas one moves the row and the other finds it
// moved.
func WorkspacePin(ctx context.Context, q Querier, tenantID, id, fromOwnerRunID string, to store.WorkspaceOwner) (bool, error) {
	if fromOwnerRunID == "" || to.RunID == "" {
		return false, fmt.Errorf("workspace pin: the owner read and the run to pin to are required")
	}
	d := q.Dialect()
	n, err := q.Exec(ctx,
		`UPDATE workspaces SET owner_kind = ?, owner_run_id = ?, owner_agent_id = ?, root_run_id = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND lifetime = ? AND owner_run_id = ?`,
		store.WorkspaceOwnerRun, to.RunID, to.AgentID, to.RootRunID, d.TimeArg(time.Now().UTC()),
		tenantID, id, store.WorkspaceLifetimeDurable, fromOwnerRunID)
	if IsUniqueViolation(err) {
		return false, &store.ErrConflict{Kind: "workspace", ID: id}
	}
	if err != nil {
		return false, fmt.Errorf("workspace pin: %w", err)
	}
	return n > 0, nil
}

// WorkspaceUnpin parks a durable row its run holds: the parked key, which
// no run id can be, and no agent, root or shares, so no run-keyed read
// reaches it until the next pin (RFC GW P12 plan D-P12-1).
func WorkspaceUnpin(ctx context.Context, q Querier, tenantID, id, fromRunID string) (bool, error) {
	if fromRunID == "" {
		return false, fmt.Errorf("workspace unpin: the run that holds it is required")
	}
	d := q.Dialect()
	n, err := q.Exec(ctx,
		`UPDATE workspaces SET owner_kind = ?, owner_run_id = ?, owner_agent_id = '', root_run_id = '', shares = '[]', updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND lifetime = ? AND owner_kind = ? AND owner_run_id = ?`,
		store.WorkspaceOwnerParked, store.WorkspaceParkedKey(id), d.TimeArg(time.Now().UTC()),
		tenantID, id, store.WorkspaceLifetimeDurable, store.WorkspaceOwnerRun, fromRunID)
	if err != nil {
		return false, fmt.Errorf("workspace unpin: %w", err)
	}
	return n > 0, nil
}

// encodeWorkspaceGit is the git column's text: "" for a workspace that is not
// a git one.
func encodeWorkspaceGit(g *store.WorkspaceGit) string {
	if g == nil {
		return ""
	}
	b, err := json.Marshal(g)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeWorkspaceGit reads the git column. One that does not decode gives no
// repository: the git ops then refuse the workspace, never act on a guess.
func decodeWorkspaceGit(s string) *store.WorkspaceGit {
	if s == "" {
		return nil
	}
	var g store.WorkspaceGit
	if err := json.Unmarshal([]byte(s), &g); err != nil || g.URL == "" {
		return nil
	}
	return &g
}

// WorkspaceSetGit replaces the tenant's row's git record.
func WorkspaceSetGit(ctx context.Context, q Querier, tenantID, id string, git store.WorkspaceGit) (bool, error) {
	if git.URL == "" {
		return false, fmt.Errorf("workspace set git: a repository URL is required")
	}
	d := q.Dialect()
	n, err := q.Exec(ctx, `UPDATE workspaces SET git = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		encodeWorkspaceGit(&git), d.TimeArg(time.Now().UTC()), tenantID, id)
	if err != nil {
		return false, fmt.Errorf("workspace set git: %w", err)
	}
	return n > 0, nil
}
