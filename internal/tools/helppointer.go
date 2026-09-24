package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// HelpIndex is implemented by the Context tool. It answers one question: is
// there a help topic by exactly this name? A run that holds such a tool can be
// pointed at a tool's help from that tool's description; a run without one
// cannot follow the pointer, so it gets none.
type HelpIndex interface {
	HasHelpTopic(name string) bool
}

// ScopeGrant is whether THIS run may use one value of a tool's scope argument.
type ScopeGrant struct {
	Scope   string `json:"scope"`
	Granted bool   `json:"granted"`
	// Reason is the refusal the tool would return for this value; empty when
	// granted.
	Reason string `json:"reason,omitempty"`
}

// ScopeField is one way a tool reads its scope argument, and the run's grant for
// every value of it. Most tools have one. Memory has two, because its SQL
// operations check the same argument against a separate grant.
type ScopeField struct {
	// Applies names the operations this field governs; "" means all of them.
	Applies string       `json:"applies,omitempty"`
	Grants  []ScopeGrant `json:"grants"`
}

// ScopedTool is implemented by every tool whose input has a `scope` argument
// with a fixed set of values. ScopeGrants must come from the same check the
// tool applies when it is called — the report exists so a model learns its
// grants before a refusal, and a report that can disagree with the refusal is
// worse than none.
type ScopedTool interface {
	ScopeGrants(ctx context.Context) []ScopeField
}

// SchemaEnum returns the enum of property prop in a JSON-Schema object, or nil
// when the schema does not parse or the property has no enum.
func SchemaEnum(schema json.RawMessage, prop string) []string {
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil
	}
	return s.Properties[prop].Enum
}

// ScopesHelpTopic is the help topic that explains scopes across every tool.
const ScopesHelpTopic = "scopes"

// SpecsFor is Specs for one run. When the run holds a HelpIndex tool (Context),
// each tool that has a help article gets one line naming the exact call that
// returns it, and each scoped tool also says which scopes this run may use.
//
// Both lines are derived: a tool gains its pointer when its article exists and
// loses it when the run cannot call Context. Nothing here is per request, so a
// run's tool array stays byte-identical from call to call.
//
// The pointer shows only the ARGUMENT object. A {name, arguments} envelope or a
// tagged block is the shape small models copy into their reply as text, which
// turns guidance into a broken tool call.
func (d *Dispatcher) SpecsFor(ctx context.Context, order []Tool) []providers.ToolSpec {
	specs := d.Specs(order)
	idx, helpTool := d.helpIndex(order)
	if idx == nil {
		return specs
	}
	byName := make(map[string]Tool, len(order))
	for _, t := range order {
		byName[t.Name()] = t
	}
	for i := range specs {
		t := byName[specs[i].Name]
		if t == nil {
			continue
		}
		if line := helpPointer(ctx, t, helpTool, idx); line != "" {
			specs[i].Description = strings.TrimRight(specs[i].Description, " \n") + "\n\n" + line
		}
	}
	return specs
}

// helpIndex returns the run's HelpIndex tool and its name, or nil. It must be a
// tool this dispatcher will actually dispatch, or the pointer names a call the
// run cannot make.
func (d *Dispatcher) helpIndex(order []Tool) (HelpIndex, string) {
	for _, t := range order {
		if _, held := d.tools[t.Name()]; !held {
			continue
		}
		if idx, ok := t.(HelpIndex); ok {
			return idx, t.Name()
		}
	}
	return nil, ""
}

func helpPointer(ctx context.Context, t Tool, helpTool string, idx HelpIndex) string {
	name := t.Name()
	var parts []string
	if idx.HasHelpTopic(name) {
		s := fmt.Sprintf("Usage and examples: call %s with %s", helpTool, helpArgs(name))
		if op := firstDocumentedOp(t, idx); op != "" {
			s += fmt.Sprintf("; for one operation: %s", helpArgs(name+"/"+op))
		}
		parts = append(parts, s+".")
	}
	if st, ok := t.(ScopedTool); ok {
		if s := scopeLine(st.ScopeGrants(ctx)); s != "" {
			if idx.HasHelpTopic(ScopesHelpTopic) {
				s += fmt.Sprintf(" What scopes mean: %s.", helpArgs(ScopesHelpTopic))
			}
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// withHelpPointer adds the help call to a FAILED call of a documented tool.
// A failed call is the moment a model is guaranteed to be reading, and the most
// likely cause is a call shape it got wrong, so it gets the article for the
// operation it tried (or the tool's, when that operation has none).
//
// Left alone: successes; a tool with no article; a dispatcher that cannot
// serve help; and a failure marked retryable, where the fix is to send the
// same call again and a pointer to the manual would only suggest otherwise.
func (d *Dispatcher) withHelpPointer(name string, input json.RawMessage, res Result) Result {
	if !res.IsError || d.help == nil || !d.help.HasHelpTopic(name) {
		return res
	}
	if res.Error != nil && res.Error.Retryable {
		return res
	}
	topic := name
	var in struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(input, &in) == nil && in.Op != "" && d.help.HasHelpTopic(name+"/"+in.Op) {
		topic = name + "/" + in.Op
	}
	res.Text = strings.TrimRight(res.Text, " \n") +
		fmt.Sprintf("\n\nHow to call it: call %s with %s.", d.helpName, helpArgs(topic))
	return res
}

// helpArgs renders the argument object of a help call. Marshalled, not
// formatted, so a topic name can never break the JSON.
func helpArgs(topic string) string {
	b, _ := json.Marshal(struct {
		Op    string `json:"op"`
		Topic string `json:"topic"`
	}{"help", topic})
	return string(b)
}

// firstDocumentedOp is the first operation, in the tool's own schema order, that
// has an article — a concrete example of the second call, rather than a
// placeholder a model has to fill in. "" for a tool without operations.
func firstDocumentedOp(t Tool, idx HelpIndex) string {
	for _, op := range SchemaEnum(t.InputSchema(), "op") {
		if idx.HasHelpTopic(t.Name() + "/" + op) {
			return op
		}
	}
	return ""
}

// scopeLine renders a run's scope grants, e.g.
//
//	Scopes this run may use: agent, user (not tenant); for sql_* ops: none.
func scopeLine(fields []ScopeField) string {
	var clauses []string
	for _, f := range fields {
		var granted, refused []string
		for _, g := range f.Grants {
			if g.Granted {
				granted = append(granted, g.Scope)
			} else {
				refused = append(refused, g.Scope)
			}
		}
		if len(granted)+len(refused) == 0 {
			continue
		}
		c := "none"
		if len(granted) > 0 {
			c = strings.Join(granted, ", ")
		}
		if len(granted) > 0 && len(refused) > 0 {
			c += " (not " + strings.Join(refused, ", ") + ")"
		}
		if f.Applies != "" {
			c = "for " + f.Applies + ": " + c
		}
		clauses = append(clauses, c)
	}
	if len(clauses) == 0 {
		return ""
	}
	return "Scopes this run may use: " + strings.Join(clauses, "; ") + "."
}
