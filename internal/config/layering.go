package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Layer is one source for the config-layering merge. Name is the source label
// used in errors and the override log (a file path, or an embedded-unit name
// like "base"/"document-agent" — RFC AQ). Data, when non-nil, is the layer's
// raw bytes (an in-memory embedded preset/bundle); when nil, Name is read from
// disk. An in-memory layer with Data == nil and Name == "" is the no-yaml
// sentinel and is dropped by the caller.
type Layer struct {
	Name string
	Data []byte
	// Embedded marks a preset or bundle compiled into the binary. Only such a
	// layer may carry a top-level `include:` (the assembler has already
	// expanded it into the layers before this one); in any other layer the key
	// is refused. See includeKey.
	Embedded bool
	// Primary marks the one operator file whose top-level `presets:` the layer
	// assembly has read and acted on. The merge drops the key there and
	// refuses it in every other layer. See presetsKey.
	Primary bool
	// Warnings are advisories the assembler has about this layer (a selection
	// in it that the environment overrode). The load reports them with its
	// own, so boot, `validate` and `doctor` all print them.
	Warnings []string
}

// presetsKey is the top-level key by which the operator's primary config file
// selects embedded presets and bundles: the same names, expanded the same
// way, as LOOMCYCLE_PRESETS, which wins when it is set. Like includeKey it is
// not a setting: the layer assembly reads it before the merge (the presets go
// under the file that names them), and the merge drops it.
//
// It is read from one file only: the last --config, else the discovered
// loomcycle.yaml, else LOOMCYCLE_CONFIG_DIR/loomcycle.yaml. In any other layer
// it is refused, so which presets are layered never depends on layer order,
// and a file loaded without the assembly cannot drop the key and run with no
// preset at all.
//
// Trust: a preset named here is trusted exactly as the file that names it.
// A preset is yaml, and yaml carries agents, MCP servers and sign-in clients.
// What no yaml layer can do is turn on a switch that exists only in the
// environment (Bash, dynamic stdio MCP, privileged MCP tools, the HTTP host
// allowlist, the operator-key restriction, the access-control mode, open
// mode). The shipped units are held to a closed list of keys by
// internal/embedded's TestUnits_EveryUnitSetsOnlyKeysAPresetMayCarry.
const presetsKey = "presets"

func errPresetsKeyElsewhere(layer string) error {
	return fmt.Errorf("%s: a top-level `presets:` is read only from the primary config file (the last --config, else the discovered loomcycle.yaml, else loomcycle.yaml in LOOMCYCLE_CONFIG_DIR); move the key there, or select with LOOMCYCLE_PRESETS. (If this is that file, the key was not readable before ${VAR} expansion: write `presets:` as a plain top-level list)", layer)
}

// ReadPresetsKey returns the preset names a config file's top-level
// `presets:` lists, and whether the key is there at all. The layer assembly
// calls it on the primary file before it builds the stack. A file that does
// not read or parse is reported absent: the load fails on it as it always
// has, with its own message. A file that parses only once its ${VAR}s are
// expanded is read as absent here; the assembly then does not mark it
// primary, and the merge refuses the key instead of dropping it.
func ReadPresetsKey(path string) (names []string, present bool, err error) {
	// #nosec G304 -- path is the operator's own primary config file, chosen by the layer assembly from --config, the discovery paths or LOOMCYCLE_CONFIG_DIR
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return nil, false, nil
	}
	// The raw text, with no ${VAR} expansion. This runs before the load, and
	// so before the load's scan that refuses to interpolate a declared
	// principal's bearer (denyDeclaredPrincipalBearers): expanding here would
	// carry such a value into the unknown-preset error and the advisories. A
	// preset name is a fixed word, so there is nothing to interpolate.
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 {
		return nil, false, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	i := mapValueIndex(root, presetsKey)
	if i < 0 {
		return nil, false, nil
	}
	v := root.Content[i]
	if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
		return nil, true, nil // `presets:` with nothing under it selects none
	}
	if v.Kind != yaml.SequenceNode {
		return nil, true, fmt.Errorf("presets: in %s: must be a list of preset/bundle names, e.g. `presets: [personal]`", path)
	}
	for _, item := range v.Content {
		name := strings.TrimSpace(item.Value)
		if item.Kind != yaml.ScalarNode || name == "" {
			return nil, true, fmt.Errorf("presets: in %s: must be a list of preset/bundle names; it has an empty or non-text entry", path)
		}
		// The entry is not quoted back: it may name a secret's variable.
		if strings.Contains(name, "${") {
			return nil, true, fmt.Errorf("presets: in %s: an entry uses ${...}; preset names are written out, they are not read from the environment", path)
		}
		names = append(names, name)
	}
	return names, true, nil
}

