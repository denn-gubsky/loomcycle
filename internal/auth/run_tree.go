package auth

// RunLink is what an allowlist walk reads of one run: its id, its agent, and
// the run that spawned it ("" for a top-level run).
type RunLink struct {
	ID, Agent, ParentRunID string
}

// MaxAllowlistTreeHops bounds the walk from a run to the root of its spawn
// tree. Parent pointers are rows, so a corrupt chain must not loop forever; a
// tree deeper than this is refused rather than half-checked.
const MaxAllowlistTreeHops = 64

// maxRunTreeMemo bounds how many runs one RunTree remembers. A request reads a
// page of rows and their ancestors, far fewer; a run-state stream lives for
// hours and sees every run its user starts, so its memory is dropped and
// rebuilt rather than grown without end.
const maxRunTreeMemo = 4096

// RunTree answers whether a principal's agent allowlist reaches runs and
// sessions: whether each run's spawn tree was started as a run of an allowed
// agent. A token limited to certain agents may only START those, so reading,
// steering or controlling a run of any other agent in its tenant would hand it
// what it could never have started.
//
// The tree's ROOT decides, not the run's own agent: an agent the allowed one
// spawns is part of the run the token started, as the start gate already lets
// it be.
//
// Every answer is true, with no read, when there is no principal or no
// allowlist. Otherwise it fails closed: no reader, a run that cannot be read,
// a loop, or a chain past MaxAllowlistTreeHops is outside the list.
//
// A RunTree remembers each run it reads, so a listing walks shared ancestors
// once. It is for one request or one stream, and is not safe for concurrent
// use.
type RunTree struct {
	p    Principal
	ok   bool
	read func(runID string) (RunLink, error)
	memo map[string]RunLink
}

// NewRunTree is a RunTree for the principal (p, ok) reading runs with read.
// A nil read makes every restricted answer false.
func NewRunTree(p Principal, ok bool, read func(runID string) (RunLink, error)) *RunTree {
	return &RunTree{p: p, ok: ok, read: read}
}

// Restricted reports whether the principal has an agent allowlist, i.e.
// whether any answer can be false.
func (t *RunTree) Restricted() bool {
	return t.ok && t.p.Restrictions.RunPlaneOnly()
}

// Reaches reports whether the allowlist reaches the run.
func (t *RunTree) Reaches(runID string) bool {
	if !t.Restricted() {
		return true
	}
	seen := map[string]bool{}
	for hop := 0; hop <= MaxAllowlistTreeHops; hop++ {
		if seen[runID] {
			return false
		}
		seen[runID] = true
		link, ok := t.link(runID)
		if !ok {
			return false
		}
		if link.ParentRunID == "" {
			return t.p.Restrictions.AgentAllowed(link.Agent)
		}
		runID = link.ParentRunID
	}
	return false
}

// ReachesKnown is Reaches for a run the caller has already read: the walk
// starts from link instead of reading it again.
func (t *RunTree) ReachesKnown(link RunLink) bool {
	if !t.Restricted() {
		return true
	}
	if link.ID == "" {
		return false
	}
	t.remember(link)
	return t.Reaches(link.ID)
}

// ReachesSession reports whether the allowlist reaches a session: whether it
// reaches EVERY run in it. A session holds the history of each of its runs,
// and a session a sub-run opened belongs to the tree that run is in, so its
// own agent is not enough. A session with no runs holds no run history; it is
// judged by its own agent. runs is read only for a restricted principal.
func (t *RunTree) ReachesSession(sessionAgent string, runs func() ([]RunLink, error)) bool {
	if !t.Restricted() {
		return true
	}
	if runs == nil {
		return false
	}
	links, err := runs()
	if err != nil {
		return false
	}
	if len(links) == 0 {
		return t.p.Restrictions.AgentAllowed(sessionAgent)
	}
	for _, l := range links {
		if !t.ReachesKnown(l) {
			return false
		}
	}
	return true
}

func (t *RunTree) link(runID string) (RunLink, bool) {
	if l, ok := t.memo[runID]; ok {
		return l, true
	}
	if t.read == nil {
		return RunLink{}, false
	}
	l, err := t.read(runID)
	if err != nil {
		return RunLink{}, false
	}
	// The reader answers for runID: a row filed under another id must not be
	// taken for it.
	l.ID = runID
	t.remember(l)
	return l, true
}

func (t *RunTree) remember(l RunLink) {
	if t.memo == nil || len(t.memo) >= maxRunTreeMemo {
		t.memo = make(map[string]RunLink)
	}
	t.memo[l.ID] = l
}
