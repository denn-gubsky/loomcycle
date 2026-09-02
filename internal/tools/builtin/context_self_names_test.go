package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestContextSelf_ReportsTheOwnersDeclaredNames: an agent recording facts about people must
// be able to tell a fact about the OWNER of this memory from a fact about someone else in the
// same conversation — and it cannot, from a transcript alone.
//
// Measured going wrong: an extraction prompt said "a fact about THEM takes the subject
// `user`" without ever saying who THEM was, so on a two-speaker corpus the model picked the
// more prominent speaker. The owner's facts were filed as a third party's and the other
// speaker's as the owner's — the self-guard exactly inverted.
func TestContextSelf_ReportsTheOwnersDeclaredNames(t *testing.T) {
	d, dctx, _ := documentFixture(t)
	md := strings.Join([]string{
		"# User Profile", "", "## Identity", "",
		"- `@name` Dave", "- `@alias` dave", "- `@alias` caroline", "",
	}, "\n")
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, dctx, `{"op":"import_md","scope":"user","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	id, _ := out["document_id"].(string)
	pj, _ := json.Marshal(memrank.UserRootPath)
	if _, r := docExec(t, d, dctx,
		`{"op":"set_path","scope":"user","id":"`+id+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}

	c := &Context{Store: d.Store, SqlMem: d.SqlMem}
	res, err := c.Execute(dctx, json.RawMessage(`{"op":"self"}`))
	if err != nil || res.IsError {
		t.Fatalf("op=self: %v %s", err, res.Text)
	}
	var got struct {
		SelfNames []string `json:"self_names"`
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Text)
	}
	// "dave" collapses into "Dave" — ParseSelfNames dedupes case-insensitively, which is
	// why a distinct alias is needed to prove more than one is carried.
	if len(got.SelfNames) != 2 || got.SelfNames[0] != "Dave" || got.SelfNames[1] != "caroline" {
		t.Errorf("self_names = %v, want [Dave caroline]", got.SelfNames)
	}
}

// With no profile — or an unedited one — op=self must report NOTHING rather than a guess. A
// caller that receives nothing must not claim to know who the owner is, which is the whole
// reason the field is omitted instead of empty.
func TestContextSelf_OmitsNamesWhenNobodyDeclaredAny(t *testing.T) {
	d, dctx, _ := documentFixture(t)
	c := &Context{Store: d.Store, SqlMem: d.SqlMem}

	// (a) no profile at all
	res, _ := c.Execute(dctx, json.RawMessage(`{"op":"self"}`))
	if strings.Contains(res.Text, "self_names") {
		t.Errorf("no profile, yet names were reported: %s", res.Text)
	}

	// (b) the SHIPPED template, unedited — its example is indented, so it declares nobody
	mj, _ := json.Marshal(memrank.UserRootTemplate())
	out, r := docExec(t, d, dctx, `{"op":"import_md","scope":"user","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	id, _ := out["document_id"].(string)
	pj, _ := json.Marshal(memrank.UserRootPath)
	if _, r := docExec(t, d, dctx,
		`{"op":"set_path","scope":"user","id":"`+id+`","path":`+string(pj)+`}`); r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	res, _ = c.Execute(dctx, json.RawMessage(`{"op":"self"}`))
	if strings.Contains(res.Text, "self_names") {
		t.Errorf("the unedited template named somebody — the indented example was read as a "+
			"declaration: %s", res.Text)
	}
}

var _ = context.Background
var _ = tools.RunIdentity
