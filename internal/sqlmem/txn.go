package sqlmem

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

// txn.go — RFC AA Phase 3a: runtime-managed explicit transactions that span
// multiple tool calls. The agent opens a transaction (sql_begin), runs any
// number of sql_exec/sql_query on it, then sql_commit or sql_rollback. While a
// transaction is open for a (run, scope), the Manager routes that scope's ops
// onto it; with none open, ops auto-commit exactly as before.
//
// A transaction is keyed by txnID — RootRunID + scope + scope_id (see
// BuildTxnID) — so it is scoped to ONE run's view of ONE scope, and run-end
// cleanup reclaims it by RootRunID prefix. An open transaction PINS its scope
// connection (the backend's beginTx returns a release closure that drops the
// pin on finish), so the per-scope pool/handle is never evicted under a live
// transaction. Three lifecycle guarantees keep a held connection from leaking:
// explicit commit/rollback, run-end auto-rollback (RollbackRunTxns), and a TTL
// reaper.
//
// The transaction is replica-local (its connection lives on the replica that
// opened it). A run that migrates orphans the transaction → it is reaped, and
// the continuation auto-commits. Documented constraint.

const txnFieldSep = "\x1f"

// BuildTxnID derives the registry key for an explicit transaction from the
// run-tree root + the resolved scope. Keying off RootRunID (not the per-sub-run
// RunID) means the whole spawn tree shares one transaction per scope, and
// RollbackRunTxns(rootRunID) reclaims every transaction the tree opened.
func BuildTxnID(rootRunID, scope, scopeID string) string {
	return rootRunID + txnFieldSep + scope + txnFieldSep + scopeID
}

// openTxn is one live explicit transaction. release drops the scope-connection
// pin taken by the backend's beginTx; it MUST run exactly once, after the tx is
// committed or rolled back. mu serializes statements on the tx: tool calls in
// ONE agent turn dispatch concurrently (loop ToolParallelism), so two sql_exec
// blocks for the same scope reach this same *sql.Tx at once — and *sql.Tx is NOT
// safe for concurrent use. Every QueryTxn/ExecTxn and finish() holds mu.
type openTxn struct {
	mu      sync.Mutex
	tx      *sql.Tx
	key     ScopeKey
	runID   string // RootRunID, matched by RollbackRunTxns
	started time.Time
	release func()

	// savepoints is the LIFO SAVEPOINT stack for nested transactions (Phase 3b),
	// innermost last; spCounter mints fresh names so a released-then-re-pushed
	// level never reuses a name. depth reported to the agent is 1+len(savepoints)
	// (a freshly-opened txn is depth 1). Both guarded by mu. A whole-tx
	// commit/rollback (finish) discards every savepoint, so no per-savepoint
	// cleanup is needed on the reaper / run-end / Close paths.
	savepoints []string
	spCounter  int
}

// pushSavepoint opens a nested level: SAVEPOINT on the tx + push the name.
// Returns the new depth. Refuses past maxDepth (the stack cap). Holds mu so it
// can't race a statement / finish on the same tx.
func (t *openTxn) pushSavepoint(ctx context.Context, maxDepth int) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if maxDepth > 0 && len(t.savepoints) >= maxDepth {
		return 1 + len(t.savepoints), fmt.Errorf("transaction nesting depth limit (%d) reached — commit or roll back a nested level first", maxDepth)
	}
	t.spCounter++
	name := fmt.Sprintf("loomcycle_sp_%d", t.spCounter)
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+q(name)); err != nil {
		return 1 + len(t.savepoints), err
	}
	t.savepoints = append(t.savepoints, name)
	return 1 + len(t.savepoints), nil
}

