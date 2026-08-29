package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// The in-band tool and the off-run HTTP search must decode `when` through the SAME
// parser, and the schema must advertise exactly the modes that parser accepts.
//
// This subsystem has shipped two production bugs from the opposite arrangement:
// parseSources and parseMemorySources drifting so sources:["notes"] meant different
// things on the two surfaces, and the recall projection drifting from search's.
// Both were invisible until a benchmark measured them. A predicate that decides
// which rows get DROPPED is the last place to hand-maintain two copies, so this
// test reads the enum out of the published schema and feeds it to the parser.
func TestMemoryWhen_SchemaEnumMatchesTheParser(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(memoryInputSchema), &schema); err != nil {
		t.Fatalf("memoryInputSchema is not valid JSON: %v", err)
	}
	when, ok := schema.Properties["when"]
	if !ok {
		t.Fatal("the input schema advertises no `when` property — an agent cannot use what it is not told about")
	}
	missing, ok := when.Properties["missing"]
	if !ok || len(missing.Enum) == 0 {
		t.Fatal("`when.missing` publishes no enum; the accepted modes must be discoverable")
	}
	for _, mode := range missing.Enum {
		if _, err := memrank.ParseObservedMissing(mode); err != nil {
			t.Errorf("schema advertises missing=%q but the parser refuses it: %v", mode, err)
		}
	}
	// And the converse: every mode the parser accepts, other than the empty default,
	// must be advertised. A mode that works but is undocumented is a mode nobody uses.
	for _, mode := range []string{string(memrank.MissingPrefer), string(memrank.MissingRequire)} {
		found := false
		for _, adv := range missing.Enum {
			if adv == mode {
				found = true
			}
		}
		if !found {
			t.Errorf("parser accepts missing=%q but the schema does not advertise it", mode)
		}
	}
}

// The schema must warn about the footgun. This is a documentation assertion on
// purpose: `require` with a tight window returns nothing from a store that holds
// the answer, and the only thing standing between an agent and that outcome is the
// description it reads.
func TestMemoryWhen_SchemaWarnsAboutTheHardFilter(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(memoryInputSchema), &schema); err != nil {
		t.Fatalf("memoryInputSchema is not valid JSON: %v", err)
	}
	desc := schema.Properties["when"].Description
	if !strings.Contains(desc, "require") || !strings.Contains(strings.ToLower(desc), "generous") {
		t.Errorf("`when` must tell the agent to give a GENEROUS window and what `require` costs; got: %s", desc)
	}
	if !strings.Contains(schema.Properties["observed_at"].Description, "Omit when you do not know") {
		t.Error("`observed_at` must tell the agent to omit rather than guess — a guessed date " +
			"hides the row from the window it truly belongs in")
	}
}
