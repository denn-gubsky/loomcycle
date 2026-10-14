package awaited

import (
	"context"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RunLister is the slice of store.Store RunningInStates reads.
type RunLister interface {
	EventReader
	ListRunsByUser(ctx context.Context, tenantID, userID string, statuses []store.RunStatus, limit int, cursor string) ([]store.Run, string, error)
}

// ScanCap bounds how many of a user's running runs one awaited_state listing
// reads. The state is derived from each run's events, not stored, so the
// filter cannot go into the query: every running run is read and judged in
// turn, a few event reads apiece. The cap keeps one listing's cost bounded
// for a user with a great many runs in flight.
const ScanCap = 1000

// scanPage is how many runs one store read of the scan returns.
const scanPage = 100

// Held is a running run an awaited_state listing returns, with what it is
// blocked on (ForRun's answer, so the caller need not derive it again).
type Held struct {
	Run       store.Run
	State, On string
}

// RunningInStates returns userID's running runs whose awaited state is one of
// states, newest first: at most max of them, from at most scanCap running runs
// read (<= 0 → ScanCap). visible drops a row the caller may not see before its
// events are read; nil keeps every row. tenantID reads as in
// store.ListRunsByUser.
//
// truncated reports that the scan stopped with running runs it never judged —
// the scan cap or max was reached first — so a run in one of states may be
// missing from the answer. A scan that judged every running run is not
// truncated, however many it read.
//
// Every transport's listing filters through here so they cannot disagree on
// which runs match or where the scan stops.
func RunningInStates(ctx context.Context, st RunLister, tenantID, userID string, states []string, visible func(store.Run) bool, scanCap, max int) (held []Held, truncated bool, err error) {
	if scanCap <= 0 {
		scanCap = ScanCap
	}
	scanned := 0
	cursor := ""
	for {
		page, next, err := st.ListRunsByUser(ctx, tenantID, userID, []store.RunStatus{store.RunRunning}, min(scanPage, scanCap-scanned), cursor)
		if err != nil {
			return nil, false, err
		}
		scanned += len(page)
		for i, r := range page {
			if visible != nil && !visible(r) {
				continue
			}
			state, on := ForRun(ctx, st, r.ID)
			if state == "" || !slices.Contains(states, state) {
				continue
			}
			held = append(held, Held{Run: r, State: state, On: on})
			if max > 0 && len(held) >= max {
				return held, i < len(page)-1 || next != "", nil
			}
		}
		switch {
		case next == "":
			return held, false, nil
		case scanned >= scanCap:
			return held, true, nil
		}
		cursor = next
	}
}
