package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
