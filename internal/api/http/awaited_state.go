package http

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fillAwaitedStateForRunning enriches a slice of agentResponse with
// the awaited_state / awaited_on fields (see awaited.ForRun) for each
// running entry. Non-running rows are skipped.
func fillAwaitedStateForRunning(ctx context.Context, st store.Store, items []agentResponse) {
	for i, item := range items {
		if item.Status != store.RunRunning {
			continue
		}
		if state, on := awaited.ForRun(ctx, st, item.RunID); state != "" {
			items[i].AwaitedState = state
			items[i].AwaitedOn = on
		}
	}
}

// parseUserAgentsFilter reads GET /v1/users/{user_id}/agents's filters. A
// non-empty code is a 400 with msg.
//
//   - status: one or more run statuses, comma-separated and/or repeated; a run
//     in any of them is listed. Absent or empty → running, the "what's in
//     flight for me?" view; "all" alone → no status filter. An unknown status
//     is refused rather than read as a filter nothing matches.
//   - awaited_state: one or more of awaited.States, the same way. It is derived
//     from a running run's events, so it implies status=running, and any other
//     status with it is refused.
func parseUserAgentsFilter(q url.Values) (statuses []store.RunStatus, awaitedStates []string, code, msg string) {
	raw := splitQueryList(q["status"])
	switch {
	case len(raw) == 0:
		statuses = []store.RunStatus{store.RunRunning}
	case len(raw) == 1 && raw[0] == "all":
		statuses = nil
	default:
		for _, v := range raw {
			st := store.RunStatus(v)
			if !store.IsRunStatus(st) {
				return nil, nil, "invalid_status", fmt.Sprintf("status %q is not one of %s, or all (alone)", v, joinRunStatuses())
			}
			statuses = append(statuses, st)
		}
	}
	for _, v := range splitQueryList(q["awaited_state"]) {
		if !slices.Contains(awaited.States, v) {
			return nil, nil, "invalid_awaited_state", fmt.Sprintf("awaited_state %q is not one of %s", v, strings.Join(awaited.States, ", "))
		}
		awaitedStates = append(awaitedStates, v)
	}
	if len(awaitedStates) > 0 {
		// statuses is nil for status=all, which takes in every other status.
		onlyRunning := len(statuses) > 0 && !slices.ContainsFunc(statuses, func(st store.RunStatus) bool { return st != store.RunRunning })
		if !onlyRunning {
			return nil, nil, "invalid_status", "awaited_state lists running runs only; omit status or pass status=running"
		}
	}
	return statuses, awaitedStates, "", ""
}

// splitQueryList flattens a repeatable, comma-separated query parameter into
// its trimmed, non-empty values.
func splitQueryList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// joinRunStatuses is store.RunStatuses for an error message.
func joinRunStatuses() string {
	names := make([]string, len(store.RunStatuses))
	for i, st := range store.RunStatuses {
		names[i] = string(st)
	}
	return strings.Join(names, ", ")
}
