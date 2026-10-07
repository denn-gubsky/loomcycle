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
func (s *Server) wallLimitCancel(agentID string) func() {
	return func() { s.cancelReg.Cancel(agentID, WallLimitReason) }
}
