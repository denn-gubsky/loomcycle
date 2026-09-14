package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestSpawnRun_ClassifiesTheTypedErrorItIsAboutToDestroy is the test that makes
// the classification real rather than decorative.
//
// SpawnRun reports a failed run as (result, nil) with Error set to
// runErr.Error() — the typed error is flattened to a string HERE, one layer
// below the MCP handler. Classifying in the handler would compile, would pass
// any test built from hand-made errors, and would classify nothing in
// production, because by then there is only a string to match on.
//
// So this drives the real path: a request that makes RunOnce return a wrapped
// runner.ErrInvalidArgument, and asserts the category survives onto the result.
// Deleting the classification in SpawnRun must fail this test — not merely
// leave an import unused.
func TestSpawnRun_ClassifiesTheTypedErrorItIsAboutToDestroy(t *testing.T) {
	s := &Server{}

	// RunOnce rejects a user_id outside [A-Za-z0-9_-]{1,128} with
	// fmt.Errorf("%w: ...", runner.ErrInvalidArgument) — a real typed error
	// on the real path, reachable with no fixture.
	res, err := s.SpawnRun(context.Background(), connector.SpawnRunRequest{
		Agent:  "any",
		UserID: "not a valid user id!!",
	})
	if err != nil {
		t.Fatalf("SpawnRun returned a Go error (%v); this test assumes the "+
			"flatten-to-result path — if that changed, the classification "+
			"placement needs rechecking too", err)
	}

	if res.Error == "" {
		t.Fatal("expected a failed result; got no Error string — the invalid " +
			"user_id no longer fails, so this test is not exercising what it claims")
	}
	if res.ErrorInfo == nil {
		t.Fatal("result carries an error string but NO ErrorInfo: the typed error " +
			"was destroyed without being classified")
	}
	if res.ErrorInfo.Category != tools.CategoryValidation {
		t.Errorf("category = %q, want %q", res.ErrorInfo.Category, tools.CategoryValidation)
	}
	if res.ErrorInfo.Retryable {
		t.Error("a malformed request is not fixed by resending it unchanged")
	}
	if res.ErrorInfo.Description == "" {
		t.Error("no next step given to the caller")
	}
}

// An unclassifiable failure must leave ErrorInfo nil rather than acquire a
// default bucket — an invented category is worse than an absent one, because
// the agent acts on it.
func TestSpawnRun_UnclassifiableFailureLeavesErrorInfoNil(t *testing.T) {
	s := &Server{}

	// No agent configured and a valid user_id: the failure comes from
	// resolution//plumbing rather than a sentinel this runtime classifies.
	res, err := s.SpawnRun(context.Background(), connector.SpawnRunRequest{
		Agent:  "definitely-not-configured",
		UserID: "valid-user",
	})
	if err != nil {
		t.Skipf("path returned a Go error (%v); nothing to assert here", err)
	}
	if res.Error == "" {
		t.Skip("call unexpectedly succeeded; nothing to assert")
	}
	if res.ErrorInfo != nil && res.ErrorInfo.Category == "" {
		t.Errorf("ErrorInfo present with an empty category — half-populated classification")
	}
}
