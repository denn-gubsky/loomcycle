package teamgraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// keys.go — the keys of a definition, read as written.
//
// A definition is decoded into structs, and Go's decoder is generous about
// keys: it matches one to a field by ignoring case, lets the last of two keys
// for one field win, and drops a key no field takes. Every other reader of
// the same text — a diff, jq, an editor, a person — reads keys exactly. So a
// definition could be saved that is not the one that was read: a handler with
// "agent" and then "Agent" shows the first everywhere and runs the second; a
// misspelt "hoks" is stored as a team with no hooks.
//
// KeyIssues walks the TEXT of a definition beside the types it decodes into
// and reports each key the decoder would read differently from how it is
// written. It is applied to what an author sends (a create's or a fork's
// overlay), never to a stored definition: what is stored was written by the
// runtime's own encoder and has none of these.

// IssueKeyInvalid is the kind of a key issue: the caller's overlay is not
// acceptable as written. It is the kind TeamDef reports for an overlay that
// does not decode, so a client needs no new case for it.
const IssueKeyInvalid = "overlay_invalid"

// BodyKeys says how the keys of one kind of body under `local` are to be read.
// The types of those bodies live above this package (an agent's is the
// overlay AgentDef takes), so the caller that knows them supplies them.
type BodyKeys struct {
	// Type is the struct a body of this kind is decoded into.
	Type reflect.Type
	// UnknownRefusedElsewhere is set when the body's own decoder already
	// refuses a key it does not take, in its own words. Unknown keys are then
	// left to it; a repeated key or a key matched only by ignoring its case,
	// which that decoder lets through, is still reported here.
	UnknownRefusedElsewhere bool
}

// LocalBodyKeys maps the path of a `local` kind ("local.agents",
// "local.channels", "local.webhooks") to how its bodies' keys are read.
type LocalBodyKeys map[string]BodyKeys

var (
	unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	// Local decodes itself by hand, but its keys are exactly its fields'.
	walkedDespiteUnmarshaler = map[reflect.Type]bool{reflect.TypeOf(Local{}): true}

	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
	// An inline hook is written as an object but decoded by hand into Entry,
	// whose own fields are not its wire keys. Its keys are Inline's.
	keyTypeFor = map[reflect.Type]reflect.Type{
		reflect.TypeOf(hooks.Entry{}): reflect.TypeOf(hooks.Inline{}),
	}
)

// KeyIssues returns an issue for every key in raw that the definition's
// decoder would not read as written: a key repeated in one object, a key that
// names a field only when its case is ignored, two spellings of one field,
// and a key no field takes. Nil when there is none, or when raw is not JSON
// (Parse reports that).
//
// Field names are checked wherever the definition has typed fields. Where it
// holds free-form names — variable names, the names under `local.*`, layout
// nodes, colours, a vars state's `set` — the names are the author's own and
// only an exact repeat is an issue. Inside a value the definition does not
// type (an input schema, a local agent's body, a schedule's payload) only a
// repeated key is reported: those are decoded, and judged, by their own
// readers.
//
// bodies types the bodies under `local` that this package holds as raw JSON;
// without an entry a kind's bodies are checked for repeated keys only.
//
// def, when not nil, is raw as parsed; it supplies the state id for an issue
// inside a state's handler.
func KeyIssues(raw []byte, def *Definition, bodies LocalBodyKeys) []*Issue {
	w := &keyWalker{bodies: bodies}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := w.value(dec, reflect.TypeOf(Definition{}), ""); err != nil {
		return nil
	}
	for _, is := range w.out {
		m := handlerKeyRe.FindStringSubmatch(is.Path)
		if m == nil {
			continue
		}
		is.Field = m[2]
		if i, err := strconv.Atoi(m[1]); err == nil && def != nil && i < len(def.States) {
			is.State = def.States[i].ID
		}
	}
	return w.out
}

// notInADefinition explains, for a key an author is likely to write at the top
// of a definition out of habit, where it belongs instead: other definitions
// (an agent's, a hook's) do carry these in their bodies.
var notInADefinition = map[string]string{
	"name":        "a team's name is given beside the definition, not inside it",
	"description": "a version's description is given beside the definition, not inside it",
}

var handlerKeyRe = regexp.MustCompile(`^states\[(\d+)\]\.handler\.(.+)$`)

type keyWalker struct {
	out    issues
	bodies LocalBodyKeys
	// unknownOK is set while inside a body whose own decoder refuses unknown
	// keys (BodyKeys.UnknownRefusedElsewhere).
	unknownOK bool
}

