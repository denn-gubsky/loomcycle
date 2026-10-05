package http

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// teamlocalchannels.go — the channels a team declares for itself, at run time.
//
// A team's own channel n of team T is stored as "_team/T/n" (store.
// TeamChannelName), a prefix nothing outside the team can declare or address.
// It is reached three ways, all resolving its definition from a TEAM VERSION
// through builtin.LocalChannelDefinition:
//
//	a walk's starter, channel state or input publish   the version it walks
//	a team's own agent, via the Channel tool           the version on its scope
//	the channel writer, deciding a hold                the writer's own scope
//
// Its keyspace is the team's tenant: a tenant-scoped one is shared by every
// walk of the team there (scope_id ""), a user-scoped one is the run's user's.

// teamLocalChannelTarget resolves team sc's own channel `local` from def — the
// definition of the version sc names — to the name its messages are stored
// under, its definition, and the keyspace a run of userID reads and writes.
//
// runTenant is the tenant the caller writes in. A team's own channels are its
// tenant's: an admin running another tenant's team by def_id would otherwise
// write its messages where deleting the team never looks.
func (s *Server) teamLocalChannelTarget(def teamgraph.Definition, sc store.TeamScope, runTenant, local, userID string) (string, tools.ChannelDef, store.MemoryScope, string, error) {
	ref := teamgraph.LocalRefPrefix + local
	if sc.Team == "" || sc.DefID == "" {
		return "", tools.ChannelDef{}, "", "", fmt.Errorf("%q names a team's own channel, which can only be used inside a walk of that team", ref)
	}
	if sc.Tenant != runTenant {
		return "", tools.ChannelDef{}, "", "", fmt.Errorf("team %q's own channels belong to its tenant, and this walk runs in another; run it as that tenant", sc.Team)
	}
	body, ok := def.LocalChannel(local)
	if !ok {
		return "", tools.ChannelDef{}, "", "", fmt.Errorf("team %q declares no channel of its own named %q", sc.Team, local)
	}
	cd, err := builtin.LocalChannelDefinition(sc.Team, local, body)
	if err != nil {
		return "", tools.ChannelDef{}, "", "", fmt.Errorf("team %q: its channel %q is unreadable: %w", sc.Team, local, err)
	}
	switch cd.Scope {
	case "tenant":
		return cd.Name, cd, store.MemoryScopeTenant, "", nil
	case "user":
		if userID == "" {
			return "", tools.ChannelDef{}, "", "", fmt.Errorf("team %q's channel %q is scope=user, and this run has no user_id", sc.Team, local)
		}
		return cd.Name, cd, store.MemoryScopeUser, userID, nil
	}
	return "", tools.ChannelDef{}, "", "", fmt.Errorf("team %q: its channel %q has scope %q", sc.Team, local, cd.Scope)
}

// publishTeamLocalChannel publishes payload into team sc's own channel
// `local`, reading the channel's definition from the team version sc names,
// in tenant (which must be the team's). It is the way in for a writer that is
// not a walk — a schedule or webhook delivering into a team — and goes
// through the channel writer like every other write, so a hold applies.
// userID keys a user-scoped channel and is the message's attribution.
func (s *Server) publishTeamLocalChannel(ctx context.Context, tenant string, sc store.TeamScope, local, userID string, payload json.RawMessage) (channels.WriteResult, error) {
	if s.systemPublisher == nil {
		return channels.WriteResult{}, fmt.Errorf("no system publisher wired")
	}
	_, def, err := s.teamVersion(ctx, tenant, sc)
	if err != nil {
		return channels.WriteResult{}, err
	}
	name, cd, scope, scopeID, err := s.teamLocalChannelTarget(def, sc, tenant, local, userID)
	if err != nil {
		return channels.WriteResult{}, err
	}
	return s.writeChannel(store.WithTeamScope(ctx, sc), channels.WriteRequest{
		Channel: name, TenantID: tenant, Scope: scope, ScopeID: scopeID,
		Payload: payload, PublishedBy: userID, MaxMessages: cd.MaxMessages,
		ExpiresAt: ttlExpiry(cd.DefaultTTL),
	})
}

// teamChannelWriteDef is ChannelWriteDef for a team's own channel: whether it
// holds, decided from the version the WRITER is acting under — the team scope
// on its ctx, which must be this team's in this tenant.
//
// It FAILS CLOSED. The prefix is reserved, so there is no legitimate write to
// a "_team/" name that is not a team's own channel, and a lenient answer
// would publish visible messages into a channel whose definition says hold.
// A write is refused when ctx carries no scope for this team and tenant (every
// writer that reaches such a name — a walk, a team's own agent through the
// Channel tool, publishTeamLocalChannel — carries one; the admin surfaces and
// the scheduler refuse the name as undeclared before they write), when that
// version is gone or unreadable, when it does not declare the channel, and on
// a store fault. The team's ACTIVE version is never consulted: the writer may
// be acting under another one, whose definition is the one that applies.
func (s *Server) teamChannelWriteDef(ctx context.Context, tenantID, team, local string) (channels.WriteDef, error) {
	name := store.TeamChannelName(team, local)
	sc, ok := store.TeamScopeFromContext(ctx)
	if !ok || sc.Team != team || sc.Tenant != tenantID {
		return channels.WriteDef{}, fmt.Errorf("channel %q is team %q's own, and this write is not made from inside that team in its tenant", store.TeamChannelDisplayName(name), team)
	}
	_, def, err := s.teamVersion(ctx, tenantID, sc)
	if err != nil {
		return channels.WriteDef{}, err
	}
	body, ok := def.LocalChannel(local)
	if !ok {
		return channels.WriteDef{}, fmt.Errorf("team %q (version %s) declares no channel of its own named %q", team, sc.DefID, local)
	}
	cd, err := builtin.LocalChannelDefinition(team, local, body)
	if err != nil {
		return channels.WriteDef{}, fmt.Errorf("team %q: its channel %q is unreadable: %w", team, local, err)
	}
	return channels.WriteDef{Hold: cd.Hold}, nil
}
