package teamgraph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// localchannels.go — the channels a team declares for itself.
//
// A local channel exists only inside its team: its definition is part of the
// team's (versioned and hashed with it), and only the team's walks and its own
// agents can reach it. A channel-bearing field (see ChannelRefs) names one as
// "./<name>"; a bare name keeps meaning a channel the operator or the tenant
// declared, so no definition written before this changes meaning.
//
// The TEAM holds both sides of its own channels. They need no entry in the
// definition's `channels` ACL — which is checked against what the author holds,
// and the author holds no grant on a channel that does not exist outside the
// team — and may not have one.
//
// What a body may say (scope, ttl, ...) depends on the runtime's channel
// rules, which this leaf package cannot import; the authoring caller judges
// it. Here: the names, the count, and that every "./<name>" is declared.

// MaxLocalChannels bounds how many channels a team may declare for itself,
// the number MaxLocalAgents uses, for the same reason.
const MaxLocalChannels = 64

// LocalChannel returns the definition of the local channel `name`, if the
// team declares it.
func (d Definition) LocalChannel(name string) (json.RawMessage, bool) {
	if d.Local == nil {
		return nil, false
	}
	body, ok := d.Local.Channels[name]
	return body, ok
}

// LocalChannelNames returns the declared local channel names, sorted.
func (d Definition) LocalChannelNames() []string {
	if d.Local == nil {
		return nil
	}
	out := make([]string, 0, len(d.Local.Channels))
	for name := range d.Local.Channels {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// validateLocalChannels checks the declared channels' count and names, and
// that the team's own ACL does not name one of them.
func validateLocalChannels(d Definition) error {
	if d.Local != nil && len(d.Local.Channels) > MaxLocalChannels {
		return fmt.Errorf("team definition: local.channels declares %d channels, more than the maximum %d", len(d.Local.Channels), MaxLocalChannels)
	}
	for _, name := range d.LocalChannelNames() {
		// The variable-name grammar: one segment of [A-Za-z0-9_-], 1..64 —
		// the channel is stored under a name of which this is the last segment.
		if !varNameRe.MatchString(name) {
			return fmt.Errorf("team definition: local.channels: name %q must be one segment of A-Z a-z 0-9 _ -, at most 64 characters", name)
		}
	}
	if d.Channels != nil {
		for _, side := range []struct {
			key  string
			list []string
		}{{"publish", d.Channels.Publish}, {"subscribe", d.Channels.Subscribe}} {
			for _, entry := range side.list {
				if strings.HasPrefix(strings.TrimSpace(entry), LocalRefPrefix) {
					return fmt.Errorf("team definition: channels.%s: %q names one of the team's own channels, "+
						"which the team may always publish to and read — remove it from the ACL", side.key, entry)
				}
			}
		}
	}
	return nil
}

// CheckLocalChannelRefs reports the first "./<name>" channel reference that
// names a channel the definition does not declare. A walk checks it again
// before it starts, for the reason CheckLocalRefs gives.
func CheckLocalChannelRefs(d Definition) error {
	for _, ref := range ChannelRefs(d) {
		name, isLocal := LocalRef(ref.Channel)
		if !isLocal {
			continue
		}
		if _, ok := d.LocalChannel(name); ok {
			continue
		}
		declared := "it declares none"
		if names := d.LocalChannelNames(); len(names) > 0 {
			declared = "declared: " + strings.Join(names, ", ")
		}
		return fmt.Errorf("team definition: state %q %s: %q names a channel the team does not declare under local.channels (%s)",
			ref.State, ref.Field, ref.Channel, declared)
	}
	return nil
}
