package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func TestValidateDirentEntry_RefusesWhatNoToolCouldHaveWritten(t *testing.T) {
	entry := func(mut func(m map[string]any)) json.RawMessage {
		m := map[string]any{"tenant_id": "acme", "scope": "user", "scope_id": "alice",
			"parent_path": "/docs/", "name": "plan", "kind": "document",
			"resource_ref": map[string]any{"document_id": "d1"}}
		if mut != nil {
			mut(m)
		}
		b, _ := json.Marshal(m)
		return b
	}
	good := map[string]json.RawMessage{
		"document":      entry(nil),
		"root child":    entry(func(m map[string]any) { m["parent_path"] = "/" }),
		"tenant scope":  entry(func(m map[string]any) { m["scope"] = "tenant"; m["scope_id"] = "" }),
		"agent scope":   entry(func(m map[string]any) { m["scope"] = "agent"; m["scope_id"] = "helper" }),
		"operator tree": entry(func(m map[string]any) { delete(m, "tenant_id") }),
		"memory entry": entry(func(m map[string]any) {
			m["kind"] = "memory_entry"
			m["resource_ref"] = map[string]any{"scope": "user", "scope_id": "alice", "key": "k", "facet": "kv"}
		}),
		"volume mount": entry(func(m map[string]any) {
			m["scope"], m["scope_id"], m["kind"] = "tenant", "", "volume_mount"
			m["resource_ref"] = map[string]any{"volume_name": "data", "mode": "rw"}
		}),
		"directory": entry(func(m map[string]any) { m["kind"] = "directory"; m["resource_ref"] = map[string]any{} }),
	}
	for name, body := range good {
		if err := ValidateDirentEntry(body); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	bad := map[string]json.RawMessage{
		"unknown scope":             entry(func(m map[string]any) { m["scope"] = "global" }),
		"tenant with a scope id":    entry(func(m map[string]any) { m["scope"] = "tenant"; m["scope_id"] = "acme" }),
		"user without a scope id":   entry(func(m map[string]any) { m["scope_id"] = "" }),
		"parent without slash":      entry(func(m map[string]any) { m["parent_path"] = "/docs" }),
		"relative parent":           entry(func(m map[string]any) { m["parent_path"] = "docs/" }),
		"dot-dot parent":            entry(func(m map[string]any) { m["parent_path"] = "/docs/../etc/" }),
		"doubled slash":             entry(func(m map[string]any) { m["parent_path"] = "/docs//" }),
		"name with a slash":         entry(func(m map[string]any) { m["name"] = "a/b" }),
		"dot-dot name":              entry(func(m map[string]any) { m["name"] = ".." }),
		"empty name":                entry(func(m map[string]any) { m["name"] = "" }),
		"name with a space":         entry(func(m map[string]any) { m["name"] = "my plan" }),
		"overlong name":             entry(func(m map[string]any) { m["name"] = strings.Repeat("a", maxSegmentLen+1) }),
		"unknown kind":              entry(func(m map[string]any) { m["kind"] = "symlink" }),
		"ref not an object":         entry(func(m map[string]any) { m["resource_ref"] = "d1" }),
		"document without an id":    entry(func(m map[string]any) { m["resource_ref"] = map[string]any{} }),
		"memory entry without key":  entry(func(m map[string]any) { m["kind"] = "memory_entry" }),
		"mount without volume name": entry(func(m map[string]any) { m["kind"] = "volume_mount" }),
	}
	for name, body := range bad {
		if err := ValidateDirentEntry(body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestSnapshotDocumentExists_FindsADocumentByItsDirentCoordinates: the check is
// handed the coordinates a dirent carries and must find the document the
// Document tool filed at the SQL Memory coordinates of the same scope — for
// every scope, and for the operator tenant "" (which SQL Memory stores as
// "default"). A document id the scope does not hold, and a scope nobody has
// used, are absent; asking about the unused scope does not create it.
func TestSnapshotDocumentExists_FindsADocumentByItsDirentCoordinates(t *testing.T) {
	d, tenantCtx, st := documentDirentFixture(t)
	operatorCtx := tools.WithRunIdentity(tenantCtx, tools.RunIdentityValue{AgentID: "a", UserID: "u1"})
	exists := SnapshotDocumentExists(d.SqlMem)

	cases := []struct {
		name, scope string
		ctx         context.Context
	}{
		{"tenant tnt, agent scope", "agent", tenantCtx},
		{"tenant tnt, user scope", "user", tenantCtx},
		{"tenant tnt, tenant scope", "tenant", tenantCtx},
		{"operator tenant, user scope", "user", operatorCtx},
		{"operator tenant, tenant scope", "tenant", operatorCtx},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			path := "/probe/" + strings.ReplaceAll(c.name, " ", "-")
			path = strings.ReplaceAll(path, ",", "")
			out, res := docExec(t, d, c.ctx, `{"op":"create_document","scope":"`+c.scope+`","title":"probe","path":"`+path+`"}`)
			if res.IsError {
				t.Fatalf("create_document: %s", res.Text)
			}
			docID, _ := out["document_id"].(string)
			tenant := tools.RunIdentity(c.ctx).TenantID
			scopeID := map[string]string{"agent": "curator", "user": "u1", "tenant": ""}[c.scope]
			parent, name, _ := splitPath(path)
			row, err := st.DirentGet(ctx, tenant, c.scope, scopeID, parent, name)
			if err != nil {
				t.Fatalf("the dirent is not at the coordinates this test expects: %v", err)
			}
			var ref struct {
				DocumentID string `json:"document_id"`
			}
			_ = json.Unmarshal(row.ResourceRef, &ref)
			if ok, err := exists(ctx, row.TenantID, row.Scope, row.ScopeID, ref.DocumentID); err != nil || !ok || ref.DocumentID != docID {
				t.Errorf("document %s named at %s: exists=%v err=%v; want it found", docID, path, ok, err)
			}
			if ok, err := exists(ctx, row.TenantID, row.Scope, row.ScopeID, "no-such-document"); err != nil || ok {
				t.Errorf("an id the scope does not hold: exists=%v err=%v, want false", ok, err)
			}
		})
	}

	ctx := context.Background()
	before, err := d.SqlMem.ListScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := exists(ctx, "tnt", "user", "nobody", "d1"); err != nil || ok {
		t.Errorf("a scope nobody has used: exists=%v err=%v, want false", ok, err)
	}
	after, err := d.SqlMem.ListScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("asking about an unused scope provisioned it: %d scopes before, %d after", len(before), len(after))
	}
}