// includeKey is the top-level key an embedded preset uses to name the presets
// and bundles layered under it. It is not a config setting: the layer assembly
// expands it before the merge, and the merge drops it.
//
// An operator's own file may not use it. The schema ignores an unknown
// top-level key without a word, so an `include: [base]` in a loomcycle.yaml
// would load nothing and say nothing, and the deployment would run without the
// routing it believed it had selected.
const includeKey = "include"

func errOperatorInclude(layer string) error {
	return fmt.Errorf("%s: a top-level `include:` works only inside a preset built into the binary; in your own config, select presets with LOOMCYCLE_PRESETS (or --preset) and add files with --config or LOOMCYCLE_CONFIG_DIR, then remove the key", layer)
}

// takeAssemblyKeys handles the two top-level keys that belong to the layer
// assembly, not to the config: it refuses each where it may not appear and
// removes it where it may, so neither reaches the merged tree.
func takeAssemblyKeys(l Layer, root *yaml.Node) error {
	if i := mapValueIndex(root, includeKey); i >= 0 {
		if !l.Embedded {
			return errOperatorInclude(l.Name)
		}
		root.Content = append(root.Content[:i-1], root.Content[i+1:]...)
	}
	if i := mapValueIndex(root, presetsKey); i >= 0 {
		if !l.Primary {
			return errPresetsKeyElsewhere(l.Name)
		}
		root.Content = append(root.Content[:i-1], root.Content[i+1:]...)
	}
	return nil
}

// refuseAssemblyKeys is takeAssemblyKeys for a file decoded straight into the
// Config, where an unknown key is ignored and so needs no removal. A document
// that does not parse is left to the caller's own decode to report.
func refuseAssemblyKeys(l Layer, expanded string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(expanded), &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	if root := doc.Content[0]; root.Kind == yaml.MappingNode {
		return takeAssemblyKeys(l, root)
	}
	return nil
}

// RFC AQ §3 — opt-in sequence-merge tags. A sequence in an overlay tagged
// !prepend merges its items IN FRONT of the accumulated sequence; !append merges
// them AFTER; an untagged sequence replaces wholesale (RFC AN, unchanged).
const (
	tagPrepend = "!prepend"
	tagAppend  = "!append"
	tagSeq     = "!!seq"
	tagMap     = "!!map"
)

