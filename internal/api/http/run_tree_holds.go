package http

import "sync"

// runTreeHolds keeps a run tree's ephemeral state — its ephemeral volumes and
// run-scope SQL database — alive past its top-level run while a detached team
// walk started inside the tree still runs.
//
// The tree's state is torn down when its top-level run ends, which was safe
// while every run in a tree ended before its root. A detached walk does not: it
// keeps the starter's root run id, so its members read and write the starter
// tree's volumes, and it outlives the run that started it. Torn down at the
// root's end, the walk's files vanished under it mid-run.
//
// A detached walk holds its tree from the moment its run opens until its run
// closes. The root's end with a hold outstanding records the purge as owed; the
// last hold released pays it. In-process only, and that is enough here: the
// walk is opened by a tool call inside the tree, so it runs on the replica the
// tree runs on. A walk that dies with its process holds nothing in the next
// one — the ephemeral sweeper covers that, by not purging a tree while any run
// beneath its root is still running (EphemeralVolumeSweepCandidates).
//
// Zero value works (test fixtures build a Server without a constructor).
type runTreeHolds struct {
	mu   sync.Mutex
	held map[string]int  // root run id → detached walks holding it
	owed map[string]bool // root run id → its top-level run ended while held
}

// hold takes one hold on the tree rooted at root.
func (h *runTreeHolds) hold(root string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = map[string]int{}
	}
	h.held[root]++
}

// release drops one hold and reports whether the tree's purge is now due: the
// last hold went and the top-level run already ended.
func (h *runTreeHolds) release(root string) (purge bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held[root] <= 0 {
		return false
	}
	h.held[root]--
	if h.held[root] > 0 {
		return false
	}
	delete(h.held, root)
	purge = h.owed[root]
	delete(h.owed, root)
	return purge
}

// deferPurge is asked when the tree's top-level run ends. It reports whether
// the purge must wait, recording it as owed to the last hold when it must.
func (h *runTreeHolds) deferPurge(root string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held[root] <= 0 {
		return false
	}
	if h.owed == nil {
		h.owed = map[string]bool{}
	}
	h.owed[root] = true
	return true
}
