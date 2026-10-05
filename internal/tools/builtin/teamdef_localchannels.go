package builtin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamdef_localchannels.go — a team's own channels, on the authoring side.
//
// A local channel's body is the definition a runtime ChannelDef takes, decoded
// into the same request type and checked by the same function
// (connector.NormalizeChannelFields), so a team cannot declare a channel a
// tenant could not create. On top of that, what does not fit a channel that
// lives inside a team is refused:
//
//   - scope global (one keyspace across tenants) and agent (a walk reads its
//     channels as the walk, not as one agent): a team's channel is shared by
//     the walks of that team in its tenant, or by one user's.
//   - publisher/period: a cadence channel is written by the runtime on the
//     operator's behalf, which only the operator's config may declare.
//   - hooks: the channel-hook worker resolves a channel's hooks by its
//     declared name, which a team's own channel does not have.
//   - name: the key under local.channels is the name.

// decodeLocalChannel turns a local channel's body into the definition the
// Channel tool and the walks resolve it to, without its stored name.
func decodeLocalChannel(body json.RawMessage) (tools.ChannelDef, error) {
	var req connector.ChannelCreateRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return tools.ChannelDef{}, fmt.Errorf("not a channel definition: %v", err)
	}
	switch {
	case req.Name != "":
		return tools.ChannelDef{}, fmt.Errorf("name: the key under local.channels is the channel's name; remove `name`")
	case req.Publisher != "" || req.Period != "":
		return tools.ChannelDef{}, fmt.Errorf("publisher/period: a channel the runtime writes on a cadence is declared only in the operator's config")
	case len(req.Hooks) > 0:
		return tools.ChannelDef{}, fmt.Errorf("hooks: a team's own channel cannot carry hooks")
	}
	scope, semantic, err := connector.NormalizeChannelFields(strings.TrimSpace(req.Scope), strings.TrimSpace(req.Semantic), req.DefaultTTL, req.MaxMessages)
	if err != nil {
		return tools.ChannelDef{}, err
	}
	if scope != "tenant" && scope != "user" {
		return tools.ChannelDef{}, fmt.Errorf("scope must be tenant (shared by the team's walks in its tenant) or user (one per user), got %q", scope)
	}
	return tools.ChannelDef{
		Scope:       scope,
		Semantic:    semantic,
		DefaultTTL:  req.DefaultTTL,
		MaxMessages: req.MaxMessages,
		Hold:        req.Hold,
	}, nil
}

// LocalChannelDefinition is the definition of team `team`'s own channel
// `name`, named by the reserved name its messages are stored under. Every
// reader of a team's own channel — the walks, the Channel tool, the channel
// writer — resolves it through here, so they agree on what it is.
func LocalChannelDefinition(team, name string, body json.RawMessage) (tools.ChannelDef, error) {
	def, err := decodeLocalChannel(body)
	if err != nil {
		return tools.ChannelDef{}, err
	}
	def.Name = store.TeamChannelName(team, name)
	return def, nil
}

// checkLocalChannels refuses, at create and fork, a definition whose own
// channels could not work, and any channel reference that names the reserved
// prefix directly. Store-free and caller-free: what a local channel may be
// does not depend on who writes it (a global one, which would, is refused).
func checkLocalChannels(team string, def teamgraph.Definition) error {
	// The reserved spelling, wherever a definition can name a channel. Checked
	// for every team: a team with no channels of its own must not reach
	// another's by writing its stored name.
	for _, ref := range teamgraph.ChannelRefs(def) {
		if store.IsTeamChannelName(ref.Channel) {
			return fmt.Errorf("state %q %s: %q is a reserved name — a team's own channel is named \"./<name>\"", ref.State, ref.Field, ref.Channel)
		}
	}
	if def.Channels != nil {
		for _, entry := range append(append([]string(nil), def.Channels.Publish...), def.Channels.Subscribe...) {
			if store.IsTeamChannelName(strings.TrimSpace(entry)) {
				return fmt.Errorf("channels: %q is a reserved name — a team holds its own channels without an ACL entry", entry)
			}
		}
	}
	names := def.LocalChannelNames()
	if len(names) > 0 {
		// Stored as "_team/<team>/<name>", which must split one way.
		if err := teamgraph.ValidateName(team); err != nil {
			return fmt.Errorf("local: a team that declares its own channels needs a name of one segment "+
				"(A-Z a-z 0-9 _ -), because each is stored under the team's name: %w", err)
		}
	}
	for _, name := range names {
		if _, err := decodeLocalChannel(def.Local.Channels[name]); err != nil {
			return fmt.Errorf("local.channels[%q]: %w", name, err)
		}
	}
	return checkLocalAgentChannelGrants(def)
}

// checkLocalAgentChannelGrants refuses a team's own agent whose channel ACL
// names "./x" for a channel the team does not declare, or names the reserved
// prefix. A local agent is granted one of the team's channels by listing
// "./<name>" in its own `channels`, side by side as for any channel.
//
// A body that does not decode is skipped: checkLocalAgents refuses it with
// the decoder's own words.
func checkLocalAgentChannelGrants(def teamgraph.Definition) error {
	for _, agent := range def.LocalAgentNames() {
		ov, err := decodeAgentOverlay(def.Local.Agents[agent])
		if err != nil {
			continue
		}
		for _, side := range []struct {
			key  string
			list []string
		}{{"publish", ov.Channels.Publish}, {"subscribe", ov.Channels.Subscribe}} {
			for _, entry := range side.list {
				entry = strings.TrimSpace(entry)
				if store.IsTeamChannelName(entry) {
					return fmt.Errorf("local.agents[%q]: channels.%s: %q is a reserved name — name the team's own channel as \"./<name>\"", agent, side.key, entry)
				}
				name, isLocal := teamgraph.LocalRef(entry)
				if !isLocal {
					continue
				}
				if _, ok := def.LocalChannel(name); !ok {
					return fmt.Errorf("local.agents[%q]: channels.%s: %q names a channel the team does not declare under local.channels", agent, side.key, entry)
				}
			}
		}
	}
	return nil
}
