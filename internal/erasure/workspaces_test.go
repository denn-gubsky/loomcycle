package erasure_test

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC GW P12 ruling Q2: a durable workspace goes only on discard or its
// user's erasure. An erasure reports the subject's durable workspaces in its
// tenant, deletes nothing on a dry run, and asks the workspace layer to
// remove them (the worker removes the directory); without that layer they
// are named as retained.
func seedDurable(t *testing.T, s *erasure.Service) {
	t.Helper()
	for _, r := range []store.WorkspaceRow{
		{ID: "ws_a1", TenantID: "acme", UserID: "alice", Name: "one", WorkerID: "tenant:acme/w1", OwnerRunID: "run-1", Lifetime: store.WorkspaceLifetimeDurable},
		{ID: "ws_a2", TenantID: "acme", UserID: "alice", Name: "two", WorkerID: "tenant:acme/w1", OwnerRunID: "run-2", Lifetime: store.WorkspaceLifetimeDurable},
		{ID: "ws_ae", TenantID: "acme", UserID: "alice", Name: "eph", WorkerID: "tenant:acme/w1", OwnerRunID: "run-3"},
		{ID: "ws_b1", TenantID: "acme", UserID: "bob", Name: "one", WorkerID: "tenant:acme/w1", OwnerRunID: "run-4", Lifetime: store.WorkspaceLifetimeDurable},
		{ID: "ws_g1", TenantID: "globex", UserID: "alice", Name: "one", WorkerID: "tenant:globex/w1", OwnerRunID: "run-5", Lifetime: store.WorkspaceLifetimeDurable},
	} {
		if _, err := s.Store.WorkspaceCreate(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestService_ErasesTheSubjectsDurableWorkspaces(t *testing.T) {
	s := newSvc(t)
	seedDurable(t, s)
	var asked []string
	s.EraseDurableWorkspaces = func(_ context.Context, tenant, subject string) (int, error) {
		asked = append(asked, tenant+"/"+subject)
		return 2, nil
	}
	ctx := context.Background()
	rep, err := s.Report(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Tier1.Counts["workspaces_durable"]; got != 2 {
		t.Errorf("report workspaces_durable = %d, want 2", got)
	}
	dry, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Deleted["workspaces_durable"] != 2 || len(asked) != 0 {
		t.Errorf("dry run deleted %d, asked %v; want 2 and nothing asked", dry.Deleted["workspaces_durable"], asked)
	}
	res, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted["workspaces_durable"] != 2 || strings.Join(asked, ",") != "acme/alice" {
		t.Errorf("deleted %d, asked %v; want 2, acme/alice", res.Deleted["workspaces_durable"], asked)
	}
}

func TestService_DurableWorkspacesAreRetainedWithoutTheWorkspaceLayer(t *testing.T) {
	s := newSvc(t)
	seedDurable(t, s)
	res, err := s.Execute(context.Background(), erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted["workspaces_durable"] != 0 || !strings.Contains(res.Retained["workspaces_durable"], "2 durable workspace") {
		t.Errorf("deleted %d, retained %q", res.Deleted["workspaces_durable"], res.Retained["workspaces_durable"])
	}
}
