package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Appender is the store surface a StoreSink writes through.
type Appender interface {
	AuditAppend(ctx context.Context, rows []store.AuditEventRow, seal store.AuditSealFunc, opened *store.AuditEventRow) ([]store.AuditEventRow, error)
}

// storeAppendTimeout bounds one append, since Sink.Record carries no context:
// a stalled database must not hold the operation being recorded forever.
const storeAppendTimeout = 10 * time.Second

// StoreSink records events into the store-backed chain.
type StoreSink struct {
	store Appender
	keys  ChainKeys
	now   func() time.Time
}

// NewStoreSink returns a sink appending to s, signing with keys. It refuses
// without a current key: an unsigned chain would promise an integrity it
// cannot keep against a database writer.
func NewStoreSink(s Appender, keys ChainKeys) (*StoreSink, error) {
	if s == nil {
		return nil, errors.New("audit: no store")
	}
	if len(keys.Current) == 0 {
		return nil, errors.New("audit: the store-backed audit stream signs its chain with a key derived from LOOMCYCLE_SECRET_KEY, which is not set")
	}
	return &StoreSink{store: s, keys: keys, now: time.Now}, nil
}

// Record appends ev to its tenant's chain.
func (s *StoreSink) Record(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = s.now().UTC()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeAppendTimeout)
	defer cancel()
	row := RowFor(ev, payload)
	var opened *store.AuditEventRow
	if row.TenantID != "" {
		o, err := openedRow(row.TenantID, ev.TS)
		if err != nil {
			return err
		}
		opened = &o
	}
	if _, err := s.store.AuditAppend(ctx, []store.AuditEventRow{row}, s.keys.Seal(), opened); err != nil {
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
