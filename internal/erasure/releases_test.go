package erasure_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
)

// The ontology release plane: its copies are redacted before the bindings that find
// them are cleared, and a failure on the way keeps the bindings (fail closed), so a
// re-run can still find the copies.

func hasError(errs []string, plane string) bool {
	for _, e := range errs {
		if strings.HasPrefix(e, plane+":") {
			return true
		}
	}
	return false
}

// TestService_BindingsStayWhenTheReleaseCopiesCannotBeRedacted: when the release
// redaction fails, or the bound subjects cannot be read for it, a live erasure clears
// no binding and reports the release plane as failed; when it succeeds, the bindings go
// after it.
func TestService_BindingsStayWhenTheReleaseCopiesCannotBeRedacted(t *testing.T) {
	ctx := context.Background()
	live := erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"}
	cases := []struct {
		name                       string
		boundErr, releaseErr       error
		wantCleared, wantRedaction bool
	}{
		{name: "redaction fails", releaseErr: errors.New("store fault"), wantRedaction: true},
		{name: "bound subjects unreadable", boundErr: errors.New("store fault")},
		{name: "redaction succeeds", wantCleared: true, wantRedaction: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSvc(t)
			f := &fakeSubjects{bound: boundPair()}
			wireSubjects(s, f)
			if tc.boundErr != nil {
				s.BoundSubjects = func(context.Context, string, string) ([]erasure.BoundSubject, error) {
					return nil, tc.boundErr
				}
			}
			var redacted bool
			var clearedFirst bool
			s.ReleaseCopies = func(_ context.Context, _ string, subjects []erasure.BoundSubject, dryRun bool) (erasure.ReleaseRedaction, error) {
				redacted, clearedFirst = !dryRun && len(subjects) == 2, f.cleared
				return erasure.ReleaseRedaction{Entries: 4, Releases: 2}, tc.releaseErr
			}
			res, err := s.Execute(ctx, live)
			if err != nil {
				t.Fatal(err)
			}
			if f.cleared != tc.wantCleared {
				t.Errorf("bindings cleared = %v, want %v", f.cleared, tc.wantCleared)
			}
			if redacted != tc.wantRedaction || clearedFirst {
				t.Errorf("redaction ran = %v (after clearing: %v), want %v before any clearing", redacted, clearedFirst, tc.wantRedaction)
			}
			if failed := hasError(res.Errors, "ontology_releases"); failed == tc.wantCleared {
				t.Errorf("errors = %v, want the release plane failed only when the bindings stay", res.Errors)
			}
			if tc.wantCleared && (res.Deleted["ontology_release_entries"] != 4 || res.Retained["ontology_releases"] == "") {
				t.Errorf("deleted %v retained %q", res.Deleted, res.Retained["ontology_releases"])
			}
		})
	}
}
