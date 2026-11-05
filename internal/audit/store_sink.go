package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Appender is the store surface a StoreSink writes through.
type Appender interface {
	AuditAppend(ctx context.Context, rows []store.AuditEventRow, seal store.AuditSealFunc, opened *store.AuditEventRow) ([]store.AuditEventRow, error)
}

// syncAppendTimeout bounds a caller's wait for a state change's append,
// since Sink.Record carries no context: a stalled database must not hold the
// operation being recorded for long. The queue's writer, which no caller
// waits on, allows longer per batch.
const (
	syncAppendTimeout  = 5 * time.Second
	writerBatchTimeout = 10 * time.Second
	// writerBatch is how many queued observations the writer appends per
	// pass, one transaction per tenant among them.
	writerBatch = 64
	// DefaultQueueCapacity is the observation queue's size when StartAsync
	// is given none.
	DefaultQueueCapacity = 4096
)

// ActionEventsDropped is the operator-chain summary of observations the
// queue dropped.
const ActionEventsDropped = "audit_events_dropped"

// IsObservation reports whether an action is an observation — something
// refused, denied, looked up or used, recorded per request or per tool call
// and caused by whoever sends it — rather than a state change. Observations
// are queued and written by one writer per process, so a flood of them (a
// burst of failed sign-ins) cannot hold the database's connections or a
// chain's head lock against the requests behind it; a state change (a token
// minted, a rule edited, a user approved, an erasure) is written before
// Record returns. An action not listed here is a state change: the safe
// default for an audit record is to be waited for.
func IsObservation(action string) bool {
	switch action {
	case "auth_login_failed", "auth_assertion_refused", "auth_downgrade_refused", "auth_gate_sync_refused",
		"auth_link_refused", "auth_manage_refused", "auth_login_locked",
		"oauth_refused", "oauth_token_refused", "oauth_code_reuse", "oauth_refresh_reuse",
		"acl_denied", "acl_unavailable", "acl_shadow_diverged", "acl_resolution_failed",
		"identity_lookup", "crypto_op", "sql_query", "sql_exec":
		return true
	}
	return strings.HasPrefix(action, "git_")
}

// StoreSink records events into the store-backed chain.
type StoreSink struct {
	store Appender
	keys  ChainKeys
	now   func() time.Time

	mu      sync.Mutex
	queue   chan Event // nil until StartAsync: every event is written at once
	closed  bool
	done    chan struct{}
	dropped map[string]int
}

// NewStoreSink returns a sink appending to s, signing with keys. It refuses
// without a current key: an unsigned chain would promise an integrity it
// cannot keep against a database writer. Until StartAsync, every event is
// written before Record returns.
func NewStoreSink(s Appender, keys ChainKeys) (*StoreSink, error) {
	if s == nil {
		return nil, errors.New("audit: no store")
	}
	if len(keys.Current) == 0 {
		return nil, errors.New("audit: the store-backed audit stream signs its chain with a key derived from LOOMCYCLE_SECRET_KEY, which is not set")
	}
	return &StoreSink{store: s, keys: keys, now: time.Now, dropped: map[string]int{}}, nil
}

// StartAsync starts the observation queue (capacity events; 0 takes the
// default) and its single writer. Call Close to drain it.
func (s *StoreSink) StartAsync(capacity int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue != nil || s.closed {
		return
	}
	if capacity <= 0 {
		capacity = DefaultQueueCapacity
	}
	s.queue = make(chan Event, capacity)
	s.done = make(chan struct{})
	go s.writer(s.queue, s.done)
}

// Close stops queueing (later observations are written at once), and waits
// until the writer has written what was queued, and the drop summary, or ctx
// ends.
func (s *StoreSink) Close(ctx context.Context) error {
	s.mu.Lock()
	q, done := s.queue, s.done
	if q != nil && !s.closed {
		close(q)
	}
	s.closed = true
	s.mu.Unlock()
	if q == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Record appends ev to its tenant's chain: a state change before it returns,
// an observation through the queue (never waited for; dropped and counted
// when the queue is full).
func (s *StoreSink) Record(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = s.now().UTC()
	}
	if IsObservation(ev.Action) && s.enqueue(ev) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncAppendTimeout)
	defer cancel()
	return s.appendEvents(ctx, []Event{ev})
}