func (w *keyWalker) add(path, format string, args ...any) {
	w.out.add(&Issue{Kind: IssueKeyInvalid, Path: path, Msg: fmt.Sprintf(format, args...)})
}

// inObject names the object a key is in, for a message.
func inObject(path string) string {
	if path == "" {
		return "the definition"
	}
	return path
}

// value consumes one JSON value, checking it against t (nil = untyped).
func (w *keyWalker) value(dec *json.Decoder, t reflect.Type, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar
	}
	switch delim {
	case '{':
		return w.object(dec, t, path)
	case '[':
		elem := elemType(t)
		for i := 0; dec.More(); i++ {
			if err := w.value(dec, elem, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ']'
		return err
	}
	return nil
}

// resolve strips pointers and applies the hand-decoded substitutions.
func resolve(t reflect.Type) reflect.Type {
	for t != nil {
		if sub, ok := keyTypeFor[t]; ok {
			t = sub
			continue
		}
		if t == rawMessageType {
			return nil
		}
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
			continue
		}
		// A type that decodes itself may read keys its fields do not show.
		// Unless it is known to read exactly them, only repeats are judged.
		if reflect.PointerTo(t).Implements(unmarshalerType) && !walkedDespiteUnmarshaler[t] {
			return nil
		}
		return t
	}
	return nil
}

func elemType(t reflect.Type) reflect.Type {
	t = resolve(t)
	if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
		return t.Elem()
	}
	return nil
}

// fieldsOf maps each wire key of struct t to its field's type.
func fieldsOf(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if tag == "-" {
			continue
		}
		if name == "" {
			if f.Anonymous && resolve(f.Type) != nil && resolve(f.Type).Kind() == reflect.Struct {
				for k, v := range fieldsOf(resolve(f.Type)) {
					out[k] = v
				}
				continue
			}
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// object consumes the rest of an object whose '{' was read.
func (w *keyWalker) object(dec *json.Decoder, t reflect.Type, path string) error {
	t = resolve(t)
	var fields map[string]reflect.Type // a struct's wire keys; nil otherwise
	var valueType reflect.Type         // a map's value type
	switch {
	case t == nil:
	case t.Kind() == reflect.Struct:
		fields = fieldsOf(t)
	case t.Kind() == reflect.Map:
		valueType = t.Elem()
	}
	// The bodies of a `local` kind: typed by the caller.
	if body, ok := w.bodies[path]; ok {
		valueType = body.Type
		if body.UnknownRefusedElsewhere && !w.unknownOK {
			w.unknownOK = true
			defer func() { w.unknownOK = false }()
		}
	}
	written := map[string]bool{} // keys seen, exactly as spelt
	spelt := map[string]string{} // field → the spelling it was first given
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		keyPath := PathKey(path, key)
		child := valueType
		switch {
		case written[key]:
			w.add(keyPath, "%s: key %q is written twice; the second would replace the first without a word. Write it once.", inObject(path), key)
			if fields != nil {
				child = fields[fieldFor(fields, key)]
			}
		case fields != nil:
			field := fieldFor(fields, key)
			switch {
			case field == "" && w.unknownOK:
			case field == "":
				hint := didYouMean(fields, key)
				if path == "" && notInADefinition[key] != "" {
					hint = " — " + notInADefinition[key]
				}
				w.add(keyPath, "%s: unknown key %q%s", inObject(path), key, hint)
			case spelt[field] != "":
				w.add(keyPath, "%s: keys %q and %q are both read as %q, and the second would replace the first without a word. Write it once, as %q.",
					inObject(path), spelt[field], key, field, field)
			case field != key:
				w.add(keyPath, "%s: key %q is read as %q only by ignoring its case. Write %q.", inObject(path), key, field, field)
			}
			if field != "" {
				child = fields[field]
				if spelt[field] == "" {
					spelt[field] = key
				}
			}
		}
		written[key] = true
		if err := w.value(dec, child, keyPath); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}

// fieldFor returns the wire key of the field Go's decoder would give key to:
// the exact name, else one equal under Unicode simple case folding. "" when
// no field takes it.
func fieldFor(fields map[string]reflect.Type, key string) string {
	if _, ok := fields[key]; ok {
		return key
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.EqualFold(name, key) {
			return name
		}
	}
	return ""
}

// didYouMean suggests the known key a misspelt one is closest to, when one is
// within two edits: " — did you mean \"hooks\"?". "" otherwise.
func didYouMean(fields map[string]reflect.Type, key string) string {
	best, bestDist := "", 3
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if d := editDistance(strings.ToLower(key), name); d < bestDist {
			best, bestDist = name, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" — did you mean %q?", best)
}

// editDistance is the Levenshtein distance between a and b, by rune.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
