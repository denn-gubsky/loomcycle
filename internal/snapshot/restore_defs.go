package snapshot

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// agent_defs
func restoreAgentDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionAgentDefs]; ok {
		var sec AgentDefsSection
		if err := decodeWithMigration(migrations.SectionAgentDefs, rawSection, &sec); err != nil {
			return err
		}
		validate := existingSectionValidator(opts, migrations.SectionAgentDefs, len(sec.Entries), result)
		for _, e := range sec.Entries {
			where := fmt.Sprintf("agent_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
			if refusedByValidation(validate, where, e.Definition, &result.AgentDefsRefused, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreAgentDef(ctx, store.AgentDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
				OperatorAuthored:       e.OperatorAuthored,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("agent_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.AgentDefsRestored++
				if !e.Retired {
					scan.addAgentRefs("agent_def "+qualifiedName(e.TenantID, e.Name), e.TenantID, e.Name, defRefs("", e.Definition))
				}
			}
		}
	}
	return nil
}

// agent_def_active (after agent_defs for FK)
func restoreAgentDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionAgentDefActive]; ok {
		var sec AgentDefActiveSection
		if err := decodeWithMigration(migrations.SectionAgentDefActive, rawSection, &sec); err != nil {
			return err
		}
		for _, e := range sec.Entries {
			if !admitActivePointer(ctx, s, agentDefOwner, "agent_def_active", e.TenantID, e.Name, e.DefID, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreAgentDefActive(ctx, store.AgentDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("agent_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.AgentDefActiveRestored++
			}
		}
	}
	return nil
}

// skill_defs (v0.8.22) — mirror of agent_defs restore
func restoreSkillDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result := rs.s, rs.opts, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionSkillDefs]; ok {
		var sec SkillDefsSection
		if err := decodeWithMigration(migrations.SectionSkillDefs, rawSection, &sec); err != nil {
			return err
		}
		validate := existingSectionValidator(opts, migrations.SectionSkillDefs, len(sec.Entries), result)
		for _, e := range sec.Entries {
			where := fmt.Sprintf("skill_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
			if refusedByValidation(validate, where, e.Definition, &result.SkillDefsRefused, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreSkillDef(ctx, store.SkillDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("skill_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.SkillDefsRestored++
			}
		}
	}
	return nil
}

// skill_def_active (after skill_defs for FK)
func restoreSkillDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionSkillDefActive]; ok {
		var sec SkillDefActiveSection
		if err := decodeWithMigration(migrations.SectionSkillDefActive, rawSection, &sec); err != nil {
			return err
		}
		for _, e := range sec.Entries {
			if !admitActivePointer(ctx, s, skillDefOwner, "skill_def_active", e.TenantID, e.Name, e.DefID, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreSkillDefActive(ctx, store.SkillDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("skill_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.SkillDefActiveRestored++
			}
		}
	}
	return nil
}

// team_defs — mirror of skill_defs restore
func restoreTeamDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionTeamDefs]; ok {
		var sec TeamDefsSection
		if err := decodeWithMigration(migrations.SectionTeamDefs, rawSection, &sec); err != nil {
			return err
		}
		validate := existingSectionValidator(opts, migrations.SectionTeamDefs, len(sec.Entries), result)
		for _, e := range sec.Entries {
			where := fmt.Sprintf("team_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
			if refusedByValidation(validate, where, e.Definition, &result.TeamDefsRefused, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreTeamDef(ctx, store.TeamDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
				OperatorAuthored:       e.OperatorAuthored,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("team_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.TeamDefsRestored++
				if !e.Retired {
					scan.addRefs("team_def "+qualifiedName(e.TenantID, e.Name), e.TenantID,
						append(defRefs("", e.Definition), teamWebhookRefs(e.Definition)...))
				}
			}
		}
	}
	return nil
}

// team_def_active (after team_defs for FK)
func restoreTeamDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionTeamDefActive]; ok {
		var sec TeamDefActiveSection
		if err := decodeWithMigration(migrations.SectionTeamDefActive, rawSection, &sec); err != nil {
			return err
		}
		for _, e := range sec.Entries {
			if !admitActivePointer(ctx, s, teamDefOwner, "team_def_active", e.TenantID, e.Name, e.DefID, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreTeamDefActive(ctx, store.TeamDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
				Promoter:          promoterRow(e.Promoter),
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("team_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.TeamDefActiveRestored++
			}
		}
	}
	return nil
}

// hook_defs — mirror of team_defs restore
func restoreHookDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionHookDefs]; ok {
		var sec HookDefsSection
		if err := decodeWithMigration(migrations.SectionHookDefs, rawSection, &sec); err != nil {
			return err
		}
		validate := existingSectionValidator(opts, migrations.SectionHookDefs, len(sec.Entries), result)
		for _, e := range sec.Entries {
			where := fmt.Sprintf("hook_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
			if refusedByValidation(validate, where, e.Definition, &result.HookDefsRefused, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreHookDef(ctx, store.HookDefRow{
				DefID:            e.DefID,
				TenantID:         e.TenantID,
				Name:             e.Name,
				Version:          e.Version,
				ParentDefID:      e.ParentDefID,
				Definition:       e.Definition,
				Description:      e.Description,
				CreatedAt:        e.CreatedAt,
				CreatedByAgentID: e.CreatedByAgentID,
				CreatedByRunID:   e.CreatedByRunID,
				Retired:          e.Retired,
				ContentSHA256:    e.ContentSHA256,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("hook_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.HookDefsRestored++
				if !e.Retired {
					scan.addRefs("hook_def "+qualifiedName(e.TenantID, e.Name), e.TenantID, defRefs("", e.Definition))
				}
			}
		}
	}
	return nil
}

// hook_def_active (after hook_defs for FK)
func restoreHookDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionHookDefActive]; ok {
		var sec HookDefActiveSection
		if err := decodeWithMigration(migrations.SectionHookDefActive, rawSection, &sec); err != nil {
			return err
		}
		for _, e := range sec.Entries {
			if !admitActivePointer(ctx, s, hookDefOwner, "hook_def_active", e.TenantID, e.Name, e.DefID, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreHookDefActive(ctx, store.HookDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("hook_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.HookDefActiveRestored++
			}
		}
	}
	return nil
}

// mcp_server_defs (v0.9.x) — mirror of agent_defs / skill_defs restore
func restoreMCPServerDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionMCPServerDefs]; ok {
		var sec MCPServerDefsSection
		if err := decodeWithMigration(migrations.SectionMCPServerDefs, rawSection, &sec); err != nil {
			return err
		}
		// A refused def is not in the store, so the caller's post-restore
		// registry reload (which reads the store) never makes it dialable.
		validate := existingSectionValidator(opts, migrations.SectionMCPServerDefs, len(sec.Entries), result)
		for _, e := range sec.Entries {
			where := fmt.Sprintf("mcp_server_def %s v%d (def %s)", qualifiedName(e.TenantID, e.Name), e.Version, e.DefID)
			if refusedByValidation(validate, where, e.Definition, &result.MCPServerDefsRefused, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreMCPServerDef(ctx, store.MCPServerDefRow{
				DefID:                  e.DefID,
				TenantID:               e.TenantID,
				Name:                   e.Name,
				Version:                e.Version,
				ParentDefID:            e.ParentDefID,
				Definition:             e.Definition,
				Description:            e.Description,
				CreatedAt:              e.CreatedAt,
				CreatedByAgentID:       e.CreatedByAgentID,
				CreatedByRunID:         e.CreatedByRunID,
				Retired:                e.Retired,
				BootstrappedFromStatic: e.BootstrappedFromStatic,
				ContentSHA256:          e.ContentSHA256,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("mcp_server_def %s: %v", e.DefID, err))
				continue
			}
			if inserted {
				result.MCPServerDefsRestored++
				if !e.Retired {
					scan.addRefs("mcp_server_def "+qualifiedName(e.TenantID, e.Name), e.TenantID, defRefs("", e.Definition, "url", "env"))
				}
			}
		}
	}
	return nil
}

// mcp_server_def_active (after mcp_server_defs for FK)
func restoreMCPServerDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionMCPServerDefActive]; ok {
		var sec MCPServerDefActiveSection
		if err := decodeWithMigration(migrations.SectionMCPServerDefActive, rawSection, &sec); err != nil {
			return err
		}
		for _, e := range sec.Entries {
			if !admitActivePointer(ctx, s, mcpServerDefOwner, "mcp_server_def_active", e.TenantID, e.Name, e.DefID, result) {
				continue
			}
			inserted, err := s.SnapshotRestoreMCPServerDefActive(ctx, store.MCPServerDefActiveEntry{
				Name:              e.Name,
				TenantID:          e.TenantID,
				DefID:             e.DefID,
				PromotedAt:        e.PromotedAt,
				PromotedByAgentID: e.PromotedByAgentID,
			})
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("mcp_server_def_active %s: %v", e.Name, err))
				continue
			}
			if inserted {
				result.MCPServerDefActiveRestored++
			}
		}
	}
	return nil
}

// volume_defs: after the definitions that bind a volume by name and
// before any resumed run resolves one. Paths are derived here, never read
// from the envelope.
func restoreVolumeDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result := rs.s, rs.opts, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionVolumeDefs]; ok {
		var sec VolumeDefsSection
		if err := decodeWithMigration(migrations.SectionVolumeDefs, rawSection, &sec); err != nil {
			return err
		}
		restoreVolumeDefs(ctx, s, &sec, opts, result)
	}
	return nil
}

