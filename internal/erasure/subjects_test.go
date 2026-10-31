package erasure_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakeSubjects stands in for the document store: two subjects bound to alice, one of
// them hidden from the caller's reading.
type fakeSubjects struct {
	bound     []erasure.BoundSubject
	cleared   bool
	announced []erasure.BoundSubject
	// chatsAtAnnounce is how many of the subject's chats were left when the
	// announcement went out.
	chatsAtAnnounce int
}

func wireSubjects(s *erasure.Service, f *fakeSubjects) {
	s.BoundSubjects = func(context.Context, string, string) ([]erasure.BoundSubject, error) {
		if f.cleared {
			return nil, nil
		}
		return f.bound, nil
	}
	s.ClearBoundSubjects = func(context.Context, string, string) ([]erasure.BoundSubject, error) {
		f.cleared = true
		return f.bound, nil
	}
	s.AnnounceErased = func(ctx context.Context, tenant string, subjects []erasure.BoundSubject) string {
		f.announced = subjects
		rows, _, _ := s.Store.ListSessions(ctx, store.SessionFilter{TenantID: tenant, UserID: "alice"}, 10, 0)
		f.chatsAtAnnounce = len(rows)
		return ""
	}
}

func boundPair() []erasure.BoundSubject {
	return []erasure.BoundSubject{
		{DocumentID: "d1", NaturalKey: "person:alice", Path: "/facts/alice", RootChunkID: "r1", Readable: true},
		{DocumentID: "d2", NaturalKey: "person:a-smith", Path: "/facts/a-smith", RootChunkID: "r2", Readable: false},
	}
}

// TestService_BoundSubjectsAreNamedOnlyWhenReadable: the report and a dry run name
// the bound subjects the caller may read and count the rest; neither clears or
// announces anything.
func TestService_BoundSubjectsAreNamedOnlyWhenReadable(t *testing.T) {
	s := newSvc(t)
	f := &fakeSubjects{bound: boundPair()}
	wireSubjects(s, f)
	ctx := context.Background()

	rep, err := s.Report(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.BoundSubjects) != 1 || rep.BoundSubjects[0].DocumentID != "d1" || rep.BoundSubjectsWithheld != 1 ||
		rep.Tier2.Counts["subject_bindings"] != 2 {
		t.Errorf("report = bound %+v withheld %d tier2 %v", rep.BoundSubjects, rep.BoundSubjectsWithheld, rep.Tier2.Counts)
	}
	res, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.BoundSubjects) != 1 || res.BoundSubjectsWithheld != 1 || res.Deleted["subject_bindings"] != 2 {
		t.Errorf("dry run = bound %+v withheld %d deleted %v", res.BoundSubjects, res.BoundSubjectsWithheld, res.Deleted)
	}
	if f.cleared || f.announced != nil {
		t.Error("a dry run cleared or announced")
	}
	// The hidden subject's names travel nowhere, prose included.
	whole, _ := json.Marshal(res)
	for _, name := range []string{"d2", "person:a-smith", "r1", "r2"} {
		if strings.Contains(string(whole), `"`+name+`"`) || strings.Contains(string(whole), name+" ") {
			t.Errorf("the result carries %q: %s", name, whole)
		}
	}
}

// TestService_ErasureAnnouncesUnboundSubjectsLast: the event follows the erasure (the
// chats are already gone when it is published), covers every unbound subject including
// the unnamed one, and Retained counts without naming.
func TestService_ErasureAnnouncesUnboundSubjectsLast(t *testing.T) {
	s := newSvc(t)
	seed(t, s, "acme", "alice")
	f := &fakeSubjects{bound: boundPair()}
	wireSubjects(s, f)

	res, err := s.Execute(context.Background(), erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.cleared || len(f.announced) != 2 {
		t.Fatalf("cleared %v, announced %+v; want both subjects unbound and announced", f.cleared, f.announced)
	}
	if f.chatsAtAnnounce != 0 {
		t.Errorf("announced with %d chat(s) still in place; the event must follow the erasure", f.chatsAtAnnounce)
	}
	if res.Deleted["subject_bindings"] != 2 {
		t.Errorf("deleted = %v", res.Deleted)
	}
	r := res.Retained["bound_subjects"]
	if !strings.HasPrefix(r, "2 subject(s)") || strings.Contains(r, "person:") || strings.Contains(r, "/facts/") {
		t.Errorf("retained[bound_subjects] = %q, want a count and no names", r)
	}
}

// TestService_UnwiredBoundSubjectsAreNotExamined: without the plane the result neither
// claims to have looked nor lists anything.
func TestService_UnwiredBoundSubjectsAreNotExamined(t *testing.T) {
	s := newSvc(t)
	res, err := s.Execute(context.Background(), erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Deleted["subject_bindings"]; ok {
		t.Errorf("deleted claims an unexamined plane: %v", res.Deleted)
	}
	if res.BoundSubjects == nil {
		t.Error("bound_subjects is null on the wire; want []")
	}
}
