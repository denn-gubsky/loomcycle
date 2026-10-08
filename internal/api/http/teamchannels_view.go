package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// teamchannels_view.go — a team's own channels, for the person who runs it.
//
// A team's own channel n of team T is stored as "_team/T/n", and every
// surface that takes a channel name refuses that name as not declared: it is
// what keeps one team from reaching another's channel by spelling it. The
// cost was that the team's operator could not see what it wrote, or how much
// was queued. These methods are the way in from outside, and they never take
// the stored name: a channel is addressed by its team and its local name, and
// the team by the caller's tenant.
//
// They read. Nothing here publishes, acks or purges, and a peek moves no
// cursor, so nothing here can take a message from the team.

// teamChannelDecl is one of a team's own channels as its declaring version
// defines it.
type teamChannelDecl struct {
	local      string
	def        tools.ChannelDef // Name is the stored name
	declaredIn string           // connector.TeamChannelDeclared*
	row        store.TeamDefRow
}

// teamChannelTenant is the tenant whose team a caller reads. A caller who
// sees every tenant (an admin, the legacy token, open mode) may name one;
// anyone else reads their own, and naming another finds nothing.
func teamChannelTenant(ctx context.Context, named string) (string, bool) {
	own, all := tenantScopeFromCtx(ctx)
	if all {
		if named != "" {
			return named, true
		}
		return tenantFromCtx(ctx), true
	}
	if named != "" && named != own {
		return "", false
	}
	return own, true
}

// teamOwnChannels returns the channels team declares for itself in the
// caller's tenant, by local name, each as defined by the version that speaks
// for it: the active version, else the newest stored one that is not retired,
// else the newest retired one. A channel's messages belong to the team and
// outlive the version that declared it, so a channel only an old version
// declares is still the team's.
//
// ErrTeamNotFound when the tenant holds no version of the team at all.
func (s *Server) teamOwnChannels(ctx context.Context, team, namedTenant string) (string, map[string]teamChannelDecl, error) {
	if s.store == nil {
		return "", nil, fmt.Errorf("a team's channels need a store to be read from")
	}
	tenant, ok := teamChannelTenant(ctx, namedTenant)
	if !ok || team == "" {
		return "", nil, connector.ErrTeamNotFound
	}
	all, err := s.store.TeamDefListByName(ctx, team)
	if err != nil {
		return "", nil, fmt.Errorf("team %q: list its versions: %w", team, err)
	}
	rows := all[:0:0]
	for _, row := range all {
		if row.TenantID == tenant {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return "", nil, connector.ErrTeamNotFound
	}
	activeID := ""
	if active, err := s.store.TeamDefGetActive(ctx, tenant, team); err == nil {
		activeID = active.DefID
	} else if !isNotFound(err) {
		return "", nil, fmt.Errorf("team %q: read its active version: %w", team, err)
	}
	// Active first, then live versions newest first, then retired ones.
	rank := func(r store.TeamDefRow) int {
		switch {
		case r.DefID == activeID:
			return 0
		case !r.Retired:
			return 1
		}
		return 2
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if ri, rj := rank(rows[i]), rank(rows[j]); ri != rj {
			return ri < rj
		}
		return rows[i].Version > rows[j].Version
	})
	decls := map[string]teamChannelDecl{}
	for _, row := range rows {
		def, err := teamgraph.Parse(row.Definition)
		if err != nil || def.Local == nil {
			continue // a version that cannot be read declares nothing here
		}
		declaredIn := [...]string{connector.TeamChannelDeclaredActive, connector.TeamChannelDeclaredInactive, connector.TeamChannelDeclaredRetired}[rank(row)]
		for local, body := range def.Local.Channels {
			if _, seen := decls[local]; seen {
				continue
			}
			cd, err := builtin.LocalChannelDefinition(team, local, body)
			if err != nil {
				continue // a later version may still declare it readably
			}
			decls[local] = teamChannelDecl{local: local, def: cd, declaredIn: declaredIn, row: row}
		}
	}
	return tenant, decls, nil
}

