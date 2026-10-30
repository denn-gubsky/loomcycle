package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// bundleFunction extracts one top-level `function name(` … `}` or `var name = {` … `};`
// from the bundle source by brace matching. The test reads the BUNDLE, never a copy of
// the JavaScript: a drift guard that carried its own copy would only prove the port
// matches the copy.
func bundleFunction(t *testing.T, src, start string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("the bundle no longer defines %q — update the port and this test together", start)
	}
	depth, opened := 0, false
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
			opened = true
		case '}':
			depth--
			if opened && depth == 0 {
				end := j + 1
				if end < len(src) && src[end] == ';' {
					end++
				}
				return src[i:end]
			}
		}
	}
	t.Fatalf("unbalanced braces after %q", start)
	return ""
}

// TestConsolidatorEntityKey_MatchesTheBundle (RFC FO decision 10f.3).
//
// Runs the consolidator's own entityKey — extracted from bundles/memory/loomcycle.yaml
// and evaluated in goja, the engine the consolidator runs in — on the same inputs as
// the Go port. An adopted ontology instance must get the key the consolidator would
// compute for the same subject, or the consolidator mints a duplicate node the first
// time it learns a fact about it.
func TestConsolidatorEntityKey_MatchesTheBundle(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "bundles", "memory", "loomcycle.yaml"))
	if err != nil {
		t.Skipf("cannot read the memory bundle (not a source checkout?): %v", err)
	}
	src := string(raw)
	var js strings.Builder
	for _, start := range []string{
		"var STOPWORDS = {", "function rawWords(", "function textFingerprint(",
		"function slug(", "function normalizeSubject(", "function entityKey(",
	} {
		js.WriteString(bundleFunction(t, src, start))
		js.WriteString("\n")
	}
	vm := goja.New()
	if _, err := vm.RunString(js.String()); err != nil {
		t.Fatalf("evaluate the bundle's functions: %v\n%s", err, js.String())
	}
	entityKey, ok := goja.AssertFunction(vm.Get("entityKey"))
	if !ok {
		t.Fatal("entityKey is not a function in the bundle")
	}

	type vec struct{ typ, subject string }
	vectors := []vec{
		{"organization", "Acme Bank"},
		{"organization", "  The   Acme  Bank "},
		{"person", "An Introduction"},
		{"OrganizationalUnit", "Compliance Department"},
		{"organization", "The Bank of the Republic of the United Kingdom of Great Britain and Northern Ireland"},
		{"object", "a b c d e f g h i j"},
		{"object", "Supercalifragilisticexpialidocious-and-a-very-long-hyphenated-name-indeed-yes"},
		{"organization", "Національний банк України"},
		{"organization", "НБУ"},
		{"organization", "日本銀行"},
		{"organization", "Bank 😀 of Emoji"},
		{"organization", "😀"},
		{"organization", "ΟΔΟΣ"},
		{"organization", "İSTANBUL"},
		{"organization", "Acme\u00a0Bank\u2003Ltd"},
		{"organization", "\ufeffAcme"},
		{"organization", ""},
		{"organization", "the"},
		{"organization", "of the and"},
		{"concept", "R&D / Ops (2026)"},
	}
	for _, v := range vectors {
		got, err := entityKey(goja.Undefined(), vm.ToValue(v.typ), vm.ToValue(v.subject))
		if err != nil {
			t.Fatalf("entityKey(%q, %q): %v", v.typ, v.subject, err)
		}
		if want, have := got.String(), ConsolidatorEntityKey(v.typ, v.subject); want != have {
			t.Errorf("entityKey(%q, %q): bundle %q, Go port %q", v.typ, v.subject, want, have)
		}
	}
}

// TestConsolidatorSlug_IsTheKeyWithoutItsType.
func TestConsolidatorSlug_IsTheKeyWithoutItsType(t *testing.T) {
	if got := ConsolidatorSlug("The Acme Bank"); "organization:"+got != ConsolidatorEntityKey("organization", "The Acme Bank") {
		t.Errorf("slug %q is not the key's tail", got)
	}
}