// popSavepoint closes the innermost nested level: commit → RELEASE (merge up),
// rollback → ROLLBACK TO + RELEASE (undo + exit). Returns (newDepth, popped).
// popped is false when there is no savepoint (the caller then finishes the whole
// txn). Holds mu. SAVEPOINT/RELEASE/ROLLBACK TO are standard SQL on both tiers.
func (t *openTxn) popSavepoint(ctx context.Context, commit bool) (depth int, popped bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.savepoints) == 0 {
		return 0, false, nil
	}
	name := t.savepoints[len(t.savepoints)-1]
	if commit {
		_, err = t.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+q(name))
	} else {
		if _, e := t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+q(name)); e != nil {
			err = e
		} else {
			_, err = t.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+q(name))
		}
	}
	if err != nil {
		return 1 + len(t.savepoints), true, err
	}
	t.savepoints = t.savepoints[:len(t.savepoints)-1]
	return 1 + len(t.savepoints), true, nil
}

// finish commits or rolls back the transaction and always drops the pin. It
// takes mu so it can't race an in-flight statement on the same tx (a concurrent
// sql_commit + sql_exec in the same turn).
func (t *openTxn) finish(commit bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var err error
	if commit {
		err = t.tx.Commit()
	} else {
		err = t.tx.Rollback()
	}
	t.release()
	return err
}

// txnRegistry holds the process's open explicit transactions, keyed by txnID.
type txnRegistry struct {
	mu   sync.Mutex
	open map[string]*openTxn
}

func newTxnRegistry() *txnRegistry { return &txnRegistry{open: make(map[string]*openTxn)} }

// rollbackAll rolls back + releases every open transaction (Manager.Close).
func (r *txnRegistry) rollbackAll() {
	r.mu.Lock()
	all := r.open
	r.open = make(map[string]*openTxn)
	r.mu.Unlock()
	for id, t := range all {
		if err := t.finish(false); err != nil {
			log.Printf("sqlmem: rollback txn %q on close: %v", id, err)
		}
	}
}

// InTxn reports whether a fully-open explicit transaction exists for txnID. A
// nil entry is a reservation placeholder for a BeginTxn still mid-round-trip
// (see BeginTxn) — treated as NOT open, so a concurrent op auto-commits rather
// than routing onto a transaction that isn't ready.
func (m *Manager) InTxn(txnID string) bool {
	m.txns.mu.Lock()
	defer m.txns.mu.Unlock()
	return m.txns.open[txnID] != nil
}

// BeginTxn opens an explicit transaction for txnID — or, when one is already
// open for txnID, opens a nested level (a SAVEPOINT) on it (Phase 3b). Returns
// the resulting nesting depth (1 for a freshly-opened txn; 2+ for a nested one).
// Errors if the process-wide MaxOpenTxns cap is reached (new txn) or the
// per-txn MaxTxnDepth cap is reached (nested).
func (m *Manager) BeginTxn(ctx context.Context, txnID, rootRunID string, key ScopeKey) (int, error) {
	m.txns.mu.Lock()
	if existing, ok := m.txns.open[txnID]; ok {
		if existing == nil {
			// A concurrent BeginTxn for this id is mid-round-trip (the reservation
			// placeholder). Don't nest onto a txn that isn't ready.
			m.txns.mu.Unlock()
			return 0, fmt.Errorf("a transaction is being opened for this scope — retry")
		}
		// Nested begin → push a SAVEPOINT on the existing txn.
		m.txns.mu.Unlock()
		return existing.pushSavepoint(ctx, m.cfg.MaxTxnDepth)
	}
	if max := m.cfg.MaxOpenTxns; max > 0 && len(m.txns.open) >= max {
		m.txns.mu.Unlock()
		return 0, fmt.Errorf("too many open transactions (%d) — commit or rollback before opening more", max)
	}
	// Reserve the slot with a placeholder so a concurrent BeginTxn for the same
	// id can't also pass the check, then release the lock for the DB round-trip.
	m.txns.open[txnID] = nil
	m.txns.mu.Unlock()

	tx, release, err := m.backend.beginTx(ctx, key)
	if err != nil {
		m.txns.mu.Lock()
		delete(m.txns.open, txnID)
		m.txns.mu.Unlock()
		return 0, err
	}
	m.txns.mu.Lock()
	m.txns.open[txnID] = &openTxn{tx: tx, key: key, runID: rootRunID, started: time.Now(), release: release}
	m.txns.mu.Unlock()
	m.touch(key) // a transaction is durable-scope use (GC last_used)
	return 1, nil
}