// memory_backend_defs, then document_source_defs, each with its pointers:
// before memory, since a routing def precedes the data routed through it,
// and before any resumed run makes its first Memory or Document call.
func restoreMemoryBackendDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionMemoryBackendDefs]; ok {
		var sec MemoryBackendDefsSection
		if err := decodeWithMigration(migrations.SectionMemoryBackendDefs, rawSection, &sec); err != nil {
			return err
		}
		restoreMemoryBackendDefs(ctx, s, &sec, opts, scan, result)
	}
	return nil
}

// restoreMemoryBackendDefActiveSection restores the memory_backend_def_active section (its place: restoreSectionOrder).
func restoreMemoryBackendDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionMemoryBackendDefActive]; ok {
		var sec MemoryBackendDefActiveSection
		if err := decodeWithMigration(migrations.SectionMemoryBackendDefActive, rawSection, &sec); err != nil {
			return err
		}
		restoreMemoryBackendDefActive(ctx, s, &sec, result)
	}
	return nil
}

// restoreDocSourceDefsSection restores the document_source_defs section (its place: restoreSectionOrder).
func restoreDocSourceDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionDocSourceDefs]; ok {
		var sec DocSourceDefsSection
		if err := decodeWithMigration(migrations.SectionDocSourceDefs, rawSection, &sec); err != nil {
			return err
		}
		restoreDocSourceDefs(ctx, s, &sec, opts, scan, result)
	}
	return nil
}

