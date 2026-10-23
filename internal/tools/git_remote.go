package tools

// GitRemotePolicyValue is what a run's Git tool may do over the network (RFC
// FB): clone, fetch, pull and ls_remote need Fetch; push needs Push. The zero
// value allows nothing, so a run whose agent never listed git_remote, or whose
// entry point built no policy, keeps its Git tool to local repository work.
type GitRemotePolicyValue struct{ Fetch, Push bool }

// GitRemoteFor is the policy an agent definition's git_remote declares.
// Unknown values are ignored here because config.ValidateGitRemote refuses
// them wherever a definition is written. Push counts only alongside fetch:
// validation refuses ["push"] alone, and a value that reached here anyway (a
// row stored before that check) must not grant a push the definition could
// not have been written to hold.
func GitRemoteFor(ops []string) GitRemotePolicyValue {
	var fetch, push bool
	for _, op := range ops {
		switch op {
		case "fetch":
			fetch = true
		case "push":
			push = true
		}
	}
	return GitRemotePolicyValue{Fetch: fetch, Push: push && fetch}
}

// Allows reports whether the run's Git tool may perform the network operation
// op, "fetch" or "push". Any other op is refused.
func (v GitRemotePolicyValue) Allows(op string) bool {
	switch op {
	case "fetch":
		return v.Fetch
	case "push":
		return v.Push
	}
	return false
}

// NarrowGitRemote is a spawned child's policy: each operation only when both
// its parent run and its own definition allow it.
func NarrowGitRemote(parent, own GitRemotePolicyValue) GitRemotePolicyValue {
	return GitRemotePolicyValue{Fetch: parent.Fetch && own.Fetch, Push: parent.Push && own.Push}
}

// Ops is the policy as a git_remote list, for reporting and for the spawn
// record a resume narrows against. Never nil, so a report of "nothing" reads
// as an empty list rather than as an absent field.
func (v GitRemotePolicyValue) Ops() []string {
	out := []string{}
	if v.Fetch {
		out = append(out, "fetch")
	}
	if v.Push {
		out = append(out, "push")
	}
	return out
}
