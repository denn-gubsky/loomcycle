// connector_impl_channels_crud.go — Connector method bodies for the
// v0.11.5 channel admin CRUD (Create / Update / Delete) on
// runtime-substrate channels. yaml-declared channels are immutable
// from this surface; mutations against a yaml name return
// ErrChannelYamlImmutable so the operator edits the yaml + restarts
// instead of getting silent drift.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// validChannelName is a strict ident shape — same allow-set as
// scope-id / user-id elsewhere on the admin surface. Channel names
// are surfaced in URLs and operator yaml; keep them URL-safe.
func validChannelName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// rowToBareDescriptor renders a substrate row WITHOUT joining stats.
// Callers that need stats either know they're zero (fresh inserts) or
// supply them explicitly via attachStats below.
func rowToBareDescriptor(row store.ChannelRow) connector.ChannelDescriptor {
	return connector.ChannelDescriptor{
		Name:        row.Name,
		Description: row.Description,
		Scope:       row.Scope,
		Semantic:    row.Semantic,
		Publisher:   row.Publisher,
		Period:      row.Period,
		DefaultTTL:  row.DefaultTTL,
		MaxMessages: row.MaxMessages,
		Hold:        row.Hold,
		Hooks:       decodeChannelHooks(row.Hooks),
		Source:      "runtime",
	}
}

// decodeChannelHooks reads a runtime channel's stored hooks. They were
// validated before they were written, so a row that does not decode is
// reported as having none here; the writer and the worker, which must not
// guess, refuse on it instead.
func decodeChannelHooks(raw json.RawMessage) hooks.EventHooks {
	if store.NoChannelHooks(raw) {
		return nil
	}
	var h hooks.EventHooks
	if json.Unmarshal(raw, &h) != nil {
		return nil
	}
	return h
}

// checkChannelHooks validates hooks a runtime channel is being given and
// returns them as stored (nil for none). They are stored whether or not the
// server runs channel hooks — off, they are skipped until it does — and every
// reference must resolve, in the channel's tenant or the shared one, to a
// HookDef that answers channel_publish: a gate named on a channel must exist
// when it is named.
func (s *Server) checkChannelHooks(ctx context.Context, name, publisher, tenant string, h hooks.EventHooks) (json.RawMessage, error) {
	if len(h) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(h)
	if err != nil || store.NoChannelHooks(raw) {
		return nil, err
	}
	if err := config.ValidateChannelHooks(name, publisher, h); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrChannelHooksInvalid, err)
	}
	src := hooks.Source{Owner: hooks.ChannelOwner(name), Tenant: tenant}
	if err := hooks.ResolveChannel(ctx, src, h, builtin.HookDefLookup(s.store), hooks.NewSet()); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrChannelHooksInvalid, err)
	}
	return raw, nil
}

// attachStats folds one ChannelStat into a descriptor in place. Cheap;
// avoids the N+1 aggregation query the v0.11.5 first cut had.
func attachStats(desc *connector.ChannelDescriptor, st store.ChannelStats) {
	desc.MessageCount = st.MessageCount
	desc.HeldCount, desc.AwaitingHooksCount = st.Held, st.AwaitingHooks
	desc.OldestVisibleAt = formatVisibleAt(st.OldestVisibleAt)
	desc.NewestVisibleAt = formatVisibleAt(st.NewestVisibleAt)
}

