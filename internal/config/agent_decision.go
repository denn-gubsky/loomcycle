package config

import (
	"fmt"
	"strings"
)

// AgentDecision is an agent's `decision` block (yaml, and the AgentDef
// create/fork overlay): which of the operator's decision models this agent's
// Decision calls may name, and which one answers when a call names none.
//
// IT ONLY NARROWS. The models are the operator's (the top-level decision:
// block): every name here must be one of them, and a name that is not is
// refused when the agent is authored, never widened into. Unset, the agent may
// name any of the operator's models and gets the operator's default.
type AgentDecision struct {
	// Default answers a call that names no model. It must be one of Models when
	// Models is set, and one of the operator's models either way.
	Default string `json:"default,omitempty" yaml:"default"`
	// Models are the models a call may name. Empty = the operator's whole list.
	Models []string `json:"models,omitempty" yaml:"models"`
}

// IsZero reports whether nothing is set, so an empty block collapses to nil and
// an agent that never mentions it keeps a byte-stable content hash.
func (d *AgentDecision) IsZero() bool {
	return d == nil || (d.Default == "" && len(d.Models) == 0)
}

// Clone returns a copy that shares nothing with d, or nil for an empty block.
func (d *AgentDecision) Clone() *AgentDecision {
	if d.IsZero() {
		return nil
	}
	return &AgentDecision{Default: d.Default, Models: append([]string(nil), d.Models...)}
}

// Validate checks the block against itself: no empty or repeated name, and a
// default that is among the models when models are listed.
func (d *AgentDecision) Validate() error {
	if d.IsZero() {
		return nil
	}
	seen := map[string]bool{}
	for _, m := range d.Models {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("decision.models: an entry is empty")
		}
		if seen[m] {
			return fmt.Errorf("decision.models: %q is listed twice", m)
		}
		seen[m] = true
	}
	if d.Default != "" && len(d.Models) > 0 && !seen[d.Default] {
		return fmt.Errorf("decision.default: %q is not one of decision.models %v", d.Default, d.Models)
	}
	return nil
}

// Pick resolves the model one call uses, given the operator's models and
// default. asked is the name the call gave ("" = none).
//
// The models the agent may name are its own list, less any the operator no
// longer offers (a stored definition can outlive the operator's list); with no
// list of its own, the operator's. A call that names none gets, in order: the
// agent's default, the operator's default, the first of the agent's list — each
// only if it is among the models the agent may name. ok is false when asked is
// not among them, or when none is left; allowed is what to tell the caller.
func (d *AgentDecision) Pick(asked, operatorDefault string, operator []string) (name string, allowed []string, ok bool) {
	offered := map[string]bool{}
	for _, m := range operator {
		offered[m] = true
	}
	allowed = operator
	if d != nil && len(d.Models) > 0 {
		allowed = nil
		for _, m := range d.Models {
			if offered[m] {
				allowed = append(allowed, m)
			}
		}
	}
	may := map[string]bool{}
	for _, m := range allowed {
		may[m] = true
	}
	if asked != "" {
		return asked, allowed, may[asked]
	}
	if d != nil && may[d.Default] {
		return d.Default, allowed, true
	}
	if may[operatorDefault] {
		return operatorDefault, allowed, true
	}
	if len(allowed) > 0 {
		return allowed[0], allowed, true
	}
	return "", allowed, false
}

// DecisionModelNames are the names the decision block allows, in the order
// DecisionEntries gives them. Empty when the block is unset.
func (c *Config) DecisionModelNames() []string {
	entries := c.DecisionEntries()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// CheckAgentDecision refuses an agent's decision block that names a model the
// operator's decision block does not allow. It is the one check both planes
// share: operator yaml at load, a runtime-authored definition at create/fork.
func (c *Config) CheckAgentDecision(d *AgentDecision) error {
	if d.IsZero() {
		return nil
	}
	if err := d.Validate(); err != nil {
		return err
	}
	names := c.DecisionModelNames()
	if len(names) == 0 {
		return fmt.Errorf("decision: this deployment declares no decision models, so an agent cannot narrow them")
	}
	offered := map[string]bool{}
	for _, n := range names {
		offered[n] = true
	}
	if d.Default != "" && !offered[d.Default] {
		return fmt.Errorf("decision.default: %q is not one of the decision models %v", d.Default, names)
	}
	for _, m := range d.Models {
		if !offered[m] {
			return fmt.Errorf("decision.models: %q is not one of the decision models %v", m, names)
		}
	}
	return nil
}
