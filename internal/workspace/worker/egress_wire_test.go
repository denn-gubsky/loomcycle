package worker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// A container worker's session network (P4 PR 4b) must reach the worker's
// egress handler (PR 4a), not the engine's placeholder that answers every
// request 503. The worker does not exist yet when the engine is built, so the
// handler reads it when a session starts: before then it is the placeholder.
func TestWireEgress_SessionsReachTheWorkersEgressHandler(t *testing.T) {
	opt := Options{Container: engine.ContainerOptions{Egress: &engine.ContainerEgress{Dir: "/x"}}}
	var self *worker
	wireEgress(&opt, &self)
	h := opt.Container.Egress.Handler
	if h == nil {
		t.Fatal("no egress handler wired into the container engine")
	}
	connect := func() int {
		rec := httptest.NewRecorder()
		h("ws1").ServeHTTP(rec, httptest.NewRequest(http.MethodConnect, "http://egress/", nil))
		return rec.Code
	}
	if got := connect(); got != http.StatusServiceUnavailable {
		t.Errorf("before the worker exists = %d, want 503", got)
	}
	self = &worker{}
	if got := connect(); got != http.StatusBadRequest {
		t.Errorf("a proxy request through the wired handler = %d, want the worker's own 400", got)
	}
}

// A host worker, and a container worker with no egress, wire nothing.
func TestWireEgress_NoEgressWiresNothing(t *testing.T) {
	opt := Options{}
	var self *worker
	wireEgress(&opt, &self)
	if opt.Container.Egress != nil {
		t.Errorf("egress = %+v, want none", opt.Container.Egress)
	}
}
