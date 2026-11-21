package main

import "github.com/denn-gubsky/loomcycle/internal/config"

// heldTools is what a run of a holds by its tools list: the grants, with the
// default tools a run's toolset adds when it is resolved (Calc) unless the
// list removes them. A bundle test that pins an agent's tools pins this, so a
// worker that must hold nothing else fails when it would also hold Calc.
// Context is not added here: config load already wrote it into a.Tools.
func heldTools(a config.AgentDef) []string {
	grants, _ := config.SplitTools(config.EffectiveTools(a.Tools))
	return grants
}
