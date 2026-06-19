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
// committed or rolled back.
type openTxn struct {
	tx      *sql.Tx
	key     ScopeKey
	runID   string // RootRunID, for RollbackRunTxns prefix matching
	started time.Time
	release func()
}

// finish commits or rolls back the transaction and always drops the pin.
func (t *openTxn) finish(commit bool) error {
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

// InTxn reports whether an explicit transaction is open for txnID.
func (m *Manager) InTxn(txnID string) bool {
	m.txns.mu.Lock()
	defer m.txns.mu.Unlock()
	_, ok := m.txns.open[txnID]
	return ok
}

// BeginTxn opens an explicit transaction for txnID against scope key. Errors if
// one is already open for txnID or the process-wide MaxOpenTxns cap is reached.
func (m *Manager) BeginTxn(ctx context.Context, txnID, rootRunID string, key ScopeKey) error {
	m.txns.mu.Lock()
	if _, ok := m.txns.open[txnID]; ok {
		m.txns.mu.Unlock()
		return fmt.Errorf("a transaction is already open for this scope — commit or rollback it before starting another")
	}
	if max := m.cfg.MaxOpenTxns; max > 0 && len(m.txns.open) >= max {
		m.txns.mu.Unlock()
		return fmt.Errorf("too many open transactions (%d) — commit or rollback before opening more", max)
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
		return err
	}
	m.txns.mu.Lock()
	m.txns.open[txnID] = &openTxn{tx: tx, key: key, runID: rootRunID, started: time.Now(), release: release}
	m.txns.mu.Unlock()
	return nil
}

// take removes and returns the open txn for txnID (nil if none / still being
// opened). Used by Commit/Rollback so the same txn is never finished twice.
func (m *Manager) take(txnID string) *openTxn {
	m.txns.mu.Lock()
	defer m.txns.mu.Unlock()
	t := m.txns.open[txnID]
	if t == nil {
		return nil
	}
	delete(m.txns.open, txnID)
	return t
}

// CommitTxn commits + releases the open transaction for txnID.
func (m *Manager) CommitTxn(txnID string) error {
	t := m.take(txnID)
	if t == nil {
		return fmt.Errorf("no open transaction to commit for this scope")
	}
	return t.finish(true)
}

// RollbackTxn rolls back + releases the open transaction for txnID.
func (m *Manager) RollbackTxn(txnID string) error {
	t := m.take(txnID)
	if t == nil {
		return fmt.Errorf("no open transaction to roll back for this scope")
	}
	return t.finish(false)
}

// RollbackRunTxns rolls back every open transaction belonging to a run tree
// (matched by RootRunID). Called from the run-completion cleanup path so a run
// that ends mid-transaction never leaks a held connection.
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
	stop := make(chan struct{})
	m.reaperStop = stop
	if m.cfg.TxnTimeoutMS <= 0 {
		return
	}
	ttl := time.Duration(m.cfg.TxnTimeoutMS) * time.Millisecond
	tick := ttl / 2
	if tick < time.Second {
		tick = time.Second
	}
	if tick > 30*time.Second {
		tick = 30 * time.Second
	}
	// Capture `stop` as a local so the goroutine never re-reads m.reaperStop
	// (which stopReaper nils — that would be a data race on the field).
	go func() {
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

func (m *Manager) stopReaper() {
	if m.reaperStop != nil {
		close(m.reaperStop)
		m.reaperStop = nil
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
