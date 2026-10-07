package http

// WallLimitReason is the stop reason of a run its own max_wall_seconds ended.
// A fixed word, so a caller can match it.
const WallLimitReason = "wall_limit"

// wallLimitCancel is what the loop calls when a run's max_wall_seconds runs
// out. It goes through the cancel registry rather than the run's ctx for two
// reasons: the registry is what records a cancel's reason on the run row, so
// the run ends cancelled with stop_reason "wall_limit" instead of failed; and
// its cascade reaches what a ctx cancel does not — resident children and
// detached walks, which run on contexts of their own.
//
// It cancels THIS run and no other. The registry is keyed by agent id, which a
// later run can hold — a continuation, or a caller reusing the id — so a
// cancel by agent id, landing as this run ends, could end that one instead.
// And it stays on this replica: the watcher lives in the process that runs
// the run, so there is nothing to route.
func (s *Server) wallLimitCancel(agentID, runID string) func() {
	return func() { s.cancelReg.CancelLocalRun(agentID, runID, WallLimitReason) }
}