// restoreDocSourceDefActiveSection restores the document_source_def_active section (its place: restoreSectionOrder).
func restoreDocSourceDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionDocSourceDefActive]; ok {
		var sec DocSourceDefActiveSection
		if err := decodeWithMigration(migrations.SectionDocSourceDefActive, rawSection, &sec); err != nil {
			return err
		}
		restoreDocSourceDefActive(ctx, s, &sec, result)
	}
	return nil
}

// a2a_agent_defs, then a2a_server_card_defs, each with its pointers:
// before any resumed run, whose A2A peer tools are enumerated at run
// start; the cards after the agent defs they expose.
func restoreA2AAgentDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result := rs.s, rs.opts, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionA2AAgentDefs]; ok {
		var sec A2AAgentDefsSection
		if err := decodeWithMigration(migrations.SectionA2AAgentDefs, rawSection, &sec); err != nil {
			return err
		}
		restoreA2AAgentDefs(ctx, s, &sec, opts, result)
	}
	return nil
}

// restoreA2AAgentDefActiveSection restores the a2a_agent_def_active section (its place: restoreSectionOrder).
func restoreA2AAgentDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionA2AAgentDefActive]; ok {
		var sec A2AAgentDefActiveSection
		if err := decodeWithMigration(migrations.SectionA2AAgentDefActive, rawSection, &sec); err != nil {
			return err
		}
		restoreA2AAgentDefActive(ctx, s, &sec, result)
	}
	return nil
}

// restoreA2AServerCardDefsSection restores the a2a_server_card_defs section (its place: restoreSectionOrder).
func restoreA2AServerCardDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionA2AServerCardDefs]; ok {
		var sec A2AServerCardDefsSection
		if err := decodeWithMigration(migrations.SectionA2AServerCardDefs, rawSection, &sec); err != nil {
			return err
		}
		restoreA2AServerCardDefs(ctx, s, &sec, opts, scan, result)
	}
	return nil
}

// restoreA2AServerCardDefActiveSection restores the a2a_server_card_def_active section (its place: restoreSectionOrder).
func restoreA2AServerCardDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionA2AServerCardDefActive]; ok {
		var sec A2AServerCardDefActiveSection
		if err := decodeWithMigration(migrations.SectionA2AServerCardDefActive, rawSection, &sec); err != nil {
			return err
		}
		restoreA2AServerCardDefActive(ctx, s, &sec, result)
	}
	return nil
}
