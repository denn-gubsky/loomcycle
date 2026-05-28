package claudeimport

// walkAgents enumerates .claude/agents/<name>.md, parses each, and
// appends an AgentEntry to the report. The v0.12.7 substrate-field
// emission (credentials comment, schedule_def_scopes + agent_def_scopes
// stubs) lives in agentToYAML; see agent_emit.go (subsequent commit).
//
// This stub is filled in by the agents-mapper commit.
func walkAgents(dir string, report *ImportReport) error {
	// Implemented in the next commit. The stub is intentionally a
	// no-op so the package builds at every commit boundary.
	_ = dir
	_ = report
	return nil
}
