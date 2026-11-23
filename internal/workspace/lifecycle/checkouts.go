package lifecycle

import (
	"context"
	"sync"
	"time"
)

// The runtime's record of each File op=checkout of Documents into a
// workspace (RFC GW P7). A checkout also writes a manifest into the
// workspace, but a command there can rewrite that file; a commit trusts
// only this record, which no command reaches. It is kept in this process,
// as the conversion quarantine is: a workspace is pinned to one run on one
// replica, and a record lost to a restart makes its commit refuse, never
// trust the manifest (durable workspaces, RFC GW P12, will need it kept).

// maxCheckoutRecords bounds the records kept; past it the oldest goes.
const maxCheckoutRecords = 128

type checkoutRecord struct {
	data []byte
	at   time.Time
}

type checkouts struct {
	mu   sync.Mutex
	recs map[string]checkoutRecord
}

// RememberCheckout keeps rec as the record of checkout id in the run's
// workspace name, replacing one kept under that id. rec is the caller's,
// opaque here; it is copied.
func (m *Manager) RememberCheckout(ctx context.Context, o Owner, name, id string, rec []byte) error {
	row, err := m.Get(ctx, o, name)
	if err != nil {
		return err
	}
	c := &m.checkouts
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recs == nil {
		c.recs = map[string]checkoutRecord{}
	}
	key := row.ID + "|" + id
	if _, ok := c.recs[key]; !ok && len(c.recs) >= maxCheckoutRecords {
		oldK, oldT := "", time.Time{}
		for k, r := range c.recs {
			if oldK == "" || r.at.Before(oldT) {
				oldK, oldT = k, r.at
			}
		}
		delete(c.recs, oldK)
	}
	c.recs[key] = checkoutRecord{data: append([]byte(nil), rec...), at: m.now()}
	return nil
}

// Checkout returns the record of checkout id in the run's workspace name;
// ok is false when this runtime keeps none.
func (m *Manager) Checkout(ctx context.Context, o Owner, name, id string) (rec []byte, ok bool, err error) {
	row, err := m.Get(ctx, o, name)
	if err != nil {
		return nil, false, err
	}
	c := &m.checkouts
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.recs[row.ID+"|"+id]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), r.data...), true, nil
}
