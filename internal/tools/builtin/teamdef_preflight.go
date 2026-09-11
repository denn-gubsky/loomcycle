package builtin

// teamdef_preflight.go — catch at CREATE what would otherwise fail at RUN.
//
// A team graph can be perfectly valid and still be unrunnable, because the two
// things it references live outside it: the channels it reads and writes, and
// the agents it runs. Both failures used to surface the same way — you author a
// workflow, it saves, you run it, and a state deep in the graph says it cannot
// reach its own source. By then the def is promoted and something may already
// have consumed it.
//
// The refusals here are written to be ACTED ON, not merely understood: each one
// carries the exact yaml or overlay line to add. A preflight that says "channel
// not declared" and leaves the author to work out the fix has moved the
// discovery earlier without making it cheaper.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// preflightChannels refuses a definition whose channel references cannot work.
//
// Two independent checks, because they fail for different reasons and have
// different fixes:
//
//  1. THE TEAM ACL — does the definition grant itself the side it uses? This is
//     purely internal to the def, so it always runs. It is the check that would
//     have caught the commonest shape of broken workflow: a Starter with no
//     `channels` block at all.
//  2. THE OPERATOR'S DECLARED SET — does the channel exist? This needs the
//     catalog, and when none is wired the check is SKIPPED rather than failed.
//     A tool that cannot see the declarations cannot tell "undeclared" from "I
//     have no list", and refusing on the second would break every create on a
//     plane that simply never wired a catalog.
func (t *TeamDef) preflightChannels(ctx context.Context, def teamgraph.Definition) error {
	refs := teamgraph.ChannelRefs(def)
	if len(refs) == 0 {
		return nil
	}

	// The ACL check, against the ONE runtime matcher — never a second copy of
	// its rules. A preflight that accepted what the runtime refuses would be
	// worse than none: it would certify a def that cannot run.
	for _, ref := range refs {
		if channelAllowed(ref.Channel, def.GrantList(ref.Side)) {
			continue
		}
		return fmt.Errorf("state %q uses channel %q as its %s, but the team's own ACL does not grant %s on it — "+
			"a Starter resolves its channels under the TEAM's authority, so add it to the definition:\n%s",
			ref.State, ref.Channel, ref.Field, ref.Side, aclFixFor(def, refs))
	}

	catalog := t.channelCatalog(ctx)
	if catalog == nil {
		return nil
	}
	for _, ref := range refs {
		if _, ok := catalog[ref.Channel]; !ok {
			return fmt.Errorf("state %q names channel %q, which is not declared — "+
				"declare it in the operator config:\n%s\nor at runtime with "+
				"`ChannelDef op=create name=%s overlay={\"scope\":\"user\"}`",
				ref.State, ref.Channel, channelYAMLFor(ref.Channel), ref.Channel)
		}
	}
	return nil
}

// channelCatalog reads the declared channel set, or nil when none is wired.
func (t *TeamDef) channelCatalog(ctx context.Context) map[string]tools.ChannelDef {
	if t.ChannelCatalog == nil {
		return nil
	}
	return t.ChannelCatalog(ctx)
}

// aclFixFor renders the `channels` block this definition needs — the WHOLE
// block, not just the missing line.
//
// The whole block, because an author who is missing one grant is usually
// missing the block, and a fragment they have to splice is a second chance to
// get it wrong. It is sorted so the same graph always produces the same text.
func aclFixFor(def teamgraph.Definition, refs []teamgraph.ChannelRef) string {
	pub, sub := map[string]bool{}, map[string]bool{}
	for _, ref := range refs {
		if ref.Side == teamgraph.SideSubscribe {
			sub[ref.Channel] = true
			continue
		}
		pub[ref.Channel] = true
	}
	var b strings.Builder
	b.WriteString("  \"channels\": {")
	first := true
	for _, side := range []struct {
		key string
		set map[string]bool
	}{{"publish", pub}, {"subscribe", sub}} {
		if len(side.set) == 0 {
			continue
		}
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, "\n    %q: [%s]", side.key, quotedList(side.set))
	}
	b.WriteString("\n  }")
	return b.String()
}

// channelYAMLFor renders the operator-config stanza for a missing channel.
//
// scope=user is the suggestion because it is the one a workflow almost always
// wants — per-end-user isolation — and because suggesting `global` would hand
// an author a cross-tenant channel they did not ask for. The comment says the
// alternatives rather than the value silently deciding for them.
func channelYAMLFor(name string) string {
	return "  channels:\n    " + name + ":\n      scope: user   # agent | user | global"
}

func quotedList(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, fmt.Sprintf("%q", k))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