// CreateChannel inserts a new runtime-substrate channel. Refuses with
// ErrChannelYamlImmutable when the name matches an operator-yaml
// channel (yaml is the floor — no shadowing). Refuses with
// ErrChannelAlreadyExists when the runtime substrate already has the
// name.
func (s *Server) CreateChannel(ctx context.Context, req connector.ChannelCreateRequest) (connector.ChannelDescriptor, error) {
	name := strings.TrimSpace(req.Name)
	// yaml-precedence first: if the operator already declared this name
	// in yaml, the most actionable error is "edit the yaml" regardless
	// of how exotic the name shape is (yaml allows slashes etc. that
	// the runtime allow-set forbids).
	if _, yaml := s.cfg().Channels[name]; yaml {
		return connector.ChannelDescriptor{}, fmt.Errorf("%w: %q", connector.ErrChannelYamlImmutable, name)
	}
	// Stated on its own, not left to the grammar below (which also refuses a
	// "/"): the reservation is what keeps a team's own channels unreachable
	// from outside it, and must hold if the grammar is ever widened.
	if store.IsTeamChannelName(name) {
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: the %q prefix is reserved for a team's own channels, which a team declares in its definition under local.channels", store.TeamChannelPrefix)
	}
	if !validChannelName(name) {
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: name must match [A-Za-z0-9_-]{1,128}")
	}

	scope, semantic, err := connector.NormalizeChannelFields(strings.TrimSpace(req.Scope), strings.TrimSpace(req.Semantic), req.DefaultTTL, req.MaxMessages)
	if err != nil {
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: %w", err)
	}
	// A global channel is a single cross-tenant keyspace (tenant_id="", see
	// store.ChannelScopeTenant): its messages are shared across every
	// tenant. Only an operator/admin may create one at runtime; a tenant
	// operator is confined to its own tenant and must use tenant|user|agent
	// scope (all partitioned by its tenant). Open mode (no principal) is
	// single-tenant and unrestricted.
	if scope == "global" {
		if p, ok := auth.PrincipalFromContext(ctx); ok && !auth.HasScope(p.Scopes, auth.ScopeAdmin) {
			return connector.ChannelDescriptor{}, fmt.Errorf("create channel: scope=global requires operator (admin) scope; a tenant operator may create tenant|user|agent channels")
		}
	}

	hooksRaw, err := s.checkChannelHooks(ctx, name, req.Publisher, tenantFromCtx(ctx), req.Hooks)
	if err != nil {
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: %w", err)
	}
	row := store.ChannelRow{
		Name:        name,
		TenantID:    tenantFromCtx(ctx), // RFC N: authoritative principal tenant
		Hooks:       hooksRaw,
		Description: req.Description,
		Scope:       scope,
		Semantic:    semantic,
		DefaultTTL:  req.DefaultTTL,
		MaxMessages: req.MaxMessages,
		Publisher:   req.Publisher,
		Period:      req.Period,
		Hold:        req.Hold,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.store.ChannelsCreate(ctx, row); err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			return connector.ChannelDescriptor{}, fmt.Errorf("%w: %q", connector.ErrChannelAlreadyExists, name)
		}
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: %w", err)
	}
	// A freshly-inserted channel has zero messages by definition —
	// skip the ChannelStats round-trip.
	return rowToBareDescriptor(row), nil
}

// UpdateChannel patches mutable fields on a runtime channel. yaml-
// declared channels refuse with ErrChannelYamlImmutable.
func (s *Server) UpdateChannel(ctx context.Context, name string, req connector.ChannelUpdateRequest) (connector.ChannelDescriptor, error) {
	name = strings.TrimSpace(name)
	if _, yaml := s.cfg().Channels[name]; yaml {
		return connector.ChannelDescriptor{}, fmt.Errorf("%w: %q", connector.ErrChannelYamlImmutable, name)
	}
	if !validChannelName(name) {
		return connector.ChannelDescriptor{}, fmt.Errorf("update channel: name must match [A-Za-z0-9_-]{1,128}")
	}
	if req.Semantic != nil {
		switch *req.Semantic {
		case "queue", "topic":
		default:
			return connector.ChannelDescriptor{}, fmt.Errorf("update channel: semantic must be one of queue|topic, got %q", *req.Semantic)
		}
	}
	if req.DefaultTTL != nil && *req.DefaultTTL < 0 {
		return connector.ChannelDescriptor{}, fmt.Errorf("update channel: default_ttl must be >= 0")
	}
	if req.MaxMessages != nil && *req.MaxMessages < 0 {
		return connector.ChannelDescriptor{}, fmt.Errorf("update channel: max_messages must be >= 0")
	}

	patch := store.ChannelPatch{
		Description: req.Description,
		DefaultTTL:  req.DefaultTTL,
		MaxMessages: req.MaxMessages,
		Semantic:    req.Semantic,
		Hold:        req.Hold,
	}
	if req.Hooks != nil {
		// A runtime channel is written only by loomcycle when its publisher
		// is "system"; the stored row says so.
		publisher := ""
		if row, err := s.store.ChannelGet(ctx, tenantFromCtx(ctx), name); err == nil {
			publisher = row.Publisher
		}
		raw, err := s.checkChannelHooks(ctx, name, publisher, tenantFromCtx(ctx), *req.Hooks)
		if err != nil {
			return connector.ChannelDescriptor{}, fmt.Errorf("update channel: %w", err)
		}
		patch.Hooks = &raw
	}
	if err := s.store.ChannelsUpdate(ctx, tenantFromCtx(ctx), name, patch); err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return connector.ChannelDescriptor{}, fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
		}
		return connector.ChannelDescriptor{}, fmt.Errorf("update channel: %w", err)
	}

	// Re-read so the descriptor reflects the post-patch state. We
	// also fetch ChannelStats ONCE here so the response carries live
	// message_count + visible_at bounds without an additional query.
	rows, err := s.store.ChannelsList(ctx)
	if err != nil {
		return connector.ChannelDescriptor{}, fmt.Errorf("update channel re-read: %w", err)
	}
	// ChannelsList returns every tenant's rows; tenant operators see only
	// their own channels, admin sees all. Scope the re-read so a same-named
	// channel in another tenant can't be picked up.
	tenantID, all := s.principalTenantScope(ctx, "")
	var match *store.ChannelRow
	for i := range rows {
		if rows[i].Name == name && (all || rows[i].TenantID == tenantID) {
			match = &rows[i]
			break
		}
	}
	if match == nil {
		// Shouldn't happen — successful update implies a row exists.
		// Keep a defensive error path so future contract drift surfaces loudly.
		return connector.ChannelDescriptor{}, fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
	}
	desc := rowToBareDescriptor(*match)
	if stats, err := s.store.ChannelStats(ctx); err == nil {
		if st, ok := newStatsIndex(stats).runtimeStats(*match, all); ok {
			attachStats(&desc, st)
		}
	}
	return desc, nil
}

