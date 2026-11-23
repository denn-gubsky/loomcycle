package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// agent_workspace.go — the per-agent `workspace:` grant (RFC GW). The tool it
// governs and the operator's `workspace:` block arrive in later phases; until
// then the grant is inert, and config load says so.

// AgentWorkspace is an agent's `workspace:` grant: which operations it may
// perform in a workspace, whether it may open workspaces of its own, and the
// least isolating engine it accepts.
//
// Absent (a nil pointer on AgentDef) grants nothing. A sub-agent holds the
// operations both it and its parent run hold, may open only when both may,
// and is held to the stricter engine requirement. Not per-run overridable.
// Authority, not content: not part of content_sha256.
//
// Only the keys built so far exist (P1's, P8's share and P4's egress). The
// keys later phases add (expose, connect, templates) are refused as unknown,
// in yaml and in JSON, so no stored grant can name something that does
// nothing yet.
type AgentWorkspace struct {
	// Ops is a subset of read, write and exec.
	Ops []string `json:"ops,omitempty" yaml:"ops,omitempty"`
	// Open lets the agent open workspaces of its own.
	Open bool `json:"open,omitempty" yaml:"open,omitempty"`
	// Share lets the agent share a workspace it owns with another run of
	// the same user (RFC GW §5.2). It hands over only operations the agent
	// holds itself, and never commands.
	Share bool `json:"share,omitempty" yaml:"share,omitempty"`
	// Egress names the routes (workspace.egress.routes, or a tenant's) a
	// workspace this agent opens may reach the network through (RFC GW
	// §6.3). A sub-agent holds only the names both it and its parent list.
	// Alone it grants nothing: it bounds workspaces the agent opens.
	Egress []string `json:"egress,omitempty" yaml:"egress,omitempty"`
	// Engine is the least isolating engine the agent accepts: "" (any),
	// "host" or "container".
	Engine string `json:"engine,omitempty" yaml:"engine,omitempty"`
}

// The workspace operations an agent may be granted.
const (
	WorkspaceOpRead  = "read"
	WorkspaceOpWrite = "write"
	WorkspaceOpExec  = "exec"
)

// The engines an agent may require, from least to most isolating.
const (
	WorkspaceEngineHost      = "host"
	WorkspaceEngineContainer = "container"
)

// Grants reports whether the block grants anything: an operation or open.
// A block that grants nothing is no grant, whatever its engine says.
func (w *AgentWorkspace) Grants() bool {
	return w != nil && (len(w.Ops) > 0 || w.Open)
}

// Clone copies the block, never aliasing its op list.
func (w *AgentWorkspace) Clone() *AgentWorkspace {
	if w == nil {
		return nil
	}
	out := *w
	if w.Ops != nil {
		out.Ops = append([]string{}, w.Ops...)
	}
	if w.Egress != nil {
		out.Egress = append([]string{}, w.Egress...)
	}
	return &out
}

// ValidateAgentWorkspace refuses a workspace grant holding an unknown or
// repeated operation, an unknown engine, or an egress entry that is not a
// route name or is repeated. Whether a name is a route is not checked: a
// tenant's routes live in the store and come and go (workspaceEgressWarnings
// advises on the operator's at load). One function for every place a
// definition is authored — operator yaml, Markdown agents, the AgentDef
// overlay, register_agent — so a value refused at boot is not quietly kept by
// a runtime write.
func ValidateAgentWorkspace(w *AgentWorkspace) error {
	if w == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, op := range w.Ops {
		switch op {
		case WorkspaceOpRead, WorkspaceOpWrite, WorkspaceOpExec:
		default:
			return fmt.Errorf("workspace.ops[%d]: unknown operation %q (want read, write or exec)", i, op)
		}
		if seen[op] {
			return fmt.Errorf("workspace.ops[%d]: %q is listed twice", i, op)
		}
		seen[op] = true
	}
	seenRoute := map[string]bool{}
	for i, name := range w.Egress {
		if !ValidEgressRouteName(name) {
			return fmt.Errorf("workspace.egress[%d]: %q is not a route name (a lowercase letter, then up to 62 lowercase letters, digits or -)", i, name)
		}
		if seenRoute[name] {
			return fmt.Errorf("workspace.egress[%d]: %q is listed twice", i, name)
		}
		seenRoute[name] = true
	}
	switch w.Engine {
	case "", WorkspaceEngineHost, WorkspaceEngineContainer:
	default:
		return fmt.Errorf("workspace.engine: %q is not one of host, container (or unset for any)", w.Engine)
	}
	return nil
}

func (w *AgentWorkspace) UnmarshalYAML(n *yaml.Node) error {
	if err := strictKeys(n, "workspace", nil, reflect.TypeOf(*w)); err != nil {
		return err
	}
	type plain AgentWorkspace
	return n.Decode((*plain)(w))
}

// UnmarshalJSON is strict for the same reason as the yaml decoder: the
// AgentDef overlay and register_agent arrive as JSON, and a key a later phase
// adds must be refused rather than stored to grant nothing. A stored row a
// later version wrote with such a key therefore fails to read on this one:
// closed, never a grant read as narrower or wider than it was written.
func (w *AgentWorkspace) UnmarshalJSON(b []byte) error {
	type plain AgentWorkspace
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	*w = AgentWorkspace(p)
	return nil
}
