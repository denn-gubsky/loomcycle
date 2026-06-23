package config

import (
	"fmt"
	"os"

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
	for i := 0; i+1 < len(overlay.Content); i += 2 {
		k := overlay.Content[i]
		ov := overlay.Content[i+1]
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