// DeleteChannel removes a runtime channel + cascades persisted
// messages + cursors. yaml-declared channels refuse with
// ErrChannelYamlImmutable.
func (s *Server) DeleteChannel(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if _, yaml := s.cfg().Channels[name]; yaml {
		return fmt.Errorf("%w: %q", connector.ErrChannelYamlImmutable, name)
	}
	if !validChannelName(name) {
		return fmt.Errorf("delete channel: name must match [A-Za-z0-9_-]{1,128}")
	}
	if err := s.store.ChannelsDelete(ctx, tenantFromCtx(ctx), name); err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
		}
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}

// PurgeChannel clears all buffered messages on a channel without
// removing its definition or subscriber cursors. Unlike Create/Update/
// Delete it is ALLOWED on yaml-declared channels: purging is not a
// definition mutation, and draining a yaml channel that filled with
// test traffic was the F20 pain that previously required a raw DB
// delete. Returns ErrChannelNotFound when the name is neither
// yaml-declared nor present in the runtime substrate.
func (s *Server) PurgeChannel(ctx context.Context, name string) (connector.ChannelPurgeResult, error) {
	name = strings.TrimSpace(name)
	// The channel's declared SCOPE determines which tenant keyspace its
	// messages live in (global => "", every other scope => the caller's
	// tenant; see store.ChannelScopeTenant). Purge must target that
	// keyspace, not blindly the caller's tenant — otherwise a global
	// channel's messages (at "") are never drained.
	declaredScope := ""
	purger := tenantFromCtx(ctx)
	// operatorChannel: the purged channel is the operator's, whose global
	// layers every tenant without a channel of that name resolves to. A yaml
	// channel is (yaml wins a name collision); a runtime one only in the
	// operator's own tenant. ownRows are the tenants with a runtime channel of
	// that name — their layers belong to it, not to the operator's.
	operatorChannel := false
	ownRows := map[string]bool{}
	if yamlCh, isYaml := s.cfg().Channels[name]; isYaml {
		declaredScope = yamlCh.Scope
		operatorChannel = true
	} else {
		// Only the runtime plane obeys the strict name shape — yaml
		// channels may use exotic names (slashes etc.) the runtime
		// allow-set forbids, and we must still let those be purged.
		if !validChannelName(name) {
			return connector.ChannelPurgeResult{}, fmt.Errorf("purge channel: name must match [A-Za-z0-9_-]{1,128}")
		}
		rows, err := s.store.ChannelsList(ctx)
		if err != nil {
			return connector.ChannelPurgeResult{}, fmt.Errorf("purge channel existence check: %w", err)
		}
		// ChannelsList returns every tenant's rows; tenant operators see only
		// their own channels, admin sees all — so a cross-tenant name can't be
		// treated as "exists" here.
		tenantID, all := s.principalTenantScope(ctx, "")
		found := false
		for i := range rows {
			if rows[i].Name != name {
				continue
			}
			ownRows[rows[i].TenantID] = true
			if rows[i].TenantID == purger {
				declaredScope = rows[i].Scope
				operatorChannel = purger == store.ChannelOperatorTenant
			}
			if all || rows[i].TenantID == tenantID {
				found = true
			}
		}
		if !found {
			return connector.ChannelPurgeResult{}, fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
		}
	}
	// A purge empties the purger's own keyspace. An admin's purge of the
	// operator's global channel also empties the global layer of every tenant
	// that resolves to it — never a tenant's own same-named channel.
	layers := []string{purger}
	var scope store.MemoryScope // every scope of the purger's own channel
	if _, all := s.principalTenantScope(ctx, ""); all && operatorChannel && store.MemoryScope(declaredScope) == store.MemoryScopeGlobal {
		scope = store.MemoryScopeGlobal // other tenants' layers, never their own channels
		layers = []string{store.ChannelOperatorTenant}
		stats, err := s.store.ChannelStats(ctx)
		if err != nil {
			return connector.ChannelPurgeResult{}, fmt.Errorf("purge channel: %w", err)
		}
		for _, st := range stats {
			if st.Channel == name && st.TenantID != store.ChannelOperatorTenant && !ownRows[st.TenantID] {
				layers = append(layers, st.TenantID)
			}
		}
	}
	total := 0
	for _, t := range layers {
		n, err := s.store.ChannelPurge(ctx, t, name, scope)
		if err != nil {
			return connector.ChannelPurgeResult{}, fmt.Errorf("purge channel: %w", err)
		}
		total += n
	}
	return connector.ChannelPurgeResult{Name: name, Purged: total}, nil
}
