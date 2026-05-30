package webhook

import "time"

// recentBufferCap is the per-webhook ring depth: the last N invocations of a
// given webhook name are retained for the triage endpoint. Bounded so a
// high-traffic webhook can't grow the buffer without limit — triage is a
// recent-activity window, not an audit log (the runs table is the durable
// record). 50 matches the recent-deliveries endpoint's hard cap.
const recentBufferCap = 50

// deliveryRecord is one entry in a webhook's recent-deliveries ring. It holds
// ONLY non-sensitive triage fields: the delivery id (a content fingerprint or
// operator-supplied header, never a secret), the verdict label, the receive
// time, and the spawned run id (empty for channel deliveries / rejections).
// Credentials and raw payloads are deliberately absent.
type deliveryRecord struct {
	DeliveryID string    `json:"delivery_id"`
	Verdict    string    `json:"verdict"`
	ReceivedAt time.Time `json:"received_at"`
	RunID      string    `json:"run_id,omitempty"`
}

// recentRing is a fixed-capacity, newest-overwrites-oldest ring of
// deliveryRecords for one webhook name. Not safe for concurrent use on its
// own — the Receiver's recentMu guards all access.
type recentRing struct {
	buf  []deliveryRecord
	next int  // index to write the next record
	full bool // whether the ring has wrapped at least once
}

// add appends a record, overwriting the oldest once at capacity.
func (r *recentRing) add(rec deliveryRecord) {
	if r.buf == nil {
		r.buf = make([]deliveryRecord, recentBufferCap)
	}
	r.buf[r.next] = rec
	r.next = (r.next + 1) % recentBufferCap
	if r.next == 0 {
		r.full = true
	}
}

// snapshot returns the records newest-first, capped at limit (after the
// ring's own length). A limit <= 0 is treated as the full available depth.
func (r *recentRing) snapshot(limit int) []deliveryRecord {
	n := r.next
	if r.full {
		n = recentBufferCap
	}
	if n == 0 {
		return nil
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]deliveryRecord, 0, limit)
	// Walk backwards from the most-recently-written slot.
	idx := (r.next - 1 + recentBufferCap) % recentBufferCap
	for i := 0; i < limit; i++ {
		out = append(out, r.buf[idx])
		idx = (idx - 1 + recentBufferCap) % recentBufferCap
	}
	return out
}

// recordDelivery appends a triage record for the given webhook name. Called
// at every terminal point in handle()/deliver* with the verdict already
// computed for the span. did + runID may be empty (a rejection before the
// delivery id is derived, or a non-spawn / rejected delivery). Best-effort:
// it holds recentMu only briefly and never affects the response.
func (rec *Receiver) recordDelivery(name, did, verdict, runID string) {
	r := deliveryRecord{
		DeliveryID: did,
		Verdict:    verdict,
		ReceivedAt: rec.now(),
		RunID:      runID,
	}
	rec.recentMu.Lock()
	defer rec.recentMu.Unlock()
	if rec.recent == nil {
		rec.recent = make(map[string]*recentRing)
	}
	ring := rec.recent[name]
	if ring == nil {
		ring = &recentRing{}
		rec.recent[name] = ring
	}
	ring.add(r)
}

// recentSnapshot returns the recorded deliveries for a webhook name,
// newest-first, capped at limit. ok=false when the name has never been seen
// (so the endpoint can 404 rather than return an empty list for a typo).
func (rec *Receiver) recentSnapshot(name string, limit int) ([]deliveryRecord, bool) {
	rec.recentMu.Lock()
	defer rec.recentMu.Unlock()
	ring := rec.recent[name]
	if ring == nil {
		return nil, false
	}
	return ring.snapshot(limit), true
}
