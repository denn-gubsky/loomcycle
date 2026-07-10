package teamgraph

// Pure graph-walk helpers shared by the runtime orchestrator (internal/teamrun).
// They read the Definition; they never execute a handler — that's the runner's
// job. Kept here (stdlib-only) so the orchestrator depends only on the model.

// StateByID returns the state with the given id.
func StateByID(d Definition, id string) (State, bool) {
	for _, s := range d.States {
		if s.ID == id {
			return s, true
		}
	}
	return State{}, false
}

// NextState resolves the outbound transition from `from` whose `on` label equals
// `on` (e.g. "success", "pushback:code-fix"), returning the destination state
// id. Validate guarantees a state's outbound labels are unique, so at most one
// transition matches.
func NextState(d Definition, from, on string) (string, bool) {
	for _, t := range d.Transitions {
		if t.From == from && t.On == on {
			return t.To, true
		}
	}
	return "", false
}

// EffectiveMaxIterations is the per-state cycle cap: the definition's value, or
// DefaultMaxIterations when unset (0). Validate rejects negatives.
func EffectiveMaxIterations(d Definition) int {
	if d.MaxIterations > 0 {
		return d.MaxIterations
	}
	return DefaultMaxIterations
}
