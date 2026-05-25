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

// rowToDescriptor renders a substrate row in the same shape the read
// path uses, joining live channel_messages stats when present.
func (s *Server) rowToDescriptor(ctx context.Context, row store.ChannelRow) connector.ChannelDescriptor {
	desc := connector.ChannelDescriptor{
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
	stats, err := s.store.ChannelStats(ctx)
	if err != nil {
		return desc
	}
	for _, st := range stats {
		if st.Channel != row.Name {
			continue
		}
		desc.MessageCount = st.MessageCount
		if !st.OldestVisibleAt.IsZero() {
			desc.OldestVisibleAt = st.OldestVisibleAt.UTC().Format(time.RFC3339)
		}
		if !st.NewestVisibleAt.IsZero() {
			desc.NewestVisibleAt = st.NewestVisibleAt.UTC().Format(time.RFC3339)
		}
		break
	}
	return desc
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
	if _, yaml := s.cfg.Channels[name]; yaml {
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
	case "global", "agent", "user":
	default:
		return connector.ChannelDescriptor{}, fmt.Errorf("create channel: scope must be one of global|agent|user, got %q", scope)
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
	return s.rowToDescriptor(ctx, row), nil
}

// UpdateChannel patches mutable fields on a runtime channel. yaml-
// declared channels refuse with ErrChannelYamlImmutable.
func (s *Server) UpdateChannel(ctx context.Context, name string, req connector.ChannelUpdateRequest) (connector.ChannelDescriptor, error) {
	name = strings.TrimSpace(name)
	if _, yaml := s.cfg.Channels[name]; yaml {
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
	if err := s.store.ChannelsUpdate(ctx, name, patch); err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return connector.ChannelDescriptor{}, fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
		}
		return connector.ChannelDescriptor{}, fmt.Errorf("update channel: %w", err)
	}

	// Re-read so the descriptor reflects the post-patch state.
	rows, err := s.store.ChannelsList(ctx)
	if err != nil {
		return connector.ChannelDescriptor{}, fmt.Errorf("update channel re-read: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return s.rowToDescriptor(ctx, r), nil
		}
	}
	// Shouldn't happen — successful update implies a row exists. Keep
	// a defensive error path so future contract drift surfaces loudly.
	return connector.ChannelDescriptor{}, fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
}

// DeleteChannel removes a runtime channel + cascades persisted
// messages + cursors. yaml-declared channels refuse with
// ErrChannelYamlImmutable.
func (s *Server) DeleteChannel(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if _, yaml := s.cfg.Channels[name]; yaml {
		return fmt.Errorf("%w: %q", connector.ErrChannelYamlImmutable, name)
	}
	if !validChannelName(name) {
		return fmt.Errorf("delete channel: name must match [A-Za-z0-9_-]{1,128}")
	}
	if err := s.store.ChannelsDelete(ctx, name); err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			return fmt.Errorf("%w: %q", connector.ErrChannelNotFound, name)
		}
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}
