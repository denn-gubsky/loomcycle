package pause

// pauseBackplaneEvent is the wire payload published on
// `loomcycle.pause` when a replica transitions to / from paused. The
// `op` field discriminates pause vs resume so a single backplane
// topic carries both transitions.
type pauseBackplaneEvent struct {
	Op string `json:"op"` // "pause" | "resume"
}

// parseRuntimeState converts the wire string form (stored in the
// runtime_state.state column) back to a RuntimeState. Defaults to
// StateRunning on unknown input — the safest fallback for an
// out-of-band schema drift.
func parseRuntimeState(s string) RuntimeState {
	switch s {
	case "running":
		return StateRunning
	case "pausing":
		return StatePausing
	case "paused":
		return StatePaused
	default:
		return StateRunning
	}
}
