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

// Cluster is several core replicas (dispatchers) over one store, as a
// deployment with more than one replica runs them (RFC GW P12 plan §3.5),
// and one worker attached to those its Attach lists.
type Cluster struct {
	// D are the replicas' dispatchers.
	D []*dispatch.Dispatcher
	// Root is the worker's root: a workspace's directory is Root/<id>.
	Root string
	// WorkerID is the core's id for the worker.
	WorkerID string
	t        *testing.T
	addrs    []string
	opt      worker.Options
	stop     func()
	attached []int
}

// ClusterOptions configures StartCluster.
type ClusterOptions struct {
	// Replicas is how many dispatchers to serve (at least 1).
	Replicas int
	// Attach are the replicas, by index, the worker attaches to; nil is
	// all of them.
	Attach []int
	// RecheckEvery is each dispatcher's recheck interval; zero is the
	// dispatcher's default.
	RecheckEvery time.Duration
	// Worker are the worker's options, as Start takes them.
	Worker worker.Options
}

// StartCluster serves o.Replicas dispatchers over st on loopback ports and
// attaches one host-engine worker serving tenant to the replicas o.Attach
// names, and waits until it is attached to each.
func StartCluster(t *testing.T, st dispatch.Store, tenant string, o ClusterOptions) *Cluster {
	t.Helper()
	if o.Replicas < 1 {
		o.Replicas = 1
	}
	c := &Cluster{t: t, WorkerID: "tenant:" + tenant + "/w1"}
	for range o.Replicas {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		d := dispatch.New(dispatch.Options{RecheckEvery: o.RecheckEvery})
		d.Activate(st, func(_ context.Context, bearer string) (auth.Principal, bool) {
			if bearer != token {
				return auth.Principal{}, false
			}
			return auth.Principal{TenantID: tenant, Subject: "worker", Scopes: []string{auth.ScopeWorkersAttach}, TokenDefID: "def-worker"}, true
		})
		safego.Go(context.Background(), "workspacetest.serve", func() { _ = d.Serve(l) })
		t.Cleanup(d.Stop)
		c.D = append(c.D, d)
		c.addrs = append(c.addrs, l.Addr().String())
	}
	c.attached = o.Attach
	if c.attached == nil {
		for i := range o.Replicas {
			c.attached = append(c.attached, i)
		}
	}
	opt := o.Worker
	if opt.Root == "" {
		opt.Root = t.TempDir()
	}
	if opt.Toolchains == nil {
		opt.Toolchains = []string{"python", "node"}
	}
	if opt.IdleStreams == 0 {
		opt.IdleStreams = 4
	}
	targets := make([]string, 0, len(c.attached))
	for _, i := range c.attached {
		targets = append(targets, c.addrs[i])
	}
	opt.Target, opt.Token, opt.ID, opt.Insecure = strings.Join(targets, ","), token, "w1", true
	c.opt, c.Root = opt, opt.Root
	c.StartWorker()
	return c
}

// StartWorker (re)starts the worker on the same root and id, and waits
// until it is attached to every replica it lists.
func (c *Cluster) StartWorker() {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	opt := c.opt
	safego.Go(ctx, "workspacetest.cluster-worker", func() {
		defer close(finished)
		_ = worker.Run(ctx, opt)
	})
	c.stop = func() { cancel(); <-finished }
	c.t.Cleanup(c.StopWorker)
	c.WaitAttached(true)
}

// StopWorker stops the worker and waits until every replica let it go.
func (c *Cluster) StopWorker() {
	if c.stop == nil {
		return
	}
	c.stop()
	c.stop = nil
	c.WaitAttached(false)
}

// WaitAttached waits up to 10 s for the worker to be attached (or not) to
// every replica it lists.
func (c *Cluster) WaitAttached(want bool) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		all := true
		for _, i := range c.attached {
			got := false
			for _, w := range c.D[i].Workers() {
				got = got || w.ID == c.WorkerID
			}
			all = all && got == want
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for the worker attached=%v to replicas %v", want, c.attached)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Dir is workspace id's directory on the worker's disk.
func (c *Cluster) Dir(id string) string { return filepath.Join(c.Root, id) }
