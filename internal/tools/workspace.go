package tools

import "github.com/denn-gubsky/loomcycle/internal/config"

// WorkspacePolicyValue is what a run may do with workspaces (RFC GW): the
// operations it may perform in one, whether it may open its own, and the
// least isolating engine it accepts. The zero value allows nothing, so a run
// whose agent never listed workspace, or whose entry point built no policy,
// reaches no workspace.
type WorkspacePolicyValue struct {
	Read, Write, Exec bool
	Open              bool
	// Share lets the run share a workspace it owns with another run of its
	// user (RFC GW §5.2), handing over no more than it holds itself.
	Share bool
	// Engine is the least isolating engine the run accepts: "" (any),
	// config.WorkspaceEngineHost or config.WorkspaceEngineContainer.
	Engine string
}

// workspaceEngineRank orders the engine requirements from none to the most
// isolating. An unknown value ranks highest, so a value that reached here
// without validation (a row stored before a check) can only narrow.
func workspaceEngineRank(engine string) int {
	switch engine {
	case "":
		return 0
	case config.WorkspaceEngineHost:
		return 1
	case config.WorkspaceEngineContainer:
		return 2
	}
	return 3
}

// WorkspaceFor is the policy an agent definition's workspace declares. nil
// grants nothing. Unknown operations are ignored here because
// config.ValidateAgentWorkspace refuses them wherever a definition is written.
func WorkspaceFor(w *config.AgentWorkspace) WorkspacePolicyValue {
	if !w.Grants() {
		return WorkspacePolicyValue{}
	}
	v := WorkspacePolicyValue{Open: w.Open, Share: w.Share, Engine: w.Engine}
	for _, op := range w.Ops {
		switch op {
		case config.WorkspaceOpRead:
			v.Read = true
		case config.WorkspaceOpWrite:
			v.Write = true
		case config.WorkspaceOpExec:
			v.Exec = true
		}
	}
	return v
}

// Grants reports whether the policy allows anything at all.
func (v WorkspacePolicyValue) Grants() bool {
	return v.Read || v.Write || v.Exec || v.Open
}

// Allows reports whether the run may perform op ("read", "write" or "exec")
// in a workspace. Any other op is refused.
func (v WorkspacePolicyValue) Allows(op string) bool {
	switch op {
	case config.WorkspaceOpRead:
		return v.Read
	case config.WorkspaceOpWrite:
		return v.Write
	case config.WorkspaceOpExec:
		return v.Exec
	}
	return false
}

// AcceptsEngine reports whether a workspace on engine meets the run's
// requirement: an engine at least as isolating as the one it names.
func (v WorkspacePolicyValue) AcceptsEngine(engine string) bool {
	if engine != config.WorkspaceEngineHost && engine != config.WorkspaceEngineContainer {
		return false
	}
	return workspaceEngineRank(engine) >= workspaceEngineRank(v.Engine)
}

// AcceptsEngineRequirement reports whether requirement (an agent
// definition's workspace.engine) is at least as strict as the run's own, so a
// definition the run authors with it accepts no engine the run does not.
func (v WorkspacePolicyValue) AcceptsEngineRequirement(requirement string) bool {
	return workspaceEngineRank(requirement) >= workspaceEngineRank(v.Engine)
}

// NarrowWorkspace is a spawned child's policy: each operation only when both
// its parent run and its own definition allow it, open and share only when
// both may, and the stricter of the two engine requirements. A side that grants nothing
// leaves the child nothing.
func NarrowWorkspace(parent, own WorkspacePolicyValue) WorkspacePolicyValue {
	out := WorkspacePolicyValue{
		Read:  parent.Read && own.Read,
		Write: parent.Write && own.Write,
		Exec:  parent.Exec && own.Exec,
		Open:  parent.Open && own.Open,
		Share: parent.Share && own.Share,
	}
	if !out.Grants() {
		return WorkspacePolicyValue{}
	}
	out.Engine = parent.Engine
	if workspaceEngineRank(own.Engine) > workspaceEngineRank(parent.Engine) {
		out.Engine = own.Engine
	}
	return out
}

// Ops is the policy's operations as a workspace.ops list. Never nil, so a
// report of "nothing" reads as an empty list rather than as an absent field.
func (v WorkspacePolicyValue) Ops() []string {
	out := []string{}
	if v.Read {
		out = append(out, config.WorkspaceOpRead)
	}
	if v.Write {
		out = append(out, config.WorkspaceOpWrite)
	}
	if v.Exec {
		out = append(out, config.WorkspaceOpExec)
	}
	return out
}

// Grant is the policy as a workspace block, for the spawn record a resume
// narrows against. nil when it grants nothing, so a record of "nothing" and
// a record written before the field read the same.
func (v WorkspacePolicyValue) Grant() *config.AgentWorkspace {
	if !v.Grants() {
		return nil
	}
	return &config.AgentWorkspace{Ops: v.Ops(), Open: v.Open, Share: v.Share, Engine: v.Engine}
}

// Report is the policy for Context op=self. Always present: ops [] and open
// false is the answer "no workspace access".
func (v WorkspacePolicyValue) Report() map[string]any {
	return map[string]any{"ops": v.Ops(), "open": v.Open, "share": v.Share, "engine": v.Engine}
}
