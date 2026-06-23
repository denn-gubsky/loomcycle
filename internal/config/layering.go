package config

import (
	"fmt"
	"os"
	"reflect"

	"gopkg.in/yaml.v3"
)

// RFC AN — config layering. mergeConfigFiles reads + env-expands + deep-merges N
// config files at the YAML-tree level (before typed unmarshal, so a key's
// PRESENCE is explicit — no Go zero-value / omitempty ambiguity). Files merge
// left→right, last layer wins; it returns the merged tree plus a list of every
// leaf a later layer REPLACED (for the startup override log / strict-mode fatal).
//
// One recursive rule covers every section (no per-section special-casing):
// mapping ⊕ mapping → merge keys recursively; anything else (scalar, sequence,
// type mismatch) → the later layer's value replaces. So agents/models/volumes/
// mcp_servers/channels/... merge by key (a same-named entry field-merges, matching
// the mergeAgentDef precedent); provider_priority/context_plugins (sequences)
// replace wholesale; struct sections (defaults/concurrency/...) merge field-by-field.
//
// Each file keeps its OWN expandEnv raw-text stage, so a later layer can't inject
// ${ENV} into an earlier layer's text (no cross-layer interpolation surprises).
func mergeConfigFiles(files []string) (map[string]any, []string, error) {
	merged := map[string]any{}
	var overrides []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", f, err)
		}
		expanded := expandEnv(string(raw))
		var tree map[string]any
		if err := yaml.Unmarshal([]byte(expanded), &tree); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", f, err)
		}
		if tree == nil {
			continue // empty / all-comments file
		}
		merged = mergeTree(merged, tree, f, "", &overrides)
	}
	return merged, overrides, nil
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
