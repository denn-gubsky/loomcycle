// teamchannels.go — the Starter's channel executor (RFC CY L4).
//
// teamrun is a pure graph-walker: it knows a Starter reads a channel, and
// nothing about what a channel IS. This is the other side of that seam — the
// store, the scope resolution and the team ACL live here, where they already
// do for every other channel caller.
//
// THE TEAM IS THE ACL SUBJECT. A Starter reads its source and publishes its
// sink under `Definition.Channels`, validated at create/fork to only narrow
// what its author held. That is what lets an agent in a wave hold no channel
// grant in either direction: the workflow has the authority, not the workers.
package http

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamChannelIO implements teamrun.ChannelIO for one walk, closed over that
// walk's ACL. One per run: the ACL is the definition's, and a definition is
// what a walk is walking.
type teamChannelIO struct {
	srv    *Server
	acl    *teamgraph.TeamChannels
	tenant string
}

// newTeamChannelIO returns the executor for a walk, or nil when the server has
// no store (the same posture every other optional collaborator takes).
func (s *Server) newTeamChannelIO(ctx context.Context, d teamgraph.Definition) *teamChannelIO {
	if s.store == nil {
		return nil
	}
	return &teamChannelIO{srv: s, acl: d.Channels, tenant: tenantFromCtx(ctx)}
}

// allowed checks a channel against the TEAM's allowlist, reusing the agent
// matcher verbatim so a team ACL and an agent ACL mean the same thing —
// including the trailing `/*` prefix wildcard and the path-traversal guard.
//
// No ACL means no channel: default-deny, exactly as an agent with no channels
// block gets. A workflow that reads a channel has to say which.
func (io *teamChannelIO) allowed(side, channel string) error {
	if io.acl == nil {
		return fmt.Errorf("team has no `channels` block, so %s on %q is refused — "+
			"declare it in the definition (it may only narrow what the author holds)", side, channel)
	}
	list := io.acl.Subscribe
	if side == "publish" {
		list = io.acl.Publish
	}
	if !builtin.ChannelAllowed(channel, list) {
		return fmt.Errorf("%s on %q is not in the team's channels.%s allowlist (%v)", side, channel, side, list)
	}
	return nil
}

// resolve maps a channel name to its declared (scope, scope_id), refusing an
// undeclared channel. A Starter is a walk-level subject with no agent identity
// of its own, so an agent-scoped channel has no scope_id to key on and is
// refused with that reason rather than silently keying on something arbitrary.
func (io *teamChannelIO) resolve(ctx context.Context, channel string) (tools.ChannelDef, store.MemoryScope, string, error) {
	def, ok := io.srv.ResolveChannelScope(ctx, channel)
	if !ok {
		return def, "", "", fmt.Errorf("channel %q is not declared (static yaml or runtime substrate)", channel)
	}
	switch def.Scope {
	case "global":
		return def, store.MemoryScopeGlobal, "", nil
	case "tenant":
		return def, store.MemoryScopeTenant, "", nil
	case "user":
		uid := tools.RunIdentity(ctx).UserID
		if uid == "" {
			return def, "", "", fmt.Errorf("channel %q is scope=user but the walk has no user_id", channel)
		}
		return def, store.MemoryScopeUser, uid, nil
	default:
		return def, "", "", fmt.Errorf("channel %q has scope=%q, which a team workflow cannot key on — "+
			"a starter reads on the WALK's behalf, not as one agent", channel, def.Scope)
	}
}

func (io *teamChannelIO) Read(ctx context.Context, channel string, want, batch, waitMS int) ([]teamrun.ChannelMessage, string, error) {
	if err := io.allowed("subscribe", channel); err != nil {
		return nil, "", err
	}
	_, scope, scopeID, err := io.resolve(ctx, channel)
	if err != nil {
		return nil, "", err
	}
	if batch <= 0 {
		batch = 10
	}
	// PEEK, not subscribe: the cursor advances on ack, after the wave's results
	// are in. Subscribe would commit on read and lose a batch to any crash
	// between reading and dispatching.
	msgs, err := io.srv.store.ChannelPeek(ctx, io.tenant, channel, scope, scopeID, "", batch)
	if err != nil {
		return nil, "", err
	}
	out := make([]teamrun.ChannelMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, teamrun.ChannelMessage{ID: m.ID, Payload: m.Payload})
	}
	cursor := ""
	if len(out) > 0 {
		cursor = store.EncodeChannelCursor(msgs[len(msgs)-1].VisibleAt, msgs[len(msgs)-1].ID)
	}
	return out, cursor, nil
}

func (io *teamChannelIO) Ack(ctx context.Context, channel, cursor string) error {
	if err := io.allowed("subscribe", channel); err != nil {
		return err
	}
	_, scope, scopeID, err := io.resolve(ctx, channel)
	if err != nil {
		return err
	}
	return io.srv.store.ChannelAck(ctx, io.tenant, channel, scope, scopeID, cursor)
}

func (io *teamChannelIO) Publish(ctx context.Context, channel string, payload json.RawMessage) error {
	if err := io.allowed("publish", channel); err != nil {
		return err
	}
	def, scope, scopeID, err := io.resolve(ctx, channel)
	if err != nil {
		return err
	}
	if io.srv.systemPublisher == nil {
		return fmt.Errorf("no system publisher wired")
	}
	// Through the system publisher, so a `hold:` on the sink is honoured by the
	// one check that covers every internal writer — the rule the hold census
	// settled: a writer either resolves the channel definition or honours
	// nothing from it. This one resolves it.
	_, err = io.srv.systemPublisher.PublishNow(ctx, channel, io.tenant, scope, scopeID,
		payload, "_team", def.MaxMessages, def.DefaultTTL)
	return err
}