// ListTeamChannels implements connector.Connector.
func (s *Server) ListTeamChannels(ctx context.Context, req connector.TeamChannelsRequest) (connector.TeamChannelsResponse, error) {
	tenant, decls, err := s.teamOwnChannels(ctx, req.Team, req.Tenant)
	if err != nil {
		return connector.TeamChannelsResponse{}, err
	}
	out := connector.TeamChannelsResponse{Team: req.Team, Channels: make([]connector.TeamChannelDescriptor, 0, len(decls))}
	if len(decls) == 0 {
		return out, nil
	}
	stats, err := s.store.ChannelStats(ctx)
	if err != nil {
		return connector.TeamChannelsResponse{}, fmt.Errorf("team %q: channel stats: %w", req.Team, err)
	}
	ix := newStatsIndex(stats)
	for _, d := range decls {
		desc := connector.TeamChannelDescriptor{
			Name: d.local, Scope: d.def.Scope, Semantic: d.def.Semantic, Hold: d.def.Hold,
			DefaultTTL: d.def.DefaultTTL, MaxMessages: d.def.MaxMessages,
			DeclaredIn: d.declaredIn, DefID: d.row.DefID, Version: d.row.Version,
		}
		// The team's own keyspace only: a team's channels are its tenant's.
		if st, ok := ix.forKeyspace(d.def.Name, tenant); ok {
			desc.MessageCount, desc.HeldCount, desc.AwaitingHooksCount = st.MessageCount, st.Held, st.AwaitingHooks
			if !st.OldestVisibleAt.IsZero() {
				desc.OldestVisibleAt = st.OldestVisibleAt.UTC().Format(time.RFC3339Nano)
			}
			if !st.NewestVisibleAt.IsZero() {
				desc.NewestVisibleAt = st.NewestVisibleAt.UTC().Format(time.RFC3339Nano)
			}
		}
		out.Channels = append(out.Channels, desc)
	}
	sort.Slice(out.Channels, func(i, j int) bool { return out.Channels[i].Name < out.Channels[j].Name })
	return out, nil
}

// teamChannelKeyspace is the scope and scope id a caller reads team channel d
// at. A tenant-scoped channel has one keyspace. A user-scoped one is read at
// the caller's own user; another user's is read only by a caller who may read
// any user's channel, and is otherwise reported as not declared — the same
// answer an unknown channel gets, so the route is no oracle for user ids.
func teamChannelKeyspace(ctx context.Context, d teamChannelDecl, userID string) (store.MemoryScope, string, error) {
	if d.def.Scope != "user" {
		return store.MemoryScopeTenant, "", nil
	}
	if userID != "" && !validIdent(userID) {
		return "", "", &connector.TeamChannelError{Kind: connector.ErrTeamChannelUserRequired, Msg: "user_id must match [A-Za-z0-9_-]{1,128}"}
	}
	if userID == "" {
		if p, ok := auth.PrincipalFromContext(ctx); ok {
			userID = p.Subject
		}
	}
	if userID == "" {
		return "", "", &connector.TeamChannelError{Kind: connector.ErrTeamChannelUserRequired,
			Msg: fmt.Sprintf("channel %q is scope=user; pass the user_id whose messages to read", d.local)}
	}
	if !requirePrincipalOwnsPathUser(ctx, userID) {
		return "", "", connector.ErrTeamChannelNotDeclared
	}
	return store.MemoryScopeUser, userID, nil
}

