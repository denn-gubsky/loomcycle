package dispatch_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// recordingGit records what the dispatcher handed its git function.
type recordingGit struct {
	mu      sync.Mutex
	rows    []store.WorkspaceRow
	shapes  []egress.GitShape
	tickets [][]byte
}

func (r *recordingGit) fn(_ context.Context, row store.WorkspaceRow, ticket []byte, req egress.GitRequest) (*egress.Response, error) {
	if req.Body != nil {
		_, _ = io.ReadAll(req.Body)
	}
	r.mu.Lock()
	r.rows = append(r.rows, row)
	r.shapes = append(r.shapes, req.Shape)
	r.tickets = append(r.tickets, append([]byte(nil), ticket...))
	r.mu.Unlock()
	return &egress.Response{Status: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("adv"))}, nil
}

func (r *recordingGit) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

func gitRow(id, tenant, worker string, state store.WorkspaceState) store.WorkspaceRow {
	r := readyRow(id, tenant, worker)
	r.State, r.Kind = state, store.WorkspaceKindGit
	r.Git = &store.WorkspaceGit{URL: "https://forge.example/acme/app.git", Branch: "loom/a", Mirror: "0123456789abcdef0123456789abcdef"}
	return r
}

func gitStart(workerID, wsID string, shape workspacepb.GitShape) *workspacepb.EgressStart {
	return &workspacepb.EgressStart{WorkerId: workerID, WorkspaceId: wsID, GitShape: shape, Ticket: []byte("ticket-bytes")}
}

// attachV attaches the fake as id speaking protocol version v.
func (f *fakeWorker) attachV(id string, v uint32) {
	f.t.Helper()
	if _, err := f.hello(&workspacepb.Hello{WorkerId: id, ProtocolVersion: v, Engine: workspace.EngineHost, Capacity: 4}); err != nil {
		f.t.Fatalf("attach v%d: %v", v, err)
	}
}

// A worker of the protocol before git streams (v2) still attaches and is
// served route requests, but a git stream from it is refused with a class
// that names the cause, before the git function sees anything.
func TestGitEgress_AWorkerOnProtocolV2IsRefusedGitStreams(t *testing.T) {
	st := newMemStore(gitRow("ws_a", "acme", acmeID("w1"), store.WorkspaceReady))
	d, target := core(t, st, dispatch.Options{})
	git := &recordingGit{}
	d.SetGitEgress(git.fn)
	f := dialFake(t, target, tokAcme)
	f.attachV("w1", workspace.ProtocolVersion-1)
	f.heartbeat(time.Second)
	_, err := f.egress(gitStart("w1", "ws_a", workspacepb.GitShape_GIT_SHAPE_ADVERTISE_UPLOAD), nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "worker_protocol_too_old") {
		t.Fatalf("a v2 worker's git stream: %v, want FailedPrecondition worker_protocol_too_old", err)
	}
	if git.n() != 0 {
		t.Fatal("the git function saw a v2 worker's stream")
	}
}

// The core attaches the protocols it serves, the one before git streams
// included, and no other.
func TestAttach_ServesTheProtocolsFromTheOldestItKeeps(t *testing.T) {
	for v, ok := range map[uint32]bool{
		workspace.MinProtocolVersion - 1: false,
		workspace.MinProtocolVersion:     true,
		workspace.ProtocolVersion:        true,
		workspace.ProtocolVersion + 1:    false,
	} {
		st := newMemStore()
		_, target := core(t, st, dispatch.Options{})
		f := dialFake(t, target, tokAcme)
		_, err := f.hello(&workspacepb.Hello{WorkerId: "w1", ProtocolVersion: v, Engine: workspace.EngineHost, Capacity: 4})
		if got := err == nil; got != ok {
			t.Errorf("protocol %d: attached = %v (%v), want %v", v, got, err, ok)
		}
	}
}

