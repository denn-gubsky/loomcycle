// Package workspacetest runs a workspace dispatcher and a real workspace
// worker in the test's process, for the packages above them (the Workspace
// tool, the runtime's run-end release, the sweeper) to test against the real
// protocol.
package workspacetest

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// token is the one bearer the harness's resolver knows. Not a real token.
const token = "workspacetest-worker-token"

// Harness is a dispatcher with one tenant-bound worker attached.
type Harness struct {
	D *dispatch.Dispatcher
	// Root is the worker's root: a workspace's directory is Root/<id>.
	Root string
	// WorkerID is the core's id for the worker, what a row's worker_id holds.
	WorkerID string
	t        *testing.T
	target   string
	opt      worker.Options
	tenant   string
	stop     func()
	// personal are the personal workers AddPersonalWorker attached.
	personal map[string]bool
}

// Start serves a dispatcher over st on a loopback port and attaches one
// host-engine worker serving tenant, and waits until it is attached. opt
// may set the worker's capacity, toolchains, idle streams and commands.
func Start(t *testing.T, st dispatch.Store, tenant string, opt worker.Options) *Harness {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Personal workers are on: only AddPersonalWorker's token is one.
	d := dispatch.New(dispatch.Options{PersonalWorkers: func() bool { return true }})
	d.Activate(st, func(_ context.Context, bearer string) (auth.Principal, bool) {
		if user, ok := strings.CutPrefix(bearer, personalToken); ok && user != "" {
			return auth.Principal{TenantID: tenant, Subject: user, Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-personal-" + user,
				Restrictions: auth.Restrictions{WorkerPersonal: true}}, true
		}
		if bearer != token {
			return auth.Principal{}, false
		}
		return auth.Principal{TenantID: tenant, Subject: "worker", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-worker"}, true
	})
	safego.Go(context.Background(), "workspacetest.serve", func() { _ = d.Serve(l) })
	t.Cleanup(d.Stop)
	if opt.Root == "" {
		opt.Root = t.TempDir()
	}
	if opt.Toolchains == nil {
		opt.Toolchains = []string{"python", "node"}
	}
	if opt.IdleStreams == 0 {
		opt.IdleStreams = 4
	}
	opt.Target, opt.Token, opt.ID, opt.Insecure = l.Addr().String(), token, "w1", true
	h := &Harness{D: d, Root: opt.Root, WorkerID: "tenant:" + tenant + "/w1", t: t, target: opt.Target, opt: opt, tenant: tenant}
	h.StartWorker()
	return h
}

// StartWorker (re)starts the worker on the same root and id and waits until
// it is attached.
func (h *Harness) StartWorker() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	opt := h.opt
	safego.Go(ctx, "workspacetest.worker", func() {
		defer close(finished)
		_ = worker.Run(ctx, opt)
	})
	h.stop = func() { cancel(); <-finished }
	h.t.Cleanup(h.StopWorker)
	h.WaitAttached(true)
}

// AddWorker attaches a second worker for the same tenant, on a root of its
// own, holding at most capacity workspaces, and waits until it is attached.
// It returns the core's id for it; the worker stops with the test. With
// the first worker's capacity bounded too, opens spill from one to the
// other, which is how a test puts two workspaces on two workers.
func (h *Harness) AddWorker(id string, capacity int) string {
	h.t.Helper()
	opt := h.opt
	opt.ID, opt.Root, opt.Capacity = id, h.t.TempDir(), capacity
	workerID := "tenant:" + h.tenant + "/" + id
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	safego.Go(ctx, "workspacetest.worker2", func() {
		defer close(finished)
		_ = worker.Run(ctx, opt)
	})
	h.t.Cleanup(func() { cancel(); <-finished })
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, w := range h.D.Workers() {
			if w.ID == workerID {
				return workerID
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for worker %s", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// personalToken, followed by a user, is that user's personal worker token
// in the harness's resolver (RFC GW P13). Not a real token.
const personalToken = "workspacetest-personal-"

// AddPersonalWorker attaches a personal worker of user (RFC GW P13) on the
// host engine, offering roots, and waits until it is attached. It returns
// the core's id for it; the worker stops with the test.
func (h *Harness) AddPersonalWorker(user string, roots []worker.Root) string {
	h.t.Helper()
	opt := h.opt
	opt.ID, opt.Root, opt.Token = "laptop", h.t.TempDir(), personalToken+user
	opt.Personal, opt.Roots, opt.Engine = true, roots, ""
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	safego.Go(ctx, "workspacetest.personal", func() {
		defer close(finished)
		_ = worker.Run(ctx, opt)
	})
	h.t.Cleanup(func() { cancel(); <-finished })
	deadline := time.Now().Add(10 * time.Second)
	// The dispatcher names it user:<tenant>/<hash of the user>/laptop; the
	// harness does not rebuild the hash, it finds the one personal worker
	// it attached for this user.
	for {
		for _, w := range h.D.Workers() {
			if strings.HasPrefix(w.ID, "user:"+h.tenant+"/") && strings.HasSuffix(w.ID, "/laptop") && !h.personal[w.ID] {
				if h.personal == nil {
					h.personal = map[string]bool{}
				}
				h.personal[w.ID] = true
				return w.ID
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s's personal worker", user)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// StopWorker stops the worker and waits until the dispatcher has let it go.
func (h *Harness) StopWorker() {
	if h.stop == nil {
		return
	}
	h.stop()
	h.stop = nil
	h.WaitAttached(false)
}

// WaitAttached waits up to 10 s for the worker to be attached (or not).
func (h *Harness) WaitAttached(want bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := false
		for _, w := range h.D.Workers() {
			got = got || w.ID == h.WorkerID
		}
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for the worker attached=%v", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Dir is workspace id's directory on the worker's disk.
func (h *Harness) Dir(id string) string { return filepath.Join(h.Root, id) }