// RFC AN + RFC AQ — config layering. mergeLayers reads + env-expands + deep-merges
// N config layers at the YAML-tree level (before typed unmarshal, so a key's
// PRESENCE is explicit — no Go zero-value / omitempty ambiguity). Layers merge
// left→right, last layer wins; it returns the merged root MAPPING node plus a list
// of every leaf a later layer REPLACED (for the startup override log / strict-mode
// fatal).
//
// The merge runs over yaml.Node (not map[string]any) because a map decode DISCARDS
// each sequence's YAML tag — and RFC AQ's !prepend/!append composition needs that
// tag (§3.3). A layer's bytes come from Layer.Data (an embedded preset/bundle) when
// set, else from reading Layer.Name as a file path.
//
// One recursive rule covers every section (no per-section special-casing):
// mapping ⊕ mapping → merge keys recursively; an overlay sequence tagged
// !prepend/!append ⊕ a base sequence → compose (RFC AQ); anything else (scalar,
// untagged sequence, type mismatch) → the later layer's value replaces. So
// agents/models/skills/volumes/mcp_servers/channels/... merge by key (a same-named
// entry field-merges, matching the mergeAgentDef precedent); provider_priority /
// tier candidate lists replace wholesale UNLESS tagged; struct sections merge
// field-by-field.
//
// Each layer keeps its OWN expandEnv raw-text stage, so a later layer can't inject
// ${ENV} into an earlier layer's text (no cross-layer interpolation surprises).
func mergeLayers(layers []Layer) (*yaml.Node, []string, error) {
	merged := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	var overrides []string
	for _, l := range layers {
		raw := l.Data
		if raw == nil {
			b, err := os.ReadFile(l.Name)
			if err != nil {
				return nil, nil, fmt.Errorf("read %s: %w", l.Name, err)
			}
			raw = b
		}
		expanded := expandEnv(string(raw))
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(expanded), &doc); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", l.Name, err)
		}
		if len(doc.Content) == 0 {
			continue // empty / all-comments layer
		}
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode {
			return nil, nil, fmt.Errorf("parse %s: top-level YAML must be a mapping", l.Name)
		}
		if err := takeAssemblyKeys(l, root); err != nil {
			return nil, nil, err
		}
		mergeNodes(merged, root, l.Name, "", &overrides)
	}
	return merged, overrides, nil
}

// mergeConfigFiles is the file-path, map-returning entry point retained for the
// historical callers/tests; it merges via mergeLayers and decodes the node tree
// to map[string]any.
func mergeConfigFiles(files []string) (map[string]any, []string, error) {
	layers := make([]Layer, len(files))
	for i, f := range files {
		layers[i] = Layer{Name: f}
	}
	node, overrides, err := mergeLayers(layers)
	if err != nil {
		return nil, nil, err
	}
	m := map[string]any{}
	if err := node.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("decode merged tree: %w", err)
	}
	return m, overrides, nil
}

// mergeNodes folds the overlay mapping node onto base (mutated in place). overrides
// accumulates the dotted path of every leaf the overlay REPLACED with a different
// value — a real cross-layer conflict. Adding a new key, re-setting a key to the
// same value, or an opt-in !prepend/!append sequence merge are NOT conflicts. Both
// nodes are MappingNodes (Content is a flat [key,value,key,value,...] list).
func mergeNodes(base, overlay *yaml.Node, file, prefix string, overrides *[]string) {
	// A layer that supplies BOTH halves of a mutually-exclusive pair is an error in
	// that one file, and validation must keep catching it — so the sibling-drop
	// below is suppressed in that case rather than silently letting the
	// last-listed key win.
	ovSetsBothExclusive := mapValueIndex(overlay, "model") >= 0 && mapValueIndex(overlay, "model_pattern") >= 0

	for i := 0; i+1 < len(overlay.Content); i += 2 {
		k := overlay.Content[i]
		ov := overlay.Content[i+1]
		if !ovSetsBothExclusive {
			dropExclusiveSibling(base, k.Value)
		}
		path := k.Value
		if prefix != "" {
			path = prefix + "." + k.Value
		}
		bi := mapValueIndex(base, k.Value)
		if bi < 0 {
			// New key — take it. A tagged sequence with no base to merge against is
			// just its items (strip the tag so the final decode doesn't choke on it).
			normalizeSeqTag(ov)
			base.Content = append(base.Content, k, ov)
			continue
		}
		bv := base.Content[bi]
		switch {
		case bv.Kind == yaml.MappingNode && ov.Kind == yaml.MappingNode:
			mergeNodes(bv, ov, file, path, overrides)
		case ov.Kind == yaml.SequenceNode && (ov.Tag == tagPrepend || ov.Tag == tagAppend) && bv.Kind == yaml.SequenceNode:
			// RFC AQ opt-in sequence merge — a deliberate compose, not a conflict.
			base.Content[bi] = mergeSeq(bv, ov, ov.Tag)
		default:
			// scalar / untagged sequence / type-mismatch → later replaces. Record
			// it as a conflict only when the value actually changes.
			normalizeSeqTag(ov)
			if !nodeEqual(bv, ov) {
				*overrides = append(*overrides, fmt.Sprintf("%s (set by %s, overriding an earlier layer)", path, file))
			}
			base.Content[bi] = ov
		}
	}
}

