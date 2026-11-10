package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// channel_defs, before the messages they govern: the hook worker claims a
// restored message awaiting hooks as soon as it lands, and resolves its
// channel then. ChannelsCreate is a plain insert that keeps created_at;
// a live definition already on (tenant_id, name) is left alone, as the
// other def sections leave a live row alone.
func restoreChannelDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionChannelDefs]; ok {
		var sec ChannelDefsSection
		if err := decodeWithMigration(migrations.SectionChannelDefs, rawSection, &sec); err != nil {
			return err
		}
		validate := existingSectionValidator(opts, migrations.SectionChannelDefs, len(sec.Entries), result)
		for _, e := range sec.Entries {
			if validate != nil {
				entry, err := json.Marshal(e)
				if err != nil {
					result.Warnings = append(result.Warnings, fmt.Sprintf("channel_def %s: not restored: %v", qualifiedName(e.TenantID, e.Name), err))
					continue
				}
				if refusedByValidation(validate, "channel_def "+qualifiedName(e.TenantID, e.Name), entry, &result.ChannelDefsRefused, result) {
					continue
				}
			}
			err := s.ChannelsCreate(ctx, store.ChannelRow{
				Name:        e.Name,
				TenantID:    e.TenantID,
				Description: e.Description,
				Scope:       e.Scope,
				Semantic:    e.Semantic,
				DefaultTTL:  e.DefaultTTL,
				MaxMessages: e.MaxMessages,
				Publisher:   e.Publisher,
				Period:      e.Period,
				Hold:        e.Hold,
				Hooks:       e.Hooks,
				CreatedAt:   e.CreatedAt,
			})
			var conflict *store.ErrConflict
			switch {
			case err == nil:
				result.ChannelDefsRestored++
				scan.addRefs("channel_def "+qualifiedName(e.TenantID, e.Name), e.TenantID, defRefs("hooks", e.Hooks))
			case errors.As(err, &conflict):
				// Already defined here: the live definition stands.
			default:
				result.Warnings = append(result.Warnings, fmt.Sprintf("channel_def %s/%s: %v", e.TenantID, e.Name, err))
			}
		}
	}
	return nil
}

// channels (config + messages + cursors). Config is operator-
// yaml-side; restore doesn't write it back into a table — the
// operator on the restoring host reconciles against their own
// yaml. Messages + cursors get raw inserts.
func restoreChannelsSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionChannels]; ok {
		var sec ChannelsSection
		if err := decodeWithMigration(migrations.SectionChannels, rawSection, &sec); err != nil {
			return err
		}
		if len(sec.Config) > 0 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("channels.config: %d declared channels in snapshot; reconcile against operator yaml on the restoring host", len(sec.Config)))
		}
		teamChannels := newTeamChannelGate(s)
		for _, m := range sec.Messages {
			if !teamChannels.admit(ctx, m.TenantID, m.Channel) {
				continue
			}
			var expires, visible, requested time.Time
			if m.ExpiresAt != nil {
				expires = *m.ExpiresAt
			}
			if m.VisibleAt != nil {
				visible = *m.VisibleAt
			}
			if m.RequestedVisibleAt != nil {
				requested = *m.RequestedVisibleAt
			}
			inserted, err := s.SnapshotRestoreChannelMessage(ctx, store.ChannelMessage{
				ID:                 m.ID,
				Channel:            m.Channel,
				Scope:              store.MemoryScope(m.Scope),
				ScopeID:            m.ScopeID,
				Payload:            m.Payload,
				PublishedAt:        m.PublishedAt,
				ExpiresAt:          expires,
				VisibleAt:          visible,
				PublishedByUserID:  m.PublishedByUserID,
				TenantID:           m.TenantID,
				Origin:             m.Origin,
				HookTenant:         m.HookTenant,
				RequestedVisibleAt: requested,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("channel_message %s: %v", m.ID, err))
				continue
			}
			if inserted {
				result.ChannelMessagesRestored++
			}
		}
		for _, c := range sec.Cursors {
			if !teamChannels.admit(ctx, c.TenantID, c.Channel) {
				continue
			}
			inserted, err := s.SnapshotRestoreChannelCursor(ctx, store.ChannelCursorEntry{
				Channel:   c.Channel,
				TenantID:  c.TenantID,
				Scope:     store.MemoryScope(c.Scope),
				ScopeID:   c.ScopeID,
				Cursor:    c.Cursor,
				UpdatedAt: c.UpdatedAt,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("channel_cursor %s/%s/%s: %v", c.Channel, c.Scope, c.ScopeID, err))
				continue
			}
			if inserted {
				result.ChannelCursorsRestored++
			}
		}
		result.TeamChannelRowsSkipped = teamChannels.report(result)
	}
	return nil
}
