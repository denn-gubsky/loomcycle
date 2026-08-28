// connector_impl_channels_crud.go — Connector method bodies for the
// v0.11.5 channel admin CRUD (Create / Update / Delete) on
// runtime-substrate channels. yaml-declared channels are immutable
// from this surface; mutations against a yaml name return
// ErrChannelYamlImmutable so the operator edits the yaml + restarts
// instead of getting silent drift.
package http

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
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
		Source:      "runtime",
	}
}

// attachStats folds one ChannelStat into a descriptor in place. Cheap;
// avoids the N+1 aggregation query the v0.11.5 first cut had.
func attachStats(desc *connector.ChannelDescriptor, st store.ChannelStats) {
	desc.MessageCount = st.MessageCount
	if !st.OldestVisibleAt.IsZero() {
		desc.OldestVisibleAt = st.OldestVisibleAt.UTC().Format(time.RFC3339)
	}
	if !st.NewestVisibleAt.IsZero() {
		desc.NewestVisibleAt = st.NewestVisibleAt.UTC().Format(time.RFC3339)
	}
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
	if !validChannelName(name) {
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: name must match [A-Za-z0-9_-]{1,128}")
	}

	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = "global"
	}
	switch scope {
	case "global", "agent", "user", "tenant":
	default:
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: scope must be one of global|tenant|user|agent, got %q", scope)
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

	semantic := strings.TrimSpace(req.Semantic)
	if semantic == "" {
		semantic = "queue"
	}
	switch semantic {
	case "queue", "topic":
	default:
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: semantic must be one of queue|topic, got %q", semantic)
	}
	if req.DefaultTTL < 0 || req.MaxMessages < 0 {
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: default_ttl and max_messages must be >= 0")
	}

	row := store.ChannelRow{
		Name:        name,
		TenantID:    tenantFromCtx(ctx), // RFC N: authoritative principal tenant
		Description: req.Description,
		Scope:       scope,
		Semantic:    semantic,
		DefaultTTL:  req.DefaultTTL,
		MaxMessages: req.MaxMessages,
		Publisher:   req.Publisher,
		Period:      req.Period,
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
		for _, st := range stats {
			if st.Channel == name {
				attachStats(&desc, st)
				break
			}
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
	if yamlCh, isYaml := s.cfg().Channels[name]; isYaml {
		declaredScope = yamlCh.Scope
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
			if rows[i].Name == name && (all || rows[i].TenantID == tenantID) {
				found = true
				declaredScope = rows[i].Scope
				break
			}
		}
		if !found {
			return connector.ChannelPurgeResult{}, fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
		}
	}
	msgTenant := store.ChannelScopeTenant(tenantFromCtx(ctx), store.MemoryScope(declaredScope))
	n, err := s.store.ChannelPurge(ctx, msgTenant, name)
	if err != nil {
		return connector.ChannelPurgeResult{}, fmt.Errorf("purge channel: %w", err)
	}
	return connector.ChannelPurgeResult{Name: name, Purged: n}, nil
}