// modelRefExclusive pairs the two mutually-exclusive keys of a `models:` alias
// (ModelRef): a concrete `model` or a `model_pattern` glob, never both.
//
// It is safe to key on the NAMES alone rather than on the path, because
// model_pattern appears on exactly one type in the schema — a mapping carrying
// both keys is always a model alias.
var modelRefExclusive = map[string]string{
	"model":         "model_pattern",
	"model_pattern": "model",
}

// dropExclusiveSibling removes key's mutually-exclusive partner from base, so a
// later layer's choice wins outright instead of landing beside the earlier one.
//
// Without it, overriding a preset's `model_pattern:` alias with a plain `model:`
// produces a mapping carrying both and config load fails "exactly one of model or
// model_pattern is required" — an error that names neither the layer that set the
// pattern nor the one that overrode it, so an operator who wrote only `model:` has
// nothing to go on. Later-layer-wins is what layering already promises for a
// scalar; this extends it to a pair where winning means replacing the other
// spelling, not sitting next to it.
func dropExclusiveSibling(base *yaml.Node, key string) {
	sibling, ok := modelRefExclusive[key]
	if !ok {
		return
	}
	i := mapValueIndex(base, sibling)
	if i < 0 {
		return
	}
	// mapValueIndex returns the VALUE index; the key node is the one before it.
	base.Content = append(base.Content[:i-1], base.Content[i+1:]...)
}

// mapValueIndex returns the index of key's VALUE node within a MappingNode's
// Content, or -1 if the key is absent.
func mapValueIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

// mergeSeq composes two sequence nodes per the overlay's tag: !prepend puts the
// overlay's items in front of base's, !append after; duplicate items (deep-equal)
// are dropped keeping the FIRST occurrence — so !prepend of a re-listed provider
// promotes it and drops the lower copy. The result is a plain (!!seq) sequence.
func mergeSeq(base, overlay *yaml.Node, tag string) *yaml.Node {
	var ordered []*yaml.Node
	if tag == tagPrepend {
		ordered = append(ordered, overlay.Content...)
		ordered = append(ordered, base.Content...)
	} else {
		ordered = append(ordered, base.Content...)
		ordered = append(ordered, overlay.Content...)
	}
	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
	for _, n := range ordered {
		dup := false
		for _, kept := range out.Content {
			if nodeEqual(kept, n) {
				dup = true
				break
			}
		}
		if !dup {
			out.Content = append(out.Content, n)
		}
	}
	return out
}

// normalizeSeqTag clears a !prepend/!append tag from a sequence node that is taken
// as-is (a new key, or a replace) rather than merged — so the leftover custom tag
// never reaches the final decode (which would fail to resolve it).
func normalizeSeqTag(n *yaml.Node) {
	if n.Kind == yaml.SequenceNode && (n.Tag == tagPrepend || n.Tag == tagAppend) {
		n.Tag = tagSeq
	}
}

// nodeEqual reports deep value-equality of two YAML nodes (kind + scalar
// value/tag, recursing into collections). Used for sequence de-dup and for the
// "same value re-set isn't a conflict" rule.
func nodeEqual(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind {
		return false
	}
	if a.Kind == yaml.ScalarNode {
		return a.Value == b.Value && a.Tag == b.Tag
	}
	if len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		if !nodeEqual(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}