// PeekTeamChannel implements connector.Connector.
func (s *Server) PeekTeamChannel(ctx context.Context, req connector.TeamChannelPeekRequest) (connector.TeamChannelPeekResult, error) {
	tenant, decls, err := s.teamOwnChannels(ctx, req.Team, req.Tenant)
	if err != nil {
		return connector.TeamChannelPeekResult{}, err
	}
	// One refusal for a name the team does not declare and for a keyspace the
	// caller may not read, so the two cannot be told apart.
	notDeclared := &connector.TeamChannelError{Kind: connector.ErrTeamChannelNotDeclared,
		Msg: fmt.Sprintf("team %q declares no channel of its own named %q", req.Team, req.Name)}
	d, ok := decls[req.Name]
	if !ok {
		return connector.TeamChannelPeekResult{}, notDeclared
	}
	scope, scopeID, err := teamChannelKeyspace(ctx, d, req.UserID)
	if err != nil {
		if errors.Is(err, connector.ErrTeamChannelNotDeclared) {
			return connector.TeamChannelPeekResult{}, notDeclared
		}
		return connector.TeamChannelPeekResult{}, err
	}
	limit := req.MaxMessages
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	msgs, err := s.store.ChannelPeek(ctx, tenant, d.def.Name, scope, scopeID, req.FromCursor, limit)
	if err != nil {
		return connector.TeamChannelPeekResult{}, fmt.Errorf("peek: %w", err)
	}
	out := connector.TeamChannelPeekResult{
		Team: req.Team, Name: d.local, Scope: d.def.Scope, DeclaredIn: d.declaredIn,
		Messages: make([]connector.ChannelMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, connector.ChannelMessage{
			ID: m.ID, Value: m.Payload, PublishedAt: m.PublishedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out, nil
}

// writeTeamChannelError answers a failed team-channel read. Both not-founds
// are 404s with their own code, so a client can tell "no such team" from "the
// team has no such channel" without parsing the text.
func writeTeamChannelError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, connector.ErrTeamNotFound):
		writeJSONError(w, http.StatusNotFound, "team_not_found", "no such team")
	case errors.Is(err, connector.ErrTeamChannelNotDeclared):
		writeJSONError(w, http.StatusNotFound, "team_channel_not_declared", err.Error())
	case errors.Is(err, connector.ErrTeamChannelUserRequired):
		writeJSONError(w, http.StatusBadRequest, "user_id_required", err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, "team_channel_op_failed", err.Error())
	}
}

// teamChannelScopeOK refuses a caller whose token lacks the channel scope the
// operation needs, and reports whether the request may go on.
//
// The routes are gated like the team, which a member's token reaches whatever
// it was granted. What they do is a channel read or a channel write, and the
// channel surface asks channels:read / channels:publish for those: without
// this a token granted only runs:read would read a team's messages here while
// the channel routes refuse it. A tenant operator's scope implies both; no
// principal (no authentication configured) is the operator.
func teamChannelScopeOK(w http.ResponseWriter, r *http.Request, need string) bool {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok || auth.HasScope(p.Scopes, need) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer scope="`+need+`"`)
	writeJSONError(w, http.StatusForbidden, "insufficient_scope", "insufficient scope: a team's own channels require "+need)
	return false
}

// handleTeamChannels serves GET /v1/_teamdef/{team}/channels.
func (s *Server) handleTeamChannels(w http.ResponseWriter, r *http.Request) {
	if !teamChannelScopeOK(w, r, auth.ScopeChannelRead) {
		return
	}
	out, err := s.ListTeamChannels(r.Context(), connector.TeamChannelsRequest{
		Team: r.PathValue("team"), Tenant: r.URL.Query().Get("tenant"),
	})
	if err != nil {
		writeTeamChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleTeamChannelPeek serves GET /v1/_teamdef/{team}/channels/{name}/peek.
// Query: max_messages, from_cursor, user_id (a user-scoped channel), tenant.
func (s *Server) handleTeamChannelPeek(w http.ResponseWriter, r *http.Request) {
	if !teamChannelScopeOK(w, r, auth.ScopeChannelRead) {
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("max_messages"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "max_messages must be a non-negative integer")
			return
		}
		limit = n
	}
	out, err := s.PeekTeamChannel(r.Context(), connector.TeamChannelPeekRequest{
		Team: r.PathValue("team"), Name: r.PathValue("name"), Tenant: q.Get("tenant"),
		UserID: q.Get("user_id"), FromCursor: q.Get("from_cursor"), MaxMessages: limit,
	})
	if err != nil {
		writeTeamChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
