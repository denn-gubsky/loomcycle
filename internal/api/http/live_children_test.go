package http

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Resident children are among a run's live children: with the limit reached
// by open children, another open and a spawn through the Agent tool are both
// refused; closing one frees its slot once its teardown runs.
func TestLiveChildren_ResidentChildrenCountTowardTheRunsLimit(t *testing.T) {
	srv := newResidentTestServer(t)
	srv.cfg().Env.MaxLiveChildrenPerRun = 2
	ctx := tools.WithRunID(residentParentCtx("parent-agent", ""), "r_parent")

	var open []string
	for i := 0; i < 2; i++ {
		runID, _, _, err := srv.openResidentChild(ctx, "child", "hi", "", 0, 0)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		open = append(open, runID)
	}
	// Close and wait for teardown, so no child writes after the store closes.
	defer func() {
		for _, id := range open {
			_ = srv.closeResidentChild(ctx, id)
		}
		for _, id := range open {
			waitResidentGone(t, srv, id)
		}
	}()

	_, _, _, err := srv.openResidentChild(ctx, "child", "third", "", 0, 0)
	var lim *tools.LiveChildLimitError
	if !errors.As(err, &lim) || lim.Alive != 2 || lim.Limit != 2 {
		t.Fatalf("third open: err = %v, want the live-children refusal (2 alive, limit 2)", err)
	}
	res, err := agentToolOf(t, srv).Execute(ctx, json.RawMessage(`{"name":"child","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "2 children alive") {
		t.Errorf("spawn beside two open residents = %+v, want the live-children refusal", res)
	}

	if err := srv.closeResidentChild(ctx, open[0]); err != nil {
		t.Fatal(err)
	}
	waitResidentGone(t, srv, open[0])
	open = open[1:]
	if srv.liveChildren.Alive("r_parent") != 1 {
		t.Fatalf("after closing one, %d children counted, want 1", srv.liveChildren.Alive("r_parent"))
	}
	runID, _, _, err := srv.openResidentChild(ctx, "child", "again", "", 0, 0)
	if err != nil {
		t.Fatalf("open after a close: %v", err)
	}
	open = append(open, runID)
}

// The resident cap (8 by default) still applies under the default live
// limit (32): the ninth open is refused by the resident cap.
func TestLiveChildren_ResidentCapStillAppliesUnderTheLiveLimit(t *testing.T) {
	srv := newResidentTestServer(t)
	ctx := tools.WithRunID(residentParentCtx("parent-agent", ""), "r_parent")
	var open []string
	// Close AND wait for each child's teardown: closing only cancels it, and a
	// child still finishing its run writes to the store after the test's
	// store is closed and its temp dir removed.
	defer func() {
		for _, id := range open {
			_ = srv.closeResidentChild(context.Background(), id)
		}
		for _, id := range open {
			waitResidentGone(t, srv, id)
		}
	}()
	for i := 0; i < 8; i++ {
		runID, _, _, err := srv.openResidentChild(ctx, "child", "hi", "", 0, 0)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		open = append(open, runID)
	}
	_, _, _, err := srv.openResidentChild(ctx, "child", "ninth", "", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "cap reached (8 open") {
		t.Fatalf("ninth open: err = %v, want the resident cap of 8", err)
	}
	if n := srv.liveChildren.Alive("r_parent"); n != 8 {
		t.Errorf("%d live children counted, want the 8 open residents", n)
	}
}
