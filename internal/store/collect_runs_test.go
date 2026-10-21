package store

import (
	"context"
	"fmt"
	"strconv"
	"testing"
)

// pagedRuns serves runs r0..r(n-1) newest first, a page at a time, with the
// next index as its cursor, and counts the rows it hands out.
type pagedRuns struct {
	n, served int
}

func (p *pagedRuns) ListRunsByUser(_ context.Context, _, _ string, _ []RunStatus, limit int, cursor string) ([]Run, string, error) {
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	end := min(start+limit, p.n)
	var out []Run
	for i := start; i < end; i++ {
		out = append(out, Run{ID: fmt.Sprintf("r%d", i)})
	}
	p.served += len(out)
	next := ""
	if end < p.n {
		next = strconv.Itoa(end)
	}
	return out, next, nil
}

func TestCollectRunsByUser_PagesPastDroppedRowsToFillMax(t *testing.T) {
	st := &pagedRuns{n: 350}
	// Keep only every 50th row: a first page of 100 holds two of them.
	keep := func(r Run) bool { var i int; fmt.Sscanf(r.ID, "r%d", &i); return i%50 == 0 }
	got, truncated, err := CollectRunsByUser(context.Background(), st, "", "u", nil, keep, 0, 5)
	if err != nil || len(got) != 5 || got[4].ID != "r200" || !truncated {
		t.Errorf("= %d runs (last %v), truncated %v, err %v; want r0..r200 and truncated", len(got), got, truncated, err)
	}
	got, truncated, _ = CollectRunsByUser(context.Background(), &pagedRuns{n: 350}, "", "u", nil, keep, 0, 100)
	if len(got) != 7 || truncated {
		t.Errorf("reading everything = %d runs, truncated %v; want all 7 and not truncated", len(got), truncated)
	}
}

func TestCollectRunsByUser_StopsAtTheScanCap(t *testing.T) {
	st := &pagedRuns{n: 5000}
	got, truncated, err := CollectRunsByUser(context.Background(), st, "", "u", nil, func(Run) bool { return false }, 250, 10)
	if err != nil || len(got) != 0 || !truncated || st.served != 250 {
		t.Errorf("= %d runs, truncated %v, read %d rows, err %v; want none, truncated, 250 read", len(got), truncated, st.served, err)
	}
}