// A git request names a shape and no URL: a start that also names a route,
// a method or a path is refused.
func TestGitEgress_AStartNamingARouteBesideAShapeIsRefused(t *testing.T) {
	st := newMemStore(gitRow("ws_a", "acme", acmeID("w1"), store.WorkspaceReady))
	d, target := core(t, st, dispatch.Options{})
	git := &recordingGit{}
	d.SetGitEgress(git.fn)
	f := dialFake(t, target, tokAcme)
	f.attach("w1")
	f.heartbeat(time.Second)
	for _, mod := range []func(*workspacepb.EgressStart){
		func(s *workspacepb.EgressStart) { s.Route = "pypi" },
		func(s *workspacepb.EgressStart) { s.Method = http.MethodPost },
		func(s *workspacepb.EgressStart) { s.Path = "../../other/repo.git/info/refs" },
	} {
		s := gitStart("w1", "ws_a", workspacepb.GitShape_GIT_SHAPE_ADVERTISE_UPLOAD)
		mod(s)
		if _, err := f.egress(s, nil); status.Code(err) != codes.InvalidArgument {
			t.Errorf("start %+v: %v, want InvalidArgument", s, err)
		}
	}
	if git.n() != 0 {
		t.Fatal("the git function saw a malformed start")
	}
}

// A git stream reaches the git function with the row read from the store,
// the shape and the ticket; one for another principal's worker, or for a
// workspace not on that worker, never does. An opening row reaches it (the
// open's proof of access runs while the row is opening; the ticket decides),
// where a route stream for it is refused as before.
func TestGitEgress_CarriesTheRowShapeAndTicketToTheGitFunction(t *testing.T) {
	st := newMemStore(gitRow("ws_a", "acme", acmeID("w1"), store.WorkspaceReady), gitRow("ws_o", "acme", acmeID("w1"), store.WorkspaceOpening),
		gitRow("ws_b", "acme", acmeID("w2"), store.WorkspaceReady))
	d, target := core(t, st, dispatch.Options{})
	git, rec := &recordingGit{}, &recordingEgress{}
	d.SetGitEgress(git.fn)
	d.SetEgress(rec.fn)
	f := dialFake(t, target, tokAcme)
	f.attach("w1")
	f.heartbeat(time.Second)
	a, err := f.egress(gitStart("w1", "ws_a", workspacepb.GitShape_GIT_SHAPE_RECEIVE_PACK), []byte("0000"))
	if err != nil || a.status != http.StatusOK || string(a.body) != "adv" {
		t.Fatalf("a git stream = %+v, %v", a, err)
	}
	if git.shapes[0] != egress.GitReceivePack || string(git.tickets[0]) != "ticket-bytes" || git.rows[0].ID != "ws_a" {
		t.Fatalf("the git function got %v %q %s", git.shapes[0], git.tickets[0], git.rows[0].ID)
	}
	if _, err := f.egress(gitStart("w1", "ws_o", workspacepb.GitShape_GIT_SHAPE_ADVERTISE_UPLOAD), nil); err != nil {
		t.Fatalf("an opening git row: %v", err)
	}
	if _, err := f.egress(egressStart("w1", "ws_o"), nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a route stream for an opening row: %v, want PermissionDenied", err)
	}
	if _, err := f.egress(gitStart("w1", "ws_b", workspacepb.GitShape_GIT_SHAPE_ADVERTISE_UPLOAD), nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a workspace on another worker: %v, want PermissionDenied", err)
	}
	other := dialFake(t, target, tokOther)
	if _, err := other.egress(gitStart("w1", "ws_a", workspacepb.GitShape_GIT_SHAPE_ADVERTISE_UPLOAD), nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another principal's stream: %v, want PermissionDenied", err)
	}
	if git.n() != 2 {
		t.Fatalf("the git function was called %d times, want 2", git.n())
	}
}

// With no git function set, every git stream is refused as an HTTP answer.
func TestGitEgress_WithNoGitConfiguredEveryRequestIsRefused(t *testing.T) {
	st := newMemStore(gitRow("ws_a", "acme", acmeID("w1"), store.WorkspaceReady))
	_, target := core(t, st, dispatch.Options{})
	f := dialFake(t, target, tokAcme)
	f.attach("w1")
	f.heartbeat(time.Second)
	a, err := f.egress(gitStart("w1", "ws_a", workspacepb.GitShape_GIT_SHAPE_ADVERTISE_UPLOAD), nil)
	if err != nil || a.status != http.StatusForbidden {
		t.Fatalf("no git configured: %+v, %v; want a 403", a, err)
	}
}
