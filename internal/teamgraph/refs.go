package teamgraph

// refs.go — what a definition NAMES but does not contain.
//
// A team graph is self-contained except for two kinds of reference: the
// channels it reads and writes, and the agents it runs. Both are resolved
// elsewhere and both can rot — a channel is deleted from the operator's
// config, an agent is retired — leaving a definition that validates perfectly
// and fails at the first state that needs it.
//
// Enumerating them HERE, once, is the point. The preflight at create, the
// sweep at verify and anything that comes later all walk the same list, so a
// new channel-bearing or agent-bearing field cannot be checked in one place
// and forgotten in another. Adding such a field to Handler without adding it
// here is the bug this file exists to make loud: TestChannelRefs_CoversEvery
// ChannelBearingField pins it.

// ChannelSide is which half of an ACL a reference needs. The values match the
// `channels:` yaml keys so a refusal can name the exact line to add.
type ChannelSide string

const (
	SidePublish   ChannelSide = "publish"
	SideSubscribe ChannelSide = "subscribe"
)

// ChannelRef is one channel a definition names, and why.
type ChannelRef struct {
	// Channel is the name as written.
	Channel string
	// Side is the grant this use requires.
	Side ChannelSide
	// State is the node that names it, and Field which of its fields — both
	// carried so a refusal points at the line the author wrote rather than at
	// the graph in general.
	State string
	Field string
}

// ChannelRefs returns every channel the definition names, in state order, with
// duplicates kept: the same channel used as a source in one state and a sink in
// another needs BOTH grants, and collapsing them would hide one.
func ChannelRefs(d Definition) []ChannelRef {
	var out []ChannelRef
	for _, s := range d.States {
		h := s.Handler
		if h.Source != nil && h.Source.Channel != "" {
			out = append(out, ChannelRef{h.Source.Channel, SideSubscribe, s.ID, "source"})
		}
		if h.Sink != nil && h.Sink.Channel != "" {
			out = append(out, ChannelRef{h.Sink.Channel, SidePublish, s.ID, "sink"})
		}
		if h.Channel != "" {
			out = append(out, ChannelRef{h.Channel, SidePublish, s.ID, "channel"})
		}
	}
	return out
}

// AgentRef is one agent name a definition runs, and the field that names it.
type AgentRef struct {
	Agent string
	State string
	Field string
}

// AgentRefs returns every agent name the definition runs, in state order.
//
// Duplicates are kept for the same reason ChannelRefs keeps them: a report that
// collapsed them could not say WHICH node is broken, which is the only thing
// the reader can act on.
func AgentRefs(d Definition) []AgentRef {
	var out []AgentRef
	for _, s := range d.States {
		h := s.Handler
		if h.Agent != "" {
			out = append(out, AgentRef{h.Agent, s.ID, "agent"})
		}
		for _, a := range h.Agents {
			if a != "" {
				out = append(out, AgentRef{a, s.ID, "agents"})
			}
		}
		if h.Consolidator != "" {
			out = append(out, AgentRef{h.Consolidator, s.ID, "consolidator"})
		}
		if h.Fanout != nil {
			if h.Fanout.Agent != "" {
				out = append(out, AgentRef{h.Fanout.Agent, s.ID, "fanout.agent"})
			}
			for _, a := range h.Fanout.Agents {
				if a != "" {
					out = append(out, AgentRef{a, s.ID, "fanout.agents"})
				}
			}
		}
	}
	return out
}

// GrantList returns the team's own allowlist for one side of a reference. A nil
// Channels block yields nothing — which is correct and is the case that bites:
// a definition with a Starter and no `channels` block parses, validates, and
// then refuses its own source the first time it runs.
//
// It returns the LIST rather than a verdict on purpose. Matching a name against
// an allowlist is the runtime's rule, and there must be exactly one
// implementation of it — a second one here could drift into a preflight that
// accepts what the runtime refuses, which is worse than no preflight. teamgraph
// is a leaf package and cannot import the matcher, so it hands the caller the
// list and the caller applies the one true matcher.
func (d Definition) GrantList(side ChannelSide) []string {
	if d.Channels == nil {
		return nil
	}
	if side == SideSubscribe {
		return d.Channels.Subscribe
	}
	return d.Channels.Publish
}
