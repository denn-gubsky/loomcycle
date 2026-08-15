package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestDocumentOps_TypeScriptAdapterIsInParity.
//
// The TS adapter types `op` as a closed union, so an op missing from it is one a
// TypeScript consumer cannot call without a compile error — the runtime would accept it
// happily. Nothing linked the two lists, and they had drifted by eight ops before this
// test existed: three from the fact tier, `search` and `propose_entity` from before that,
// and two from remote document sources.
//
// The failure mode is what makes it worth a test rather than a habit. It is invisible
// from the Go side (every server test passes), invisible from the adapter side (the type
// is internally consistent), and only shows up as a consumer discovering their client
// cannot express something the server has supported for months.
func TestDocumentOps_TypeScriptAdapterIsInParity(t *testing.T) {
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(documentInputSchema), &schema); err != nil {
		t.Fatalf("parse the document input schema: %v", err)
	}
	if len(schema.Properties.Op.Enum) == 0 {
		t.Fatal("no ops in the schema — the test is reading the wrong thing")
	}

	path := filepath.Join("..", "..", "..", "adapters", "ts", "src", "types.ts")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the TS adapter types: %v", err)
	}
	// The union runs from the first op to the `scope?:` field that follows it. Scoped
	// that way rather than searching the whole file so an op name appearing in some
	// unrelated comment cannot make this pass.
	src := string(b)
	start := indexOrFail(t, src, `| "create_document"`)
	end := indexOrFail(t, src[start:], "scope?:") + start
	union := src[start:end]
	quoted := regexp.MustCompile(`"([a-z_]+)"`)
	inTS := map[string]bool{}
	for _, m := range quoted.FindAllStringSubmatch(union, -1) {
		inTS[m[1]] = true
	}

	for _, op := range schema.Properties.Op.Enum {
		if !inTS[op] {
			t.Errorf("Document op %q is served by the runtime but MISSING from the TS adapter's "+
				"op union — a TypeScript caller cannot express it. Add it to DocumentInput's `op` "+
				"in adapters/ts/src/types.ts", op)
		}
	}
}

func indexOrFail(t *testing.T, haystack, needle string) int {
	t.Helper()
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	t.Fatalf("could not find %q in the TS adapter types — the file's shape changed, so this "+
		"parity test is no longer reading the op union", needle)
	return 0
}
