package concurrency

// HoldsLease reports whether owner (a run id) has a slot of providerID's lane
// for kind held for it right now — in flight or reserved between its calls
// (affinity_hold_ms). Queue-aware routing asks it at a run's call boundary: a
// run whose lease was preempted or expired, or whose lane keeps none, has no
// warm slot to lose by moving, which is the only time it may move. False for a
// provider or kind with no lane.
func (s *CallScheduler) HoldsLease(providerID, kind, owner string) bool {
	if owner == "" {
		return false
	}
	lane, _ := s.lane(providerID, kind)
	if lane == nil {
		return false
	}
	return lane.holdsLease(owner)
}

// holdsLease reports whether owner has a lease on this lane.
func (l *Lane) holdsLease(owner string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leases[owner] != nil
}
