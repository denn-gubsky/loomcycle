package snapshot

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// restoreRun carries what Restore's stage-2 sections share: the target store,
// the options, the decoded section map, the result every section counts into,
// the missing-credential scan the definition sections feed, and the schedules
// schedule_defs restored for schedule_def_active to read.
type restoreRun struct {
	s        store.Store
	opts     RestoreOptions
	sections map[string]json.RawMessage
	result   *RestoreResult
	// scan collects the definitions this restore brings back that name
	// credentials; the missing-credential scan checks them once every def
	// section has landed.
	scan              *credScan
	restoredSchedules map[string]restoredSchedule
}

// sectionRestorer is one stage-2 step. name is the snapshot section it
// restores (or a pseudo-section for a step that reads no section of its own);
// restore returns an error only where the step aborts the whole restore.
type sectionRestorer struct {
	name    string
	restore func(ctx context.Context, rs *restoreRun) error
}

// credentialScanStep names the step that checks, once every definition that
// can name a credential has landed, which of them this host cannot supply.
const credentialScanStep = "credential_scan"

// restoreSectionOrder is stage 2 of Restore, in the order it runs. The order is
// the foreign-key and naming dependency order; the comment on each entry says
// what it must come after, and TestRestoreSectionOrder_SatisfiesDependencies
// pins it.
func restoreSectionOrder() []sectionRestorer {
	return []sectionRestorer{
		// First: before any definition or resumed run.
		{migrations.SectionUsers, restoreUsersSection},
		// After users.
		{migrations.SectionTokenLimits, restoreTokenLimitsSection},
		// After the users its memberships and bindings name; before the paused
		// and queued runs, re-narrowed by their caller's set.
		{migrations.SectionACL, restoreACLSection},
		// Before every definition that names a credential.
		{migrations.SectionOAuthCredentials, restoreOAuthCredentialsSection},
		// After oauth_credentials.
		{migrations.SectionAgentDefs, restoreAgentDefsSection},
		// After agent_defs (FK: name → agent_defs.def_id).
		{migrations.SectionAgentDefActive, restoreAgentDefActiveSection},
		// After oauth_credentials.
		{migrations.SectionSkillDefs, restoreSkillDefsSection},
		// After skill_defs (FK).
		{migrations.SectionSkillDefActive, restoreSkillDefActiveSection},
		// After oauth_credentials; before channels (a team's own channels).
		{migrations.SectionTeamDefs, restoreTeamDefsSection},
		// After team_defs (FK).
		{migrations.SectionTeamDefActive, restoreTeamDefActiveSection},
		// After oauth_credentials.
		{migrations.SectionHookDefs, restoreHookDefsSection},
		// After hook_defs (FK).
		{migrations.SectionHookDefActive, restoreHookDefActiveSection},
		// After oauth_credentials.
		{migrations.SectionMCPServerDefs, restoreMCPServerDefsSection},
		// After mcp_server_defs (FK).
		{migrations.SectionMCPServerDefActive, restoreMCPServerDefActiveSection},
		// After the definitions that bind a volume by name; before any
		// resumed run and before dirents.
		{migrations.SectionVolumeDefs, restoreVolumeDefsSection},
		// After oauth_credentials; before memory and memory_pending.
		{migrations.SectionMemoryBackendDefs, restoreMemoryBackendDefsSection},
		// After memory_backend_defs.
		{migrations.SectionMemoryBackendDefActive, restoreMemoryBackendDefActiveSection},
		// After oauth_credentials.
		{migrations.SectionDocSourceDefs, restoreDocSourceDefsSection},
		// After document_source_defs.
		{migrations.SectionDocSourceDefActive, restoreDocSourceDefActiveSection},
		// Before the server cards that expose it and any resumed run.
		{migrations.SectionA2AAgentDefs, restoreA2AAgentDefsSection},
		// After a2a_agent_defs.
		{migrations.SectionA2AAgentDefActive, restoreA2AAgentDefActiveSection},
		// After a2a_agent_defs and oauth_credentials.
		{migrations.SectionA2AServerCardDefs, restoreA2AServerCardDefsSection},
		// After a2a_server_card_defs.
		{migrations.SectionA2AServerCardDefActive, restoreA2AServerCardDefActiveSection},
		// After memory_backend_defs (a routing def precedes its data).
		{migrations.SectionMemory, restoreMemorySection},
		// After memory and memory_backend_defs.
		{migrations.SectionMemoryPending, restoreMemoryPendingSection},
		// After oauth_credentials; before the channel messages they govern.
		{migrations.SectionChannelDefs, restoreChannelDefsSection},
		// After channel_defs and team_defs.
		{migrations.SectionChannels, restoreChannelsSection},
		// After the agent and channel defs a webhook delivers to.
		{migrations.SectionWebhookDefs, restoreWebhookDefsSection},
		// After webhook_defs.
		{migrations.SectionWebhookDefActive, restoreWebhookDefActiveSection},
		// After the agent and channel defs a schedule names.
		{migrations.SectionScheduleDefs, restoreScheduleDefsSection},
		// After schedule_defs (reads the schedules it restored).
		{migrations.SectionScheduleDefActive, restoreScheduleDefActiveSection},
		// After every definition that can name a credential.
		{credentialScanStep, runCredentialScan},
		// No FK; anywhere after the definitions.
		{migrations.SectionEvaluations, restoreEvaluationsSection},
		// After users, acl and the definitions a resumed run resolves; it
		// synthesizes the sessions its runs need.
		{migrations.SectionPausedRuns, restorePausedRunsSection},
		// After paused_runs (sessions) and acl.
		{migrations.SectionQueuedRuns, restoreQueuedRunsSection},
		// After paused_runs.
		{migrations.SectionInteractionHistory, restoreInteractionHistorySection},
		// After memory (document bodies came with it).
		{migrations.SectionSqlMem, restoreSqlMemSection},
		// LAST of the writes: after every target a name can point at.
		{migrations.SectionDirents, restoreDirentsSection},
		// Writes nothing; re-emits the capture's findings as warnings.
		{migrations.SectionCaptureFindings, restoreCaptureFindingsSection},
	}
}
