package tools

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// ModalTool is a tool whose every operation is classified as a read or a
// write, so an agent definition can grant it in read mode (tool_modes, RFC
// GL D6). The classification belongs to the tool, next to its op list; the
// dispatcher only asks.
type ModalTool interface {
	// CallIsRead reports the operation input names and whether the call only
	// reads. A call it cannot classify — an unknown op, an input it cannot
	// decode — is not a read: the dispatcher then refuses it in read mode.
	CallIsRead(input json.RawMessage) (op string, read bool)
}

// SetToolModes narrows tools this dispatcher holds to some of their
// operations: modes maps a tool name to its mode (config.AgentDef.ToolModes).
// Set once, when a run's dispatcher is built from its definition, so every
// call through it is held to the modes, whoever makes it.
func (d *Dispatcher) SetToolModes(modes map[string]string) {
	if len(modes) == 0 {
		d.modes = nil
		return
	}
	d.modes = maps.Clone(modes)
}

// refuseToolMode refuses a call the tool's mode does not allow, before the
// tool runs, so nothing is changed. It fails closed: a tool with a mode that
// cannot classify the call (not a ModalTool, or not this dispatcher's), or a
// mode other than read, refuses the call.
func (d *Dispatcher) refuseToolMode(name string, input json.RawMessage) (Result, bool) {
	mode, ok := d.modes[name]
	if !ok {
		return Result{}, false
	}
	op, read := "", false
	if m, isModal := d.tools[name].(ModalTool); isModal {
		op, read = m.CallIsRead(input)
	}
	if read && mode == config.ToolModeRead {
		return Result{}, false
	}
	what := "op " + op
	if op == "" {
		what = "a call with no recognised op"
	}
	msg := fmt.Sprintf("%s refused: this agent holds the %s tool in %s mode, which allows only its read operations, and %s is not one. Nothing was changed.",
		name, name, mode, what)
	return Result{
		Text:    msg,
		IsError: true,
		Error: &ErrorInfo{Category: CategoryPermission, Retryable: false,
			Description: "Use a read operation of " + name + " instead, or ask an operator for an agent that may write."},
	}, true
}
