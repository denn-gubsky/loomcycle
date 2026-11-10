package snapshot

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
)

// webhook_defs, then webhook_def_active: after the agent and channel defs
// a webhook delivers to.
func restoreWebhookDefsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result, scan := rs.s, rs.opts, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionWebhookDefs]; ok {
		var sec WebhookDefsSection
		if err := decodeWithMigration(migrations.SectionWebhookDefs, rawSection, &sec); err != nil {
			return err
		}
		restoreWebhookDefs(ctx, s, &sec, opts, scan, result)
	}
	return nil
}

// restoreWebhookDefActiveSection restores the webhook_def_active section (its place: restoreSectionOrder).
func restoreWebhookDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionWebhookDefActive]; ok {
		var sec WebhookDefActiveSection
		if err := decodeWithMigration(migrations.SectionWebhookDefActive, rawSection, &sec); err != nil {
			return err
		}
		restoreWebhookDefActive(ctx, s, &sec, result)
	}
	return nil
}

// schedule_defs with their run state, then schedule_def_active: after the
// agent and channel defs a schedule names. A restored schedule is ACTIVE.
func restoreScheduleDefsSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result, scan := rs.s, rs.sections, rs.result, rs.scan
	if rawSection, ok := sections[migrations.SectionScheduleDefs]; ok {
		var sec ScheduleDefsSection
		if err := decodeWithMigration(migrations.SectionScheduleDefs, rawSection, &sec); err != nil {
			return err
		}
		rs.restoredSchedules = restoreScheduleDefs(ctx, s, &sec, scan, result)
	}
	return nil
}

// restoreScheduleDefActiveSection restores the schedule_def_active section (its place: restoreSectionOrder).
func restoreScheduleDefActiveSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result, restoredSchedules := rs.s, rs.sections, rs.result, rs.restoredSchedules
	if rawSection, ok := sections[migrations.SectionScheduleDefActive]; ok {
		var sec ScheduleDefActiveSection
		if err := decodeWithMigration(migrations.SectionScheduleDefActive, rawSection, &sec); err != nil {
			return err
		}
		restoreScheduleDefActive(ctx, s, &sec, restoredSchedules, result)
	}
	return nil
}

// Every definition that can name a credential has landed — the agent,
// team, hook, MCP server and channel defs, the triggers, cards, memory
// backends and document sources: say which credentials they name that
// this host cannot supply.
func runCredentialScan(ctx context.Context, rs *restoreRun) error {
	opts, result, scan := rs.opts, rs.result, rs.scan
	scan.run(ctx, opts, result)
	return nil
}

// capture_findings: nothing is written. Each finding is re-emitted as a
// warning, so the operator restoring — who may never have seen the capture
// response — learns which restored definitions carry a literal credential.
func restoreCaptureFindingsSection(ctx context.Context, rs *restoreRun) error {
	sections, result := rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionCaptureFindings]; ok {
		var sec CaptureFindingsSection
		if err := decodeWithMigration(migrations.SectionCaptureFindings, rawSection, &sec); err != nil {
			return err
		}
		for _, f := range sec.Entries {
			result.Warnings = append(result.Warnings, "capture finding: "+f.Warning())
		}
	}
	return nil
}