// enqueue queues an observation, or counts it dropped when the queue is
// full. It reports false when there is no running queue, and the caller
// writes the event itself.
func (s *StoreSink) enqueue(ev Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue == nil || s.closed {
		return false
	}
	select {
	case s.queue <- ev:
	default:
		s.dropped[ev.Action]++
	}
	return true
}

func (s *StoreSink) writer(q <-chan Event, done chan<- struct{}) {
	defer close(done)
	for ev := range q {
		batch := []Event{ev}
	drain:
		for len(batch) < writerBatch {
			select {
			case e, ok := <-q:
				if !ok {
					break drain
				}
				batch = append(batch, e)
			default:
				break drain
			}
		}
		s.writeBatch(batch)
		s.writeDropSummary()
	}
	s.writeDropSummary()
}

// writeBatch appends a batch of observations, one transaction per tenant in
// the order each tenant first appears, each tenant's events in queue order.
// A failed append counts its events as dropped.
func (s *StoreSink) writeBatch(batch []Event) {
	byTenant := map[string][]Event{}
	var order []string
	for _, ev := range batch {
		t := SafeText(ChainTenant(ev))
		if _, seen := byTenant[t]; !seen {
			order = append(order, t)
		}
		byTenant[t] = append(byTenant[t], ev)
	}
	for _, t := range order {
		ctx, cancel := context.WithTimeout(context.Background(), writerBatchTimeout)
		err := s.appendEvents(ctx, byTenant[t])
		cancel()
		if err != nil {
			slog.Error("audit: writing queued observations failed; counted as dropped", "tenant", t, "events", len(byTenant[t]), "err", err)
			s.mu.Lock()
			for _, ev := range byTenant[t] {
				s.dropped[ev.Action]++
			}
			s.mu.Unlock()
		}
	}
}

// writeDropSummary writes the drops counted so far as one operator-chain
// row, and keeps them counted if it cannot.
func (s *StoreSink) writeDropSummary() {
	s.mu.Lock()
	if len(s.dropped) == 0 {
		s.mu.Unlock()
		return
	}
	counts := s.dropped
	s.dropped = map[string]int{}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), writerBatchTimeout)
	defer cancel()
	if err := s.appendEvents(ctx, []Event{{TS: s.now().UTC(), Action: ActionEventsDropped, DroppedCounts: counts}}); err != nil {
		slog.Error("audit: writing the dropped-observations summary failed", "err", err)
		s.mu.Lock()
		for a, n := range counts {
			s.dropped[a] += n
		}
		s.mu.Unlock()
	}
}

// appendEvents appends events of one tenant's chain in one transaction.
func (s *StoreSink) appendEvents(ctx context.Context, evs []Event) error {
	rows := make([]store.AuditEventRow, 0, len(evs))
	for _, ev := range evs {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("audit: marshal: %w", err)
		}
		rows = append(rows, RowFor(ev, payload))
	}
	var opened *store.AuditEventRow
	if rows[0].TenantID != "" {
		o, err := openedRow(rows[0].TenantID, evs[0].TS)
		if err != nil {
			return err
		}
		opened = &o
	}
	if _, err := s.store.AuditAppend(ctx, rows, s.keys.Seal(), opened); err != nil {
		return fmt.Errorf("audit: append: %w", err)
	}
	return nil
}

// openedRow is the operator-chain record that tenant's chain exists.
func openedRow(tenant string, ts time.Time) (store.AuditEventRow, error) {
	ev := Event{TS: ts, Action: ActionChainOpened, TargetTenant: tenant}
	payload, err := json.Marshal(ev)
	if err != nil {
		return store.AuditEventRow{}, fmt.Errorf("audit: marshal: %w", err)
	}
	row := RowFor(ev, payload)
	row.TenantID, row.Subject = "", tenant
	return row, nil
}
