package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
)

// ToolModeRead is the one tool mode: the tool runs only its read operations.
const ToolModeRead = "read"

// ToolModeTools is the closed set of tools a tool_modes entry may name. Each
// classifies its every operation as a read or a write (the tool package's
// ModalTool), so the dispatcher can refuse a write before the tool runs. A
// tool is added here only together with that classification: a mode on a tool
// that cannot classify its calls would refuse every call.
var ToolModeTools = map[string]bool{
	"Document": true,
	"Path":     true,
}

// ValidateToolModes refuses a tool_modes map that names a tool outside
// ToolModeTools, a mode other than "read", or a tool the agent's `tools` does
// not grant. tools is the agent's raw tool declaration; a tool is granted when
// it matches an entry the way the runtime grants it (an exact name, or a
// trailing-* prefix glob).
//
// A mode on an ungranted tool is refused rather than ignored: it narrows
// nothing, and the operator who wrote it believed it did.
func ValidateToolModes(modes map[string]string, tools []string) error {
	if len(modes) == 0 {
		return nil
	}
	granted := make(map[string]bool, len(tools))
	for _, t := range tools {
		granted[t] = true
	}
	names := make([]string, 0, len(modes))
	for name := range modes {
		names = append(names, name)
	}
	if err := ValidateToolModeNames(modes); err != nil {
		return err
	}
	sort.Strings(names) // a stable first error across runs
	for _, name := range names {
		if !policy.Matches(name, granted) {
			return fmt.Errorf("tool_modes.%s: %s is not in tools, so the mode narrows nothing; add %s to tools or drop the mode", name, name, name)
		}
	}
	return nil
}

// toolModeToolNames renders ToolModeTools for an error message, derived from
// the set so the message cannot fall behind it.
func toolModeToolNames() string {
	names := make([]string, 0, len(ToolModeTools))
	for n := range ToolModeTools {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ValidateToolModeNames is ValidateToolModes without the check against tools:
// each entry names a tool that can take a mode, and the mode is "read". The
// AgentDef overlay is judged by it alone, before it is merged with the
// definition it forks; the merged result is then judged in full.
func ValidateToolModeNames(modes map[string]string) error {
	names := make([]string, 0, len(modes))
	for name := range modes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !ToolModeTools[name] {
			return fmt.Errorf("tool_modes: %q cannot be given a mode (only %s can)", name, toolModeToolNames())
		}
		if mode := modes[name]; mode != ToolModeRead {
			return fmt.Errorf("tool_modes.%s: unknown mode %q (want %q)", name, mode, ToolModeRead)
		}
	}
	return nil
}