// CommitTxn releases the innermost nested level, or — at depth 1 — commits +
// releases the whole transaction. Returns the resulting depth (0 = closed).
func (m *Manager) CommitTxn(txnID string) (int, error) {
	return m.finishLevel(txnID, true)
}

// RollbackTxn rolls back the innermost nested level (ROLLBACK TO + RELEASE, the
// outer txn continuing), or — at depth 1 — rolls back + releases the whole
// transaction. Returns the resulting depth (0 = closed).
func (m *Manager) RollbackTxn(txnID string) (int, error) {
	return m.finishLevel(txnID, false)
}

// finishLevel closes one level of txnID: a nested savepoint (depth stays open)
// or, at the root, the whole transaction. Lock discipline: m.txns.mu and the
// per-openTxn mu are never held simultaneously (BeginTxn takes m.txns.mu then
// the openTxn mu via pushSavepoint; here we take them sequentially in the same
// order), so there is no deadlock. A truly-concurrent root finish + nested begin
// on one scope is serialized but ordering is undefined (documented) — never
// corrupting: finish always releases the pin.
func (m *Manager) finishLevel(txnID string, commit bool) (int, error) {
	m.txns.mu.Lock()
	t := m.txns.open[txnID]
	m.txns.mu.Unlock()
	if t == nil {
		verb := "roll back"
		if commit {
			verb = "commit"
		}
		return 0, fmt.Errorf("no open transaction to %s for this scope", verb)
	}

	// Nested level → pop a savepoint and leave the txn open.
	if depth, popped, err := t.popSavepoint(context.Background(), commit); popped || err != nil {
		return depth, err
	}

	// Root level → remove from the registry (guarding against a concurrent
	// finish that already took it) and finish the whole transaction.
	m.txns.mu.Lock()
	if cur := m.txns.open[txnID]; cur == t {
		delete(m.txns.open, txnID)
	} else {
		m.txns.mu.Unlock()
		return 0, nil // another caller already finished it
	}
	m.txns.mu.Unlock()
	if err := t.finish(commit); err != nil {
		return 0, err
	}
	return 0, nil
}

// RollbackRunTxns rolls back every open transaction belonging to a run tree
// (matched by RootRunID). Called from the run-completion cleanup path so a run
// that ends mid-transaction never leaks a held connection.
//
// PRECONDITION: call only AFTER the run tree's goroutines have joined (the
// top-level loop returned, all executePendingTools / parallel_spawn children
// awaited). It skips the nil reservation placeholder of an in-flight BeginTxn,
// so a tree with a BeginTxn still mid-round-trip would leave that txn orphaned
// to the reaper. The current sole caller (purgeEphemeralVolumesForRun, gated
// meta.IsTopLevel, fired after loop.Run returns) satisfies this.
func (m *Manager) RollbackRunTxns(rootRunID string) {
	m.txns.mu.Lock()
	var victims []*openTxn
	for id, t := range m.txns.open {
		if t != nil && t.runID == rootRunID {
			victims = append(victims, t)
			delete(m.txns.open, id)
		}
	}
	m.txns.mu.Unlock()
	for _, t := range victims {
		if err := t.finish(false); err != nil {
			log.Printf("sqlmem: rollback run %q txn on completion: %v", rootRunID, err)
		}
	}
}

