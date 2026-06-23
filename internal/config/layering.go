package config

import (
	"fmt"
	"os"
	"reflect"

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

// RFC AN — config layering. mergeLayers reads + env-expands + deep-merges N
// config layers at the YAML-tree level (before typed unmarshal, so a key's
// PRESENCE is explicit — no Go zero-value / omitempty ambiguity). Layers merge
// left→right, last layer wins; it returns the merged tree plus a list of every
// leaf a later layer REPLACED (for the startup override log / strict-mode fatal).
//
// A layer's bytes come from Layer.Data (an embedded preset/bundle — RFC AQ) when
// set, else from reading Layer.Name as a file path. Either way the rest of the
// fold is identical, so an embedded preset and a disk file compose exactly alike.
//
// One recursive rule covers every section (no per-section special-casing):
// mapping ⊕ mapping → merge keys recursively; anything else (scalar, sequence,
// type mismatch) → the later layer's value replaces. So agents/models/skills/
// volumes/mcp_servers/channels/... merge by key (a same-named entry field-merges,
// matching the mergeAgentDef precedent); provider_priority/context_plugins
// (sequences) replace wholesale; struct sections (defaults/concurrency/...) merge
// field-by-field.
//
// Each layer keeps its OWN expandEnv raw-text stage, so a later layer can't inject
// ${ENV} into an earlier layer's text (no cross-layer interpolation surprises).
func mergeLayers(layers []Layer) (map[string]any, []string, error) {
	merged := map[string]any{}
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
		var tree map[string]any
		if err := yaml.Unmarshal([]byte(expanded), &tree); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", l.Name, err)
		}
		if tree == nil {
			continue // empty / all-comments layer
		}
		merged = mergeTree(merged, tree, l.Name, "", &overrides)
	}
	return merged, overrides, nil
}

// mergeConfigFiles is the file-path-only entry point retained for the historical
// callers/tests; it adapts paths to Layer values and delegates to mergeLayers.
func mergeConfigFiles(files []string) (map[string]any, []string, error) {
	layers := make([]Layer, len(files))
	for i, f := range files {
		layers[i] = Layer{Name: f}
	}
	return mergeLayers(layers)
}

// mergeTree folds overlay onto base in place and returns base. overrides
// accumulates the dotted path of every leaf the overlay REPLACED with a
// different value (a real cross-layer conflict — adding a brand-new key, or
// re-setting a key to the same value, is not a conflict). file is the overlay's
// source, named in the override record.
func mergeTree(base, overlay map[string]any, file, prefix string, overrides *[]string) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, ov := range overlay {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if bv, exists := base[k]; exists {
			bm, bIsMap := bv.(map[string]any)
			om, oIsMap := ov.(map[string]any)
			if bIsMap && oIsMap {
				// Two mappings → recurse (merge keys); no conflict at this level.
				base[k] = mergeTree(bm, om, file, path, overrides)
				continue
			}
			// Scalar / sequence / type-mismatch → later replaces. Record it as a
			// conflict only when the value actually changes.
			if !reflect.DeepEqual(bv, ov) {
				*overrides = append(*overrides, fmt.Sprintf("%s (set by %s, overriding an earlier layer)", path, file))
			}
		}
		base[k] = ov
	}
	return base
}
