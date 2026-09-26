package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// refuseUnknownFields refuses a call to a documented tool that passes a
// top-level argument its input schema does not declare.
//
// Tools decode their input into a struct, and a field the struct does not
// have is dropped without a word. Measured on local models, that was the most
// common malformed call left: `text` for `body`, `chunkId` for `id`,
// `filter`, `chat_id`, or every argument nested under "input". The call went
// through with the field ignored — a create_chunk with `text` saved an EMPTY
// body and reported success — so no error fired and nothing showed the model
// the right call. Refusing it routes the call through the failed-call path,
// which appends a correct example for the operation.
//
// Scope, derived rather than listed: tools with a help article (the ones the
// failed-call path can show an example for), whose schema declares its
// properties and does not allow extra ones. Nested objects are not checked:
// several tools take free-form objects (fields, metadata) by design.
func (d *Dispatcher) refuseUnknownFields(t Tool, input json.RawMessage) (Result, bool) {
	if d.help == nil || !d.help.HasHelpTopic(t.Name()) {
		return Result{}, false
	}
	props, open := schemaProperties(t.InputSchema())
	if open || len(props) == 0 {
		return Result{}, false
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, false // not an object; the tool reports its own error
	}
	var unknown []string
	for k := range in {
		if !props[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return d.refuseSiblingArguments(t, in)
	}
	sort.Strings(unknown)

	var b strings.Builder
	quoted := make([]string, len(unknown))
	for i, k := range unknown {
		quoted[i] = fmt.Sprintf("%q", k)
	}
	fmt.Fprintf(&b, "%s: unknown argument %s — nothing was done. Pass only this tool's own arguments, at the top level.",
		t.Name(), strings.Join(quoted, ", "))
	// The two shapes seen most: arguments wrapped in an envelope, and a near
	// spelling of a real argument.
	for _, k := range unknown {
		switch k {
		case "input", "arguments", "args", "params", "parameters":
			if json.Valid(in[k]) && strings.HasPrefix(strings.TrimSpace(string(in[k])), "{") {
				fmt.Fprintf(&b, " Do not wrap the arguments in %q; put them directly in the call.", k)
			}
			continue
		}
		if s := nearArgument(k, props); s != "" {
			fmt.Fprintf(&b, " Did you mean %q instead of %q?", s, k)
		}
	}
	return Result{Text: b.String(), IsError: true}, true
}

// ArgumentRefusal reports whether Execute would refuse input to the named tool
// for its arguments — an unknown one, or a sibling operation's — before running
// it, and with what message. It is the same check Execute applies, exported so
// the help corpus can prove every example it teaches passes it.
func (d *Dispatcher) ArgumentRefusal(name string, input json.RawMessage) (string, bool) {
	t, ok := d.tools[name]
	if !ok {
		return "", false
	}
	r, refused := d.refuseUnknownFields(t, input)
	return r.Text, refused
}

// refuseSiblingArguments refuses an argument the tool's schema declares but
// THIS operation does not take, because it belongs to a sibling operation.
//
// An op-dispatched tool has one schema for every op, so the schema alone cannot
// say that `parent` is propose_entity's and not create_chunk's. The call went
// through with the argument dropped. Measured on local models: `parent` for
// `parent_id` on create_chunk (the chunk landed under the wrong parent), and
// `query` on query_documents, which ignored it and returned the whole document
// list — 25.8K characters — into a small context.
//
// Which op takes what comes from the op's help article, the same text the model
// is told to read. An argument is refused only when the op's article lists its
// arguments, does not list this one, and a sibling op's article does. An
// argument no article mentions is not refused: nothing says whose it is.
func (d *Dispatcher) refuseSiblingArguments(t Tool, in map[string]json.RawMessage) (Result, bool) {
	idx, ok := d.help.(OpArgumentIndex)
	if !ok {
		return Result{}, false
	}
	var op string
	if raw, has := in["op"]; !has || json.Unmarshal(raw, &op) != nil || op == "" {
		return Result{}, false
	}
	own, ok := idx.HelpArguments(t.Name() + "/" + op)
	if !ok {
		return Result{}, false
	}
	takes := make(map[string]bool, len(own))
	for _, a := range own {
		takes[a] = true
	}
	owner := map[string]string{} // argument → the first sibling op documenting it
	for _, sib := range SchemaEnum(t.InputSchema(), "op") {
		if sib == op {
			continue
		}
		args, _ := idx.HelpArguments(t.Name() + "/" + sib)
		for _, a := range args {
			if _, seen := owner[a]; !seen {
				owner[a] = sib
			}
		}
	}
	var stray []string
	for k := range in {
		if k != "op" && !takes[k] && owner[k] != "" {
			stray = append(stray, k)
		}
	}
	if len(stray) == 0 {
		return Result{}, false
	}
	sort.Strings(stray)

	var b strings.Builder
	for i, k := range stray {
		if i == 0 {
			fmt.Fprintf(&b, "%s %s: ", t.Name(), op)
		} else {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "argument %q is not one %s takes (it belongs to %s).", k, op, owner[k])
		if s := ownNearArgument(k, own); s != "" {
			fmt.Fprintf(&b, " Did you mean %q?", s)
		}
	}
	b.WriteString(" Nothing was done.")
	if len(own) == 0 {
		fmt.Fprintf(&b, " %s takes no arguments besides op.", op)
	} else {
		fmt.Fprintf(&b, " %s takes: %s.", op, strings.Join(own, ", "))
	}
	return Result{Text: b.String(), IsError: true}, true
}

// ownNearArgument suggests the op's own argument a sibling's argument was
// probably meant as: a respelling (see nearArgument), or the same name with a
// suffix, as `parent` for `parent_id`.
func ownNearArgument(k string, own []string) string {
	set := make(map[string]bool, len(own))
	for _, a := range own {
		set[a] = true
	}
	if s := nearArgument(k, set); s != "" {
		return s
	}
	var hit []string
	for _, a := range own {
		if strings.HasPrefix(a, k+"_") {
			hit = append(hit, a)
		}
	}
	if len(hit) == 1 {
		return hit[0]
	}
	return ""
}

// schemaProperties returns the set of top-level property names, and whether
// the schema allows properties beyond them. A schema that does not parse, or
// has no properties, reports open so nothing is refused on its account.
func schemaProperties(schema json.RawMessage) (map[string]bool, bool) {
	var s struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return nil, true
	}
	if s.AdditionalProperties != nil && *s.AdditionalProperties {
		return nil, true
	}
	out := make(map[string]bool, len(s.Properties))
	for k := range s.Properties {
		out[k] = true
	}
	return out, false
}

// nearArgument returns the declared argument that k is a respelling of —
// the same letters once case and separators are ignored (chunkId, chunk-id,
// Chunk_ID → chunk_id) — or "".
func nearArgument(k string, props map[string]bool) string {
	norm := func(s string) string {
		return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(s))
	}
	want := norm(k)
	var hit []string
	for p := range props {
		if norm(p) == want {
			hit = append(hit, p)
		}
	}
	if len(hit) == 1 {
		return hit[0]
	}
	return ""
}
