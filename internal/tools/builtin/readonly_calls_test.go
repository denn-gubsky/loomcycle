package builtin

import (
	"encoding/json"
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
