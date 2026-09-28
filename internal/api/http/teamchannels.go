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
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"

	"github.com/denn-gubsky/loomcycle/internal/channels"
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
	def, ok := io.srv.ResolveChannelScope(ctx, io.tenant, channel)
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

// readPoll is how often Read re-peeks while it waits and the server has no bus
// to wake it. A publish on this replica wakes the bus at once; the poll covers
// a server without one.
const readPoll = 250 * time.Millisecond

// Read returns up to batch messages after the source's committed cursor,
// waiting up to waitMS for at least want of them (0 = the operator's long-poll
// cap). It peeks rather than subscribes: the cursor advances only when the
// Starter acks, so a crash between reading and acking redelivers the batch.
//
// Reading from the committed cursor is what makes an ack mean anything: a
// read from the oldest message re-saw every acked message on the next wave,
// and a wave that arrived a moment after the read found nothing and failed the
// walk instead of waiting for it.
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
	if want < 1 {
		want = 1
	}
	from, err := io.srv.store.ChannelCommittedCursor(ctx, io.tenant, channel, scope, scopeID)
	if err != nil {
		return nil, "", err
	}
	deadline := time.Now().Add(io.readWait(waitMS))
	for {
		// Register before peeking: a publish that lands between the peek and the
		// wait still wakes this read.
		var waker chan struct{}
		if io.srv.channelBus != nil {
			waker = io.srv.channelBus.Register(channel)
		}
		msgs, err := io.srv.store.ChannelPeek(ctx, io.tenant, channel, scope, scopeID, from, batch)
		remaining := time.Until(deadline)
		if err != nil || len(msgs) >= want || remaining <= 0 {
			if waker != nil {
				io.srv.channelBus.Unregister(channel, waker)
			}
			if err != nil {
				return nil, "", err
			}
			return toTeamMessages(msgs)
		}
		if waker == nil {
			waker = make(chan struct{}) // never fires: the timer below paces the poll
			if remaining > readPoll {
				remaining = readPoll
			}
		}
		t := time.NewTimer(remaining)
		select {
		case <-waker:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		if io.srv.channelBus != nil {
			io.srv.channelBus.Unregister(channel, waker)
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
	}
}

// readWait is how long a Read may wait: the author's wait_ms, or the
// operator's long-poll cap when it is 0, and never more than that cap. A cap
// of 0 (long-poll disabled) means a Read never waits.
func (io *teamChannelIO) readWait(waitMS int) time.Duration {
	capMS := io.srv.cfg().Env.ChannelsLongPollCapMS
	if waitMS <= 0 || waitMS > capMS {
		waitMS = capMS
	}
	if waitMS <= 0 {
		return 0
	}
	return time.Duration(waitMS) * time.Millisecond
}

func toTeamMessages(msgs []store.ChannelMessage) ([]teamrun.ChannelMessage, string, error) {
	out := make([]teamrun.ChannelMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, teamrun.ChannelMessage{ID: m.ID, Payload: m.Payload})
	}
	cursor := ""
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		cursor = store.EncodeChannelCursor(last.VisibleAt, last.ID)
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

// PublishSink publishes a Starter's per-run result, marked as one: a channel
// hook on the sink cannot make it disappear (a drop is delivered as an error
// result), because the downstream fan-in counts one per run.
func (io *teamChannelIO) PublishSink(ctx context.Context, channel string, payload json.RawMessage) error {
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
	_, err = io.srv.writeChannel(ctx, channels.WriteRequest{
		Channel: channel, TenantID: io.tenant, Scope: scope, ScopeID: scopeID,
		Payload: payload, PublishedBy: "_team", MaxMessages: def.MaxMessages,
		ExpiresAt: ttlExpiry(def.DefaultTTL), Origin: channels.OriginStarterSink,
	})
	return err
}
