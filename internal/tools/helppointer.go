package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// HelpIndex is implemented by the Context tool. A run that holds such a tool
// can be sent to a tool's help from that tool's description, and shown a
// correct call when one of its calls fails; a run without one cannot follow
// either, so it gets neither.
type HelpIndex interface {
	// HasHelpTopic reports whether a help topic is named exactly name.
	HasHelpTopic(name string) bool
	// HelpExample returns the first call example of topic as compact JSON — the
	// argument object a correct call passes — or false when it has none.
	HelpExample(topic string) (string, bool)
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
			specs[i].Help = line
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

// helpPointer is the line a tool's description ends with. It is an
// INSTRUCTION, not a notice: measured on local models, a line that only said
// where the help was ("Usage and examples: call Context with …") was never
// followed, while a model that did read an article made no malformed calls
// afterwards. So it says when to read it — before the first call — and gives
// the call to make with a real topic, not a placeholder to fill in.
//
// The help tool itself gets no instruction: "before calling Context, call
// Context" is a loop, not guidance.
func helpPointer(ctx context.Context, t Tool, helpTool string, idx HelpIndex) string {
	name := t.Name()
	var parts []string
	if name != helpTool && idx.HasHelpTopic(name) {
		if op := firstDocumentedOp(t, idx); op != "" {
			parts = append(parts, fmt.Sprintf(
				"Before your first call to %s, read the call format for the operation you need: call %s with %s (put your operation in place of %s). It gives the exact arguments and an example.",
				name, helpTool, helpArgs(name+"/"+op), op))
		} else {
			parts = append(parts, fmt.Sprintf(
				"Before your first call to %s, read its call format: call %s with %s. It gives the exact arguments and an example.",
				name, helpTool, helpArgs(name)))
		}
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

// withHelpPointer adds call guidance to a FAILED call of a documented tool.
// A failed call is the moment a model is guaranteed to be reading, and the most
// likely cause is a call shape it got wrong, so it gets a correct example call
// for the operation it tried, the valid operations when the op itself was
// wrong, and the help call for the full article.
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
	var in struct {
		Op string `json:"op"`
	}
	_ = json.Unmarshal(input, &in)
	tool, _ := d.tools[name]
	ops := []string(nil)
	if tool != nil {
		ops = SchemaEnum(tool.InputSchema(), "op")
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(res.Text, " \n"))
	topic := name
	switch {
	case in.Op != "" && d.help.HasHelpTopic(name+"/"+in.Op):
		topic = name + "/" + in.Op
	case len(ops) > 0 && (in.Op == "" || !contains(ops, in.Op)):
		// The op itself is what is wrong, so the one thing that helps is the
		// list of ops this tool has.
		fmt.Fprintf(&b, "\n\nValid operations for %s: %s.", name, strings.Join(ops, ", "))
	}
	// Measured on local models: a pointer to the help was never followed, but
	// a model recovers from what the error itself says. So the error carries a
	// correct call, taken from the operation's own article, rather than only
	// the address of one.
	if ex, ok := d.help.HelpExample(topic); ok {
		fmt.Fprintf(&b, "\n\nA correct %s call looks like this (an example — use your own values):\n%s", topic, ex)
	}
	fmt.Fprintf(&b, "\n\nFull reference: call %s with %s.", d.helpName, helpArgs(topic))
	res.Text = b.String()
	return res
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
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
