package builtin

import (
	"encoding/json"
	"slices"
	"testing"
)

// The read-only classification the off-run routes apply to a member acting on
// another subject's scope: every op docACLOps asks `write`, `delete`, `link`
// or `propose` of is a write, and so are the read-asked ops that write.
func TestDocumentCallReadsOnly_OnlyOpsThatChangeNothing(t *testing.T) {
	for op, rule := range docACLOps {
		in, _ := json.Marshal(map[string]string{"op": op})
		if got := DocumentCallReadsOnly(in); rule.verb != "read" && got {
			t.Errorf("%s asks %s but is classified read-only", op, rule.verb)
		}
	}
	for in, want := range map[string]bool{
		`{"op":"export_md"}`:                                true,
		`{"op":"get_document"}`:                             true,
		`{"op":"search"}`:                                   true,
		`{"op":"update_chunk"}`:                             false,
		`{"op":"delete_document"}`:                          false,
		`{"op":"link_chunks"}`:                              false,
		`{"op":"copy_documents","dry_run":true}`:            false,
		`{"op":"move_documents"}`:                           false,
		`{"op":"judge_statements"}`:                         false,
		`{"op":"classify"}`:                                 false,
		`{"op":"classify","write":"propose"}`:               false,
		`{"op":"classify","write":"report"}`:                true,
		`{"op":"resolve_anchor"}`:                           true,
		`{"op":"find_duplicates"}`:                          true,
		`{"op":"find_duplicates","propose":false}`:          true,
		`{"op":"find_duplicates","propose":true}`:           false,
		`{"op":"resolve_anchor","create_placeholder":true}`: false,
		`{"op":"no_such_op"}`:                               false,
		`{}`:                                                false,
		`[]`:                                                false,
	} {
		if got := DocumentCallReadsOnly(json.RawMessage(in)); got != want {
			t.Errorf("DocumentCallReadsOnly(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestPathCallReadsOnly_OnlyResolveLsStat(t *testing.T) {
	for in, want := range map[string]bool{
		`{"op":"resolve"}`: true, `{"op":"ls"}`: true, `{"op":"stat"}`: true,
		`{"op":"mkdir"}`: false, `{"op":"mv"}`: false, `{"op":"rm"}`: false,
		`{"op":""}`: false, `{}`: false, `"ls"`: false,
	} {
		if got := PathCallReadsOnly(json.RawMessage(in)); got != want {
			t.Errorf("PathCallReadsOnly(%s) = %v, want %v", in, got, want)
		}
	}
}

// opEnum is the op enum of a tool's input schema: every op it dispatches.
func opEnum(t *testing.T, schema string) []string {
	t.Helper()
	var sc struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema), &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.Properties.Op.Enum) == 0 {
		t.Fatal("no op enum in the schema")
	}
	return sc.Properties.Op.Enum
}

// Every op the Document tool dispatches is classified once: on the read-only
// list or on the write list. A new op fails here until someone decides which,
// so it is never read-only by default.
func TestDocumentCallReadsOnly_EveryOpIsClassified(t *testing.T) {
	enum := opEnum(t, documentInputSchema)
	for _, op := range enum {
		_, read := documentReadOnlyOps[op]
		write := documentWriteOps[op]
		if read == write {
			t.Errorf("Document op %q: read-only=%v write=%v, want exactly one", op, read, write)
		}
		in, _ := json.Marshal(map[string]string{"op": op})
		if write && DocumentCallReadsOnly(in) {
			t.Errorf("write op %q is classified read-only", op)
		}
		if read && docACLOps[op].verb != "read" {
			t.Errorf("read-only op %q asks %q of the access-control rules", op, docACLOps[op].verb)
		}
	}
	for op := range documentReadOnlyOps {
		if !slices.Contains(enum, op) {
			t.Errorf("documentReadOnlyOps names %q, which the Document tool does not accept", op)
		}
	}
	for op := range documentWriteOps {
		if !slices.Contains(enum, op) {
			t.Errorf("documentWriteOps names %q, which the Document tool does not accept", op)
		}
	}
}

func TestPathCallReadsOnly_EveryOpIsClassified(t *testing.T) {
	enum := opEnum(t, pathInputSchema)
	for _, op := range enum {
		read, write := pathReadOnlyOps[op], pathWriteOps[op]
		if read == write {
			t.Errorf("Path op %q: read-only=%v write=%v, want exactly one", op, read, write)
		}
		in, _ := json.Marshal(map[string]string{"op": op})
		if got := PathCallReadsOnly(in); got != read {
			t.Errorf("PathCallReadsOnly(%s) = %v, want %v", op, got, read)
		}
	}
	for op := range pathReadOnlyOps {
		if !slices.Contains(enum, op) {
			t.Errorf("pathReadOnlyOps names %q, which the Path tool does not accept", op)
		}
	}
	for op := range pathWriteOps {
		if !slices.Contains(enum, op) {
			t.Errorf("pathWriteOps names %q, which the Path tool does not accept", op)
		}
	}
}
