package awaited

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeLister is a user's running runs, newest first, each with its events. It
// records every page size asked for so a test can see how much was read.
type fakeLister struct {
	runs   []store.Run
	events map[string]fakeEvents
	reads  []int
}

func (f *fakeLister) ListRunsByUser(_ context.Context, _, _ string, statuses []store.RunStatus, limit int, cursor string) ([]store.Run, string, error) {
	if !slices.Equal(statuses, []store.RunStatus{store.RunRunning}) {
		return nil, "", fmt.Errorf("scan asked for %v, want only running", statuses)
	}
	if limit <= 0 {
		return nil, "", fmt.Errorf("scan asked for a page of %d", limit)
	}
	f.reads = append(f.reads, limit)
	from := 0
	if cursor != "" {
		from, _ = strconv.Atoi(cursor)
	}
	to := min(from+limit, len(f.runs))
	next := ""
	if to < len(f.runs) {
		next = strconv.Itoa(to)
	}
	return f.runs[from:to], next, nil
}

func (f *fakeLister) GetLastEventOfTypes(ctx context.Context, runID string, types []string) (store.Event, error) {
	return f.events[runID].GetLastEventOfTypes(ctx, runID, types)
}

func (f *fakeLister) GetLastEventOfTypesBefore(ctx context.Context, runID string, types []string, beforeSeq int64) (store.Event, error) {
	return f.events[runID].GetLastEventOfTypesBefore(ctx, runID, types, beforeSeq)
}

func (f *fakeLister) GetRunEventsSince(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	return f.events[runID].GetRunEventsSince(ctx, runID, afterSeq, limit)
}

// The events of a run held for review, of an interactive run parked for the
// operator's turn, and of a run blocked on nothing.
var (
	heldForReview = fakeEvents{ev("awaiting_review", `{"type":"awaiting_review","awaiting_review":{"round":1}}`)}
	parkedOnInput = fakeEvents{ev("awaiting_input", `{"type":"awaiting_input","awaiting_input":{"since_turn":1}}`)}
	justRunning   = fakeEvents{ev("text", `{"type":"text"}`)}
)

// lister builds n running runs r_0 (newest) … r_{n-1}; kind picks each one's
// events.
func lister(n int, kind func(i int) fakeEvents) *fakeLister {
	f := &fakeLister{events: map[string]fakeEvents{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("r_%d", i)
		f.runs = append(f.runs, store.Run{ID: id, Status: store.RunRunning, TenantID: "acme"})
		f.events[id] = kind(i)
	}
	return f
}

func heldIDs(h []Held) []string {
	out := make([]string, len(h))
	for i, x := range h {
		out[i] = x.Run.ID
	}
	return out
}

// Only runs in a requested state come back, newest first, with the state they
// are in — across every page of the scan.
func TestRunningInStates_ReturnsOnlyRunsInARequestedState(t *testing.T) {
	f := lister(250, func(i int) fakeEvents {
		switch {
		case i%50 == 7:
			return heldForReview
		case i%50 == 8:
			return parkedOnInput
		}
		return justRunning
	})
	got, truncated, err := RunningInStates(context.Background(), f, "", "u", []string{Review}, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"r_7", "r_57", "r_107", "r_157", "r_207"}; !slices.Equal(heldIDs(got), want) {
		t.Errorf("held = %v, want %v", heldIDs(got), want)
	}
	for _, h := range got {
		if h.State != Review {
			t.Errorf("%s reported state %q, want review", h.Run.ID, h.State)
		}
	}
	if truncated {
		t.Error("a scan that read every running run reported truncated")
	}

	both, _, _ := RunningInStates(context.Background(), f, "", "u", []string{Review, Input}, nil, 0, 0)
	if len(both) != 10 {
		t.Errorf("review+input matched %d runs, want 10", len(both))
	}
}

// The scan reads at most scanCap running runs and says so when runs were left
// unread; reading exactly every run is not truncation.
func TestRunningInStates_StopsAtTheScanCapAndReportsTruncated(t *testing.T) {
	everyFifth := func(i int) fakeEvents {
		if i%5 == 0 {
			return heldForReview
		}
		return justRunning
	}
	f := lister(30, everyFifth)
	got, truncated, err := RunningInStates(context.Background(), f, "", "u", []string{Review}, nil, 12, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"r_0", "r_5", "r_10"}; !slices.Equal(heldIDs(got), want) {
		t.Errorf("held = %v, want %v (only the first 12 runs read)", heldIDs(got), want)
	}
	if !truncated {
		t.Error("a scan cut short by the cap did not report truncated")
	}
	if read := f.reads; len(read) != 1 || read[0] != 12 {
		t.Errorf("page sizes read = %v, want one page of 12", read)
	}

	exact := lister(12, everyFifth)
	if _, truncated, _ := RunningInStates(context.Background(), exact, "", "u", []string{Review}, nil, 12, 0); truncated {
		t.Error("a scan that read every run reported truncated")
	}

	// The production cap reads in pages of 100 and stops at 1,000.
	big := lister(1005, everyFifth)
	if _, truncated, _ := RunningInStates(context.Background(), big, "", "u", []string{Review}, nil, 0, 0); !truncated {
		t.Error("1,005 running runs under the default cap did not report truncated")
	}
	total := 0
	for _, n := range big.reads {
		total += n
	}
	if total != ScanCap || len(big.reads) != ScanCap/scanPage {
		t.Errorf("default cap read %d runs in %d pages, want %d in %d", total, len(big.reads), ScanCap, ScanCap/scanPage)
	}
}

// max bounds the answer; stopping on it with runs unread is truncation, and the
// last match being the last run is not.
func TestRunningInStates_StopsAtMaxMatches(t *testing.T) {
	all := lister(5, func(int) fakeEvents { return heldForReview })
	got, truncated, _ := RunningInStates(context.Background(), all, "", "u", []string{Review}, nil, 0, 3)
	if !slices.Equal(heldIDs(got), []string{"r_0", "r_1", "r_2"}) || !truncated {
		t.Errorf("held = %v truncated=%v, want the 3 newest and truncated", heldIDs(got), truncated)
	}
	if got, truncated, _ := RunningInStates(context.Background(), all, "", "u", []string{Review}, nil, 0, 5); len(got) != 5 || truncated {
		t.Errorf("held %d truncated=%v, want all 5 and not truncated", len(got), truncated)
	}
}

// A row the caller may not see is dropped before it is judged, and still
// counts toward the scan cap: the cap bounds work, not answers.
func TestRunningInStates_SkipsInvisibleRows(t *testing.T) {
	f := lister(4, func(int) fakeEvents { return heldForReview })
	f.runs[1].TenantID = "globex"
	got, _, _ := RunningInStates(context.Background(), f, "", "u", []string{Review}, func(r store.Run) bool { return r.TenantID == "acme" }, 0, 0)
	if want := []string{"r_0", "r_2", "r_3"}; !slices.Equal(heldIDs(got), want) {
		t.Errorf("held = %v, want %v", heldIDs(got), want)
	}
}
