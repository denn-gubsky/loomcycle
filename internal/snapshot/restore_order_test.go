package snapshot

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
)

// restoreOrderDeps is the dependency list stage 2 of Restore must satisfy,
// written independently of the table it checks: each pair is {before, after}.
// A new section added in the wrong place, or an entry moved ahead of what it
// needs, fails TestRestoreSectionOrder_SatisfiesDependencies.
func restoreOrderDeps() [][2]string {
	deps := [][2]string{
		// Users before everything that names them.
		{migrations.SectionUsers, migrations.SectionTokenLimits},
		{migrations.SectionUsers, migrations.SectionACL},
		{migrations.SectionUsers, migrations.SectionAgentDefs},
		// The caller's ACL set re-narrows the resumed runs.
		{migrations.SectionACL, migrations.SectionPausedRuns},
		{migrations.SectionACL, migrations.SectionQueuedRuns},
		// Sessions are synthesized by paused_runs; queued runs come after.
		{migrations.SectionPausedRuns, migrations.SectionQueuedRuns},
		// A routing def precedes the data routed through it.
		{migrations.SectionMemoryBackendDefs, migrations.SectionMemory},
		{migrations.SectionMemoryBackendDefs, migrations.SectionMemoryPending},
		{migrations.SectionMemory, migrations.SectionMemoryPending},
		// Channel defs before the messages they govern; a team's own
		// channels' rows only for a team defined here.
		{migrations.SectionChannelDefs, migrations.SectionChannels},
		{migrations.SectionTeamDefs, migrations.SectionChannels},
		// Triggers after the agent and channel defs they name.
		{migrations.SectionAgentDefs, migrations.SectionWebhookDefs},
		{migrations.SectionChannelDefs, migrations.SectionWebhookDefs},
		{migrations.SectionAgentDefs, migrations.SectionScheduleDefs},
		{migrations.SectionChannelDefs, migrations.SectionScheduleDefs},
		// Server cards after the agent defs they expose.
		{migrations.SectionA2AAgentDefs, migrations.SectionA2AServerCardDefs},
		// Volumes before a resumed run resolves one.
		{migrations.SectionVolumeDefs, migrations.SectionPausedRuns},
		// SQL Memory's document structure after the bodies in memory.
		{migrations.SectionMemory, migrations.SectionSqlMem},
	}
	// Each definition before its active pointer.
	for _, p := range [][2]string{
		{migrations.SectionAgentDefs, migrations.SectionAgentDefActive},
		{migrations.SectionSkillDefs, migrations.SectionSkillDefActive},
		{migrations.SectionTeamDefs, migrations.SectionTeamDefActive},
		{migrations.SectionHookDefs, migrations.SectionHookDefActive},
		{migrations.SectionMCPServerDefs, migrations.SectionMCPServerDefActive},
		{migrations.SectionMemoryBackendDefs, migrations.SectionMemoryBackendDefActive},
		{migrations.SectionDocSourceDefs, migrations.SectionDocSourceDefActive},
		{migrations.SectionA2AAgentDefs, migrations.SectionA2AAgentDefActive},
		{migrations.SectionA2AServerCardDefs, migrations.SectionA2AServerCardDefActive},
		{migrations.SectionWebhookDefs, migrations.SectionWebhookDefActive},
		{migrations.SectionScheduleDefs, migrations.SectionScheduleDefActive},
	} {
		deps = append(deps, p)
	}
	// Every definition family that can name a credential: after the OAuth
	// credentials (so the scan finds them) and before the scan itself.
	for _, def := range []string{
		migrations.SectionAgentDefs,
		migrations.SectionTeamDefs,
		migrations.SectionHookDefs,
		migrations.SectionMCPServerDefs,
		migrations.SectionMemoryBackendDefs,
		migrations.SectionDocSourceDefs,
		migrations.SectionA2AServerCardDefs,
		migrations.SectionChannelDefs,
		migrations.SectionWebhookDefs,
		migrations.SectionScheduleDefs,
	} {
		deps = append(deps,
			[2]string{migrations.SectionOAuthCredentials, def},
			[2]string{def, credentialScanStep})
	}
	return deps
}

func TestRestoreSectionOrder_SatisfiesDependencies(t *testing.T) {
	order := restoreSectionOrder()
	pos := make(map[string]int, len(order))
	for i, e := range order {
		if _, dup := pos[e.name]; dup {
			t.Fatalf("section %s appears twice in restoreSectionOrder", e.name)
		}
		if e.restore == nil {
			t.Fatalf("section %s has no restore func", e.name)
		}
		pos[e.name] = i
	}
	for _, d := range restoreOrderDeps() {
		before, okB := pos[d[0]]
		after, okA := pos[d[1]]
		if !okB || !okA {
			t.Errorf("dependency %s -> %s names a section missing from restoreSectionOrder", d[0], d[1])
			continue
		}
		if before >= after {
			t.Errorf("restoreSectionOrder: %s (at %d) must come before %s (at %d)", d[0], before, d[1], after)
		}
	}
	// dirents last of the writes: every target a name can point at lands
	// first. capture_findings writes nothing and may follow it.
	dirents := pos[migrations.SectionDirents]
	for name, i := range pos {
		if name == migrations.SectionDirents || name == migrations.SectionCaptureFindings {
			continue
		}
		if i > dirents {
			t.Errorf("restoreSectionOrder: %s (at %d) must come before dirents (at %d)", name, i, dirents)
		}
	}
}

// Every stage-2 step but the credential scan restores a section migrations
// knows, so a misspelt section name in the table cannot pass unnoticed.
func TestRestoreSectionOrder_NamesOnlyKnownSections(t *testing.T) {
	pos := map[string]bool{}
	for _, e := range restoreSectionOrder() {
		pos[e.name] = true
	}
	for name := range pos {
		if name == credentialScanStep {
			continue
		}
		if !migrations.KnownSection(name) {
			t.Errorf("restoreSectionOrder names %s, which migrations does not know", name)
		}
	}
}
