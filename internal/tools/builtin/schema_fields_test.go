package builtin

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// jsonFieldNames returns the top-level JSON keys a struct decodes, following
// embedded structs the way encoding/json does. Unexported fields and `json:"-"`
// are skipped: the wire can never set them.
func jsonFieldNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			out = append(out, jsonFieldNames(f.Type)...)
			continue
		}
		if !f.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

// Every argument a builtin decodes is one its schema declares.
//
// A documented tool refuses an argument its schema does not declare, before it
// runs. So a field the input struct reads but the schema omits is not merely
// undocumented: every call that passes it is refused. That is how the memory
// consolidator's transcript-path fact writes — upsert_chunk with
// source_session_id — started failing once the refusal shipped: the handler
// read the field, the schema never listed it.
func TestBuiltinSchemas_DeclareEveryDecodedArgument(t *testing.T) {
	cases := []struct {
		tool  tools.Tool
		input any
	}{
		{&Document{}, docInput{}},
		{&Memory{}, memoryInput{}},
		{&History{}, historyInput{}},
		{&Path{}, pathInput{}},
		{&Channel{}, channelInput{}},
		{&Context{}, contextInput{}},
		{&AgentTool{}, agentInput{}},
		{&SkillTool{}, skillInput{}},
		{&Recall{}, recallInput{}},
		{&Interruption{}, interruptionInput{}},
		{&Evaluation{}, evaluationInput{}},
		{&Grep{}, grepInput{}},
		{&Glob{}, globInput{}},
		{&NotebookEdit{}, notebookEditInput{}},
	}
	for _, c := range cases {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(c.tool.InputSchema(), &s); err != nil {
			t.Fatalf("%s schema: %v", c.tool.Name(), err)
		}
		var missing []string
		for _, name := range jsonFieldNames(reflect.TypeOf(c.input)) {
			if _, ok := s.Properties[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s decodes %v but its schema does not declare them — a call passing any of them is refused before it runs", c.tool.Name(), missing)
		}
	}
}
