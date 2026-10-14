package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestEvaluation_TypeScriptAdapterTypesMatchTheTool holds the TS adapter's
// Evaluation types to what the tool reads and writes. The adapter declares them
// by hand, so a field the tool adds, renames or drops is invisible from both
// sides — every Go test and every TS test still passes — until a consumer reads
// a field that is not there.
//
// Each side is derived, not restated: the ops and input fields from the tool's
// input schema, a record's fields from evalRowResponse, the aggregate's from
// the store's json tags.
func TestEvaluation_TypeScriptAdapterTypesMatchTheTool(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "adapters", "ts", "src", "types.ts"))
	if err != nil {
		t.Fatalf("read the TS adapter types: %v", err)
	}
	src := string(b)

	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evaluationInputSchema), &schema); err != nil {
		t.Fatalf("parse the evaluation input schema: %v", err)
	}

	// Ops: one TS input interface per op the schema serves, and no other.
	inputs := []string{"EvaluationSubmitInput", "EvaluationGetInput", "EvaluationListForRunInput", "EvaluationListForDefInput", "EvaluationAggregateInput"}
	opLiteral := regexp.MustCompile(`(?m)^\s+op: "([a-z_]+)";`)
	var tsOps []string
	for _, name := range inputs {
		body := tsInterfaceBody(t, src, name)
		m := opLiteral.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s has no `op` literal", name)
		}
		tsOps = append(tsOps, m[1])
		// Every field the adapter sends is one the tool reads.
		for _, f := range tsFields(body) {
			if _, ok := schema.Properties[f]; !ok {
				t.Errorf("%s.%s is not in the Evaluation tool's input schema — the tool ignores it", name, f)
			}
		}
	}
	schemaOps := append([]string(nil), schema.Properties["op"].Enum...)
	sort.Strings(tsOps)
	sort.Strings(schemaOps)
	if !reflect.DeepEqual(tsOps, schemaOps) {
		t.Errorf("TS evaluation ops %v, tool ops %v", tsOps, schemaOps)
	}

	// A stored evaluation, as get and the lists return it.
	var rowKeys []string
	for k := range evalRowResponse(store.EvaluationRow{}) {
		rowKeys = append(rowKeys, k)
	}
	sameFields(t, "EvaluationRecord", tsFields(tsInterfaceBody(t, src, "EvaluationRecord")), rowKeys)

	// aggregate's result and its statistics.
	sameFields(t, "EvaluationAggregate", tsFields(tsInterfaceBody(t, src, "EvaluationAggregate")), jsonTags(reflect.TypeOf(store.AggregateResult{})))
	sameFields(t, "EvaluationScoreStats", tsFields(tsInterfaceBody(t, src, "EvaluationScoreStats")), jsonTags(reflect.TypeOf(store.ScoreStats{})))
}

// tsInterfaceBody is the text between `export interface name {` and its
// closing brace at column 0.
func tsInterfaceBody(t *testing.T, src, name string) string {
	t.Helper()
	head := "export interface " + name + " {"
	start := strings.Index(src, head)
	if start < 0 {
		t.Fatalf("no %q in the TS adapter types", head)
	}
	rest := src[start+len(head):]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("%s has no closing brace", name)
	}
	return rest[:end]
}

var tsFieldRe = regexp.MustCompile(`(?m)^  ([a-z_]+)\??:`)

func tsFields(body string) []string {
	var out []string
	for _, m := range tsFieldRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func jsonTags(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

func sameFields(t *testing.T, what string, ts, goSide []string) {
	t.Helper()
	if len(ts) == 0 || len(goSide) == 0 {
		t.Fatalf("%s: nothing read (TS %v, Go %v) — the test is reading the wrong thing", what, ts, goSide)
	}
	a := append([]string(nil), ts...)
	b := append([]string(nil), goSide...)
	sort.Strings(a)
	sort.Strings(b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("%s: TS fields %v, the tool's %v", what, a, b)
	}
}
