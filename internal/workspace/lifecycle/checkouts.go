package lifecycle

import (
	"context"
	"sync"
	"time"
)

// The runtime's record of each File op=checkout of Documents into a
// workspace (RFC GW P7). A checkout also writes a manifest into the
// workspace, but a command there can rewrite that file; a commit trusts
// only this record, which no command reaches.
//
// It is kept in this process, as the conversion quarantine is, and so it
// fails closed: a commit asked on a replica that did not take the checkout,
// or after the runtime restarted, finds no record and is refused, and the
// agent checks the documents out again. A workspace is pinned to one run,
// which runs on one replica, so in practice the record is where the commit
// is asked. Durable workspaces (RFC GW P12) will need it kept in the store.
//
// Records are bounded per workspace, never across them: one run taking many
// checkouts drops only its own oldest records, never another workspace's
// (of another run or tenant). A workspace's records go when this replica
// releases it, and any left behind (a workspace released elsewhere) when
// they pass checkoutRecordTTL.

const (
	// maxCheckoutsPerWorkspace bounds the records one workspace keeps; past
	// it its oldest goes. A record is one manifest: at most
	// workspace.checkout.max_documents entries.
	maxCheckoutsPerWorkspace = 4
	// checkoutRecordTTL drops the records of a workspace no checkout has
	// touched for this long, which only a workspace released on another
	// replica can leave behind.
	checkoutRecordTTL = 7 * 24 * time.Hour
)

type checkoutRecord struct {
	data []byte
	at   time.Time
}

type checkouts struct {
	mu sync.Mutex
	// byWorkspace maps a workspace row id to its records by checkout id.
	byWorkspace map[string]map[string]checkoutRecord
}

// RememberCheckout keeps rec as the record of checkout id in the run's
// workspace name, replacing one kept under that id. rec is the caller's,
// opaque here; it is copied.
func (m *Manager) RememberCheckout(ctx context.Context, o Owner, name, id string, rec []byte) error {
	row, err := m.Get(ctx, o, name)
	if err != nil {
		return err
	}
	m.checkouts.remember(row.ID, id, rec, m.now())
	return nil
}

// Checkout returns the record of checkout id in the run's workspace name;
// ok is false when this runtime keeps none, and the caller refuses.
func (m *Manager) Checkout(ctx context.Context, o Owner, name, id string) (rec []byte, ok bool, err error) {
	row, err := m.Get(ctx, o, name)
	if err != nil {
		return nil, false, err
	}
	rec, ok = m.checkouts.lookup(row.ID, id)
	return rec, ok, nil
}

func (c *checkouts) remember(workspaceID, id string, rec []byte, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byWorkspace == nil {
		c.byWorkspace = map[string]map[string]checkoutRecord{}
	}
	for ws, recs := range c.byWorkspace {
		if ws != workspaceID && newest(recs).Before(now.Add(-checkoutRecordTTL)) {
			delete(c.byWorkspace, ws)
		}
	}
	recs := c.byWorkspace[workspaceID]
	if recs == nil {
		recs = map[string]checkoutRecord{}
		c.byWorkspace[workspaceID] = recs
	}
	if _, ok := recs[id]; !ok && len(recs) >= maxCheckoutsPerWorkspace {
		oldK, oldT := "", time.Time{}
		for k, r := range recs {
			if oldK == "" || r.at.Before(oldT) {
				oldK, oldT = k, r.at
			}
		}
		delete(recs, oldK)
	}
	recs[id] = checkoutRecord{data: append([]byte(nil), rec...), at: now}
}

func newest(recs map[string]checkoutRecord) time.Time {
	var t time.Time
	for _, r := range recs {
		if r.at.After(t) {
			t = r.at
		}
	}
	return t
}

func (c *checkouts) lookup(workspaceID, id string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.byWorkspace[workspaceID][id]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), r.data...), true
}

// forget drops a workspace's records.
func (c *checkouts) forget(workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byWorkspace, workspaceID)
}