// QueryTxn runs a validated read-only statement on the open transaction for
// txnID. (An explicit transaction is read-write; read-safety rests on the
// validator's SELECT-only rule, not the auto-commit read-only-transaction.)
func (m *Manager) QueryTxn(ctx context.Context, txnID, statement string, args []any) (*QueryResult, error) {
	if err := validateStatementForDialect(statement, true, m.dialect); err != nil {
		return nil, err
	}
	t := m.txnFor(txnID)
	if t == nil {
		return nil, fmt.Errorf("no open transaction for this scope")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	qctx, cancel := withTimeout(m.cfg, ctx)
	defer cancel()
	rows, err := t.tx.QueryContext(qctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRows(rows, m.cfg.MaxRows)
}

// ExecTxn runs a validated DDL/DML statement on the open transaction for txnID,
// enforcing the quota before the write (measured on the transaction).
func (m *Manager) ExecTxn(ctx context.Context, txnID, statement string, args []any, quotaOverride int) (*ExecResult, error) {
	if err := validateStatementForDialect(statement, false, m.dialect); err != nil {
		return nil, err
	}
	t := m.txnFor(txnID)
	if t == nil {
		return nil, fmt.Errorf("no open transaction for this scope")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ectx, cancel := withTimeout(m.cfg, ctx)
	defer cancel()
	if quota := effectiveQuota(m.cfg, quotaOverride); quota > 0 {
		size, err := m.backend.txnSizeBytes(ectx, t.tx, t.key)
		if err != nil {
			return nil, fmt.Errorf("sqlmem: quota check: %w", err)
		}
		if size >= int64(quota) {
			return nil, fmt.Errorf("sqlmem: scope is at its quota (%d bytes >= %d) — delete rows or drop tables before writing", size, quota)
		}
	}
	r, err := t.tx.ExecContext(ectx, statement, args...)
	if err != nil {
		return nil, err
	}
	out := &ExecResult{}
	if n, err := r.RowsAffected(); err == nil {
		out.RowsAffected = n
	}
	if id, err := r.LastInsertId(); err == nil {
		out.LastInsertID = id
	}
	return out, nil
}

// txnFor returns the live open txn for txnID without removing it (so the txn
// stays open across many ExecTxn/QueryTxn calls). A reserved-but-not-yet-opened
// slot (nil) reads as absent.
func (m *Manager) txnFor(txnID string) *openTxn {
	m.txns.mu.Lock()
	defer m.txns.mu.Unlock()
	return m.txns.open[txnID]
}

// startReaper launches the abandoned-transaction sweeper (no-op when
// TxnTimeoutMS <= 0). It rolls back any transaction open longer than the TTL,
// so a stuck/abandoned agent can't hold a scope connection + locks forever.
func (m *Manager) startReaper() {
	m.reaperStop = make(chan struct{})
	if m.cfg.TxnTimeoutMS <= 0 {
		return // no goroutine; reaperDone stays nil
	}
	ttl := time.Duration(m.cfg.TxnTimeoutMS) * time.Millisecond
	tick := ttl / 2
	if tick < time.Second {
		tick = time.Second
	}
	if tick > 30*time.Second {
		tick = 30 * time.Second
	}
	m.reaperDone = make(chan struct{})
	// Capture stop/done as locals so the goroutine never re-reads the fields
	// (stopReaper nils them — that would be a data race on the field).
	stop, done := m.reaperStop, m.reaperDone
	go func() {
		defer close(done)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.reapStale(ttl)
			}
		}
	}()
}

// stopReaper signals the reaper and JOINS it, so no reap (a tx.Rollback +
// release) can run after Close proceeds to rollbackAll / backend.close().
func (m *Manager) stopReaper() {
	if m.reaperStop != nil {
		close(m.reaperStop)
		m.reaperStop = nil
	}
	if m.reaperDone != nil {
		<-m.reaperDone // wait for the goroutine (incl. any in-flight reap) to exit
		m.reaperDone = nil
	}
}

// reapStale rolls back + releases every transaction open longer than ttl.
func (m *Manager) reapStale(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	m.txns.mu.Lock()
	var victims []*openTxn
	for id, t := range m.txns.open {
		if t != nil && t.started.Before(cutoff) {
			victims = append(victims, t)
			delete(m.txns.open, id)
		}
	}
	m.txns.mu.Unlock()
	for _, t := range victims {
		if err := t.finish(false); err != nil {
			log.Printf("sqlmem: reap stale txn: %v", err)
		}
	}
}
