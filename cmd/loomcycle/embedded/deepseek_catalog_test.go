package embedded

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deepseekListedModels is what DeepSeek's GET /v1/models returned on
// 2026-10-02 — the provider's catalog, not a copy of any config. The resolver
// skips a tier candidate whose model the provider does not list, silently, so a
// shipped config naming an unlisted id has a dead candidate: in that
// deployment's routing view the cost floor read `available: false` and every
// tier-low run walked on to a paid fallback. When DeepSeek changes its catalog,
// update this list from a fresh probe, then fix whatever this test flags.
var deepseekListedModels = map[string]bool{
	"deepseek-flash":  true,
	"deepseek-v4-pro": true,
}

// TestShippedConfigs_NameOnlyDeepSeekModelsTheProviderLists walks every config
// the project ships (the embedded presets and bundles, the example config, and
// the repo's bundles/, examples/ and deploy/ trees) and requires each
// `provider: deepseek` entry to name a model DeepSeek lists.
func TestShippedConfigs_NameOnlyDeepSeekModelsTheProviderLists(t *testing.T) {
	sources := map[string][]byte{"embedded/loomcycle.example.yaml": ExampleYAML()}
	for _, u := range Units() {
		sources["embedded/"+u.Kind+"/"+u.Name] = u.Data
	}
	root := filepath.Join("..", "..", "..")
	for _, dir := range []string{"bundles", "examples", "deploy"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !(strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sources[p] = data
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	checked := 0
	for name, data := range sources {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc any
			if err := dec.Decode(&doc); err != nil {
				if !errors.Is(err, io.EOF) {
					// A compose template or similar that is not plain YAML holds no
					// provider wiring; the embedded units are parse-checked elsewhere.
					t.Logf("%s: skipped, not plain YAML: %v", name, err)
				}
				break
			}
			for _, model := range deepseekModels(doc) {
				checked++
				if !deepseekListedModels[model] {
					t.Errorf("%s names deepseek model %q, which DeepSeek does not list — the resolver skips it as a candidate", name, model)
				}
			}
		}
	}
	// The base preset alone wires DeepSeek, so finding nothing means the walk
	// read the wrong tree, not that every config is clean.
	if checked == 0 {
		t.Fatal("found no deepseek model in any shipped config; the walk is not reading the configs")
	}
}

// deepseekModels returns the model of every mapping in the tree that sets
// `provider: deepseek` — a models: alias, a tier candidate, or a pinned agent.
func deepseekModels(node any) []string {
	var out []string
	switch n := node.(type) {
	case map[string]any:
		if n["provider"] == "deepseek" {
			if m, ok := n["model"].(string); ok {
				out = append(out, m)
			}
		}
		for _, v := range n {
			out = append(out, deepseekModels(v)...)
		}
	case []any:
		for _, v := range n {
			out = append(out, deepseekModels(v)...)
		}
	}
	return out
}
