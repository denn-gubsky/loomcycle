package builtin

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// A name that is a document AND has names beneath it: the one-level ls emits
// one entry per name (the document), so has_children is the only trace of
// the subtree.

type lsEntries struct {
	Entries []map[string]any `json:"entries"`
}

func lsEntry(t *testing.T, text, name string) map[string]any {
	t.Helper()
	var out lsEntries
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode ls: %v (%s)", err, text)
	}
	for _, e := range out.Entries {
		if e["name"] == name {
			return e
		}
	}
	t.Fatalf("ls has no entry %q: %s", name, text)
	return nil
}

func TestPathLs_DocumentWithDescendantsReportsHasChildren(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/a/", "b", "document")
	seedAgent(t, s, "/a/b/", "c", "document")

	_, res := pathExec(t, p, ctx, `{"op":"ls","path":"/a"}`)
	if res.IsError {
		t.Fatalf("ls /a: %s", res.Text)
	}
	b := lsEntry(t, res.Text, "b")
	if b["kind"] != "document" || b["has_children"] != true {
		t.Errorf("ls /a: want b kind=document has_children=true, got %v", b)
	}

	_, res = pathExec(t, p, ctx, `{"op":"ls","path":"/a/b"}`)
	if res.IsError {
		t.Fatalf("ls /a/b: %s", res.Text)
	}
	lsEntry(t, res.Text, "c")

	out, res := pathExec(t, p, ctx, `{"op":"stat","path":"/a/b"}`)
	if res.IsError {
		t.Fatalf("stat /a/b: %s", res.Text)
	}
	if out["has_children"] != true {
		t.Errorf("stat /a/b: want has_children=true, got %v", out)
	}
}

func TestPathLs_MkdirDirectoryWithDescendantsOmitsHasChildren(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/a/", "b", "directory")
	seedAgent(t, s, "/a/b/", "c", "document")

	_, res := pathExec(t, p, ctx, `{"op":"ls","path":"/a"}`)
	b := lsEntry(t, res.Text, "b")
	if b["kind"] != "directory" {
		t.Errorf("want kind=directory, got %v", b)
	}
	if _, ok := b["has_children"]; ok {
		t.Errorf("a directory entry must not carry has_children: %v", b)
	}
	out, _ := pathExec(t, p, ctx, `{"op":"stat","path":"/a/b"}`)
	if _, ok := out["has_children"]; ok {
		t.Errorf("stat of a directory must not carry has_children: %v", out)
	}
}

func TestPathLs_LeafDocumentOmitsHasChildren(t *testing.T) {
	p, ctx, s := pathFixture(t)
	seedAgent(t, s, "/a/", "b", "document")
	seedAgent(t, s, "/a/", "bb", "document")
	seedAgent(t, s, "/a/bb/", "c", "document") // a sibling's child must not leak onto b

	_, res := pathExec(t, p, ctx, `{"op":"ls","path":"/a"}`)
	if _, ok := lsEntry(t, res.Text, "b")["has_children"]; ok {
		t.Errorf("leaf document b must not carry has_children: %s", res.Text)
	}
	out, _ := pathExec(t, p, ctx, `{"op":"stat","path":"/a/b"}`)
	if _, ok := out["has_children"]; ok {
		t.Errorf("stat of a leaf document must not carry has_children: %v", out)
	}
}

// The flag is computed from the descendants the caller may READ, so it never
// reveals that hidden names exist beneath a visible document.
func TestPathLs_HasChildrenCountsOnlyReadableDescendants(t *testing.T) {
	d, base, st := documentFixture(t)
	seed := tenantDocCtx(base, nil)
	for _, path := range []string{"/kb/top", "/kb/top/x", "/kb/top/deep/y"} {
		if _, res := docExec(t, d, seed, `{"op":"create_document","scope":"tenant","title":"t","path":"`+path+`"}`); res.IsError {
			t.Fatalf("create_document %s: %s", path, res.Text)
		}
	}
	p := &Path{Store: st}

	denyAll := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/top/x"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/top/deep/y"}}))
	res := pathACLExec(t, p, denyAll, `{"op":"ls","scope":"tenant","path":"/kb"}`)
	if res.IsError {
		t.Fatalf("ls /kb: %s", res.Text)
	}
	if _, ok := lsEntry(t, res.Text, "top")["has_children"]; ok {
		t.Errorf("every descendant hidden: has_children must be absent: %s", res.Text)
	}
	res = pathACLExec(t, p, denyAll, `{"op":"stat","scope":"tenant","path":"/kb/top"}`)
	var st1 map[string]any
	_ = json.Unmarshal([]byte(res.Text), &st1)
	if _, ok := st1["has_children"]; ok || res.IsError {
		t.Errorf("stat, every descendant hidden: want no has_children, got %s", res.Text)
	}

	oneVisible := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/top/x"}}))
	res = pathACLExec(t, p, oneVisible, `{"op":"ls","scope":"tenant","path":"/kb"}`)
	if lsEntry(t, res.Text, "top")["has_children"] != true {
		t.Errorf("one descendant visible: want has_children=true: %s", res.Text)
	}
	res = pathACLExec(t, p, oneVisible, `{"op":"stat","scope":"tenant","path":"/kb/top"}`)
	var st2 map[string]any
	_ = json.Unmarshal([]byte(res.Text), &st2)
	if st2["has_children"] != true {
		t.Errorf("stat, one descendant visible: want has_children=true, got %s", res.Text)
	}
}
