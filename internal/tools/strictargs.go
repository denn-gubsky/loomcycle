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
		return Result{}, false
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
