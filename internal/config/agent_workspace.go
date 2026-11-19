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
// Only the keys of RFC GW P1 exist. The keys later phases add (share,
// expose, connect, templates, egress) are refused as unknown, in yaml and in
// JSON, so no stored grant can name something that does nothing yet.
type AgentWorkspace struct {
	// Ops is a subset of read, write and exec.
	Ops []string `json:"ops,omitempty" yaml:"ops,omitempty"`
	// Open lets the agent open workspaces of its own.
	Open bool `json:"open,omitempty" yaml:"open,omitempty"`
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
	return &out
}

// ValidateAgentWorkspace refuses a workspace grant holding an unknown or
// repeated operation or an unknown engine. One function for every place a
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
