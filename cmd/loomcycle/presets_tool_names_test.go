package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestBundlePrompts_NameToolsExactly.
//
// Tool dispatch is an exact-match map lookup (tools.Dispatcher.Execute does
// `d.tools[name]`), so a system prompt that instructs a model to call `document`
// when the registered tool is `Document` does not degrade — it fails outright with
// "tool not found: document" on the FIRST call, and the model then spends its whole
// budget reasoning about which tools it has.
//
// That is exactly what memory/ontologist did on a live deployment: three lowercase
// `document` op=… instructions, a failed first call, and 1,937 output tokens of the
// model concluding its tools were missing. The def granted the tool correctly; only
// the prompt's spelling was wrong, which is why nothing caught it — the ACL is
// checked, the prose is not.
//
// So: any bundle prompt that writes `name` op=… must spell a name some agent in that
// bundle is actually granted. Scoped to that backticked-name-plus-op shape rather
// than every mention of a word, because "ordinary document chunks" is legitimate
// prose in the same paragraph.
func TestBundlePrompts_NameToolsExactly(t *testing.T) {
	// The backticked call form the bundles use to teach a tool call.
	callForm := regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*)` op=")

	roots := []string{filepath.Join("..", "..", "bundles"), filepath.Join("embedded", "bundles")}
	checked := 0
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var doc struct {
				Agents map[string]struct {
					Tools        []string `yaml:"tools"`
					SystemPrompt string   `yaml:"system_prompt"`
				} `yaml:"agents"`
			}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return nil // not an agent bundle
			}
			for name, def := range doc.Agents {
				if def.SystemPrompt == "" {
					continue
				}
				granted := map[string]bool{}
				for _, tl := range def.Tools {
					granted[tl] = true
				}
				for _, m := range callForm.FindAllStringSubmatch(def.SystemPrompt, -1) {
					checked++
					if !granted[m[1]] {
						t.Errorf("%s: agent %q instructs `%s` op=… but its tools are %v — dispatch is "+
							"exact-match, so that call returns \"tool not found: %s\" and the agent "+
							"never gets started",
							filepath.Base(path), name, m[1], def.Tools, m[1])
					}
				}
			}
			return nil
		})
	}
	// Guard the guard: if the call form ever stops matching, this test would pass by
	// examining nothing at all.
	if checked == 0 {
		t.Fatal("no `name` op=… instructions found in any bundle prompt — the pattern no longer matches, so this test asserts nothing")
	}
	t.Logf("checked %d tool-call instructions across the bundles", checked)
}
