package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §1.5 seam 5: the Document and Path tools ask the caller's ACL about
// the document a call resolves to, on every call, by its Path-tree name.

func aclSet(t *testing.T, rules ...authz.Rule) *authz.Effective {
	t.Helper()
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "tnt", ID: "u1"},
		Bindings: []authz.Binding{{ACL: "under-test", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"under-test": {ACL: authz.ACL{Name: "under-test", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// tenantDocCtx is a run granted the tenant document scope, bound to eff (nil:
// no set, as a run with no principal has).
func tenantDocCtx(base context.Context, eff *authz.Effective) context.Context {
	return tools.WithRunPolicy(base, tools.RunPolicy{
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		ACL:    eff,
	})
}

type aclDocs struct {
	secretDoc, secretChunk, publicDoc, publicChunk string
}

// seedACLDocs writes /kb/secret/plan and /kb/public/notes in the tenant scope
// with no set on ctx.
func seedACLDocs(t *testing.T, d *Document, ctx context.Context) aclDocs {
	t.Helper()
	var s aclDocs
	mk := func(path, title, body string) (string, string) {
		out, res := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"`+title+`","path":"`+path+`"}`)
		if res.IsError {
			t.Fatalf("create_document %s: %s", path, res.Text)
		}
		doc := out["document_id"].(string)
		out, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"tenant","document_id":"`+doc+`","title":"`+title+` part","body":"`+body+`"}`)
		if res.IsError {
			t.Fatalf("create_chunk %s: %s", path, res.Text)
		}
		return doc, out["id"].(string)
	}
	s.secretDoc, s.secretChunk = mk("/kb/secret/plan", "Plan", "savepoint nesting secret")
	s.publicDoc, s.publicChunk = mk("/kb/public/notes", "Notes", "savepoint nesting public")
	return s
}

func refusedByACL(t *testing.T, what string, res tools.Result) {
	t.Helper()
	if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission || !strings.Contains(res.Text, "access-control") {
		t.Errorf("%s: want an access-control refusal, got IsError=%v %s", what, res.IsError, res.Text)
	}
}

func allowed(t *testing.T, what string, res tools.Result) {
	t.Helper()
	if res.IsError {
		t.Errorf("%s: refused: %s", what, res.Text)
	}
}

func TestDocumentACL_WriteDenyRefusesWritesUnderThePathOnly(t *testing.T) {
	d, base, _ := documentFixture(t)
	s := seedACLDocs(t, d, tenantDocCtx(base, nil))
	ctx := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/kb/secret/**"}}))

	_, res := docExec(t, d, ctx, `{"op":"create_chunk","scope":"tenant","document_id":"`+s.secretDoc+`","title":"x","body":"y"}`)
	refusedByACL(t, "create_chunk under /kb/secret", res)
	_, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"tenant","parent_id":"`+s.secretChunk+`","title":"x","body":"y"}`)
	refusedByACL(t, "create_chunk under a /kb/secret chunk", res)
	_, res = docExec(t, d, ctx, `{"op":"update_chunk","scope":"tenant","id":"`+s.secretChunk+`","body":"changed","revision":1}`)
	refusedByACL(t, "update_chunk under /kb/secret", res)
	_, res = docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"New","path":"/kb/secret/new"}`)
	refusedByACL(t, "create_document at /kb/secret/new", res)
	_, res = docExec(t, d, ctx, `{"op":"set_path","scope":"tenant","id":"`+s.publicDoc+`","path":"/kb/secret/alias"}`)
	refusedByACL(t, "set_path into /kb/secret", res)

	_, res = docExec(t, d, ctx, `{"op":"create_chunk","scope":"tenant","document_id":"`+s.publicDoc+`","title":"x","body":"y"}`)
	allowed(t, "create_chunk under /kb/public", res)
	_, res = docExec(t, d, ctx, `{"op":"update_chunk","scope":"tenant","id":"`+s.publicChunk+`","body":"changed","revision":1}`)
	allowed(t, "update_chunk under /kb/public", res)
	_, res = docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Other","path":"/kb/other/doc"}`)
	allowed(t, "create_document at /kb/other/doc", res)
	// Reading the denied-for-write document is still allowed.
	_, res = docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+s.secretChunk+`"}`)
	allowed(t, "get_chunk under /kb/secret (read not denied)", res)
	// The update never landed.
	out, _ := docExec(t, d, tenantDocCtx(base, nil), `{"op":"get_chunk","scope":"tenant","id":"`+s.secretChunk+`"}`)
	if out["body"] == "changed" {
		t.Error("the refused update_chunk changed the body")
	}
}

func TestDocumentACL_ReadDenyHidesFromSearchAndListAndRefusesGet(t *testing.T) {
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	s := seedACLDocs(t, d, tenantDocCtx(base, nil))
	ctx := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))

	_, res := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.secretDoc+`"}`)
	refusedByACL(t, "get_document /kb/secret/plan", res)
	_, res = docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","path":"/kb/secret/plan"}`)
	refusedByACL(t, "get_document by path", res)
	_, res = docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+s.secretChunk+`"}`)
	refusedByACL(t, "get_chunk under /kb/secret", res)
	_, res = docExec(t, d, ctx, `{"op":"export_md","scope":"tenant","document_id":"`+s.secretDoc+`"}`)
	refusedByACL(t, "export_md /kb/secret/plan", res)
	_, res = docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.publicDoc+`"}`)
	allowed(t, "get_document /kb/public/notes", res)

	for _, op := range []string{
		`{"op":"query_documents","scope":"tenant"}`,
		`{"op":"documents_summary","scope":"tenant","document_ids":["` + s.secretDoc + `","` + s.publicDoc + `"]}`,
		`{"op":"search","scope":"tenant","query":"savepoint nesting"}`,
	} {
		_, res := docExec(t, d, ctx, op)
		if res.IsError {
			t.Fatalf("%s: %s", op, res.Text)
		}
		if strings.Contains(res.Text, s.secretDoc) {
			t.Errorf("%s lists the read-denied document: %s", op, res.Text)
		}
		if !strings.Contains(res.Text, s.publicDoc) {
			t.Errorf("%s dropped the readable document: %s", op, res.Text)
		}
	}
	// An op that reads the whole scope without naming documents cannot honour a
	// partial deny, so it is refused rather than leak.
	_, res = docExec(t, d, ctx, `{"op":"list_facts","scope":"tenant"}`)
	refusedByACL(t, "list_facts over a partly read-denied scope", res)
}

func TestDocumentACL_NoSetLeavesEveryOpAsBefore(t *testing.T) {
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	ctx := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, ctx)
	for _, op := range []string{
		`{"op":"get_document","scope":"tenant","id":"` + s.secretDoc + `"}`,
		`{"op":"update_chunk","scope":"tenant","id":"` + s.secretChunk + `","body":"b2","revision":1}`,
		`{"op":"list_facts","scope":"tenant"}`,
		`{"op":"create_document","scope":"tenant","title":"N","path":"/kb/secret/n"}`,
	} {
		_, res := docExec(t, d, ctx, op)
		allowed(t, op, res)
	}
	out, res := docExec(t, d, ctx, `{"op":"query_documents","scope":"tenant"}`)
	if res.IsError || len(out["documents"].([]any)) != 3 {
		t.Errorf("query_documents with no set = %s", res.Text)
	}
}

// Off a run the set is the principal's, stamped by the transport.
func TestDocumentACL_OffRunPrincipalSetIsAsked(t *testing.T) {
	d, base, _ := documentFixture(t)
	s := seedACLDocs(t, d, tenantDocCtx(base, nil))
	eff := aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/kb/secret/**"}})
	ctx := tools.WithRunPolicy(auth.WithPrincipal(base, auth.Principal{TenantID: "tnt", Subject: "u1", Effective: eff}), tools.RunPolicy{
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
	})
	_, res := docExec(t, d, ctx, `{"op":"update_chunk","scope":"tenant","id":"`+s.secretChunk+`","body":"changed","revision":1}`)
	refusedByACL(t, "off-run update_chunk under /kb/secret", res)
	_, res = docExec(t, d, ctx, `{"op":"update_chunk","scope":"tenant","id":"`+s.publicChunk+`","body":"changed","revision":1}`)
	allowed(t, "off-run update_chunk under /kb/public", res)
}

func pathACLExec(t *testing.T, p *Path, ctx context.Context, body string) tools.Result {
	t.Helper()
	res, err := p.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Path hard error: %v", err)
	}
	return res
}

func TestPathACL_MvAndRmAskTheDocumentVerbOnBothNames(t *testing.T) {
	d, base, st := documentFixture(t)
	seedACLDocs(t, d, tenantDocCtx(base, nil))
	p := &Path{Store: st}
	ctx := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"write", "delete"}, Resources: []string{"document:tenant:/kb/secret/**"}}))

	refusedByACL(t, "mv out of /kb/secret", pathACLExec(t, p, ctx, `{"op":"mv","scope":"tenant","path":"/kb/secret/plan","to":"/kb/moved"}`))
	refusedByACL(t, "mv into /kb/secret", pathACLExec(t, p, ctx, `{"op":"mv","scope":"tenant","path":"/kb/public/notes","to":"/kb/secret/notes"}`))
	refusedByACL(t, "mv of a directory holding /kb/secret", pathACLExec(t, p, ctx, `{"op":"mv","scope":"tenant","path":"/kb","to":"/kb2"}`))
	refusedByACL(t, "rm under /kb/secret", pathACLExec(t, p, ctx, `{"op":"rm","scope":"tenant","path":"/kb/secret/plan"}`))
	refusedByACL(t, "recursive rm of a directory holding /kb/secret", pathACLExec(t, p, ctx, `{"op":"rm","scope":"tenant","path":"/kb","recursive":true}`))
	refusedByACL(t, "mkdir under /kb/secret", pathACLExec(t, p, ctx, `{"op":"mkdir","scope":"tenant","path":"/kb/secret/empty"}`))

	allowed(t, "mv within /kb/public", pathACLExec(t, p, ctx, `{"op":"mv","scope":"tenant","path":"/kb/public/notes","to":"/kb/public/notes2"}`))
	if res := pathACLExec(t, p, tenantDocCtx(base, nil), `{"op":"resolve","scope":"tenant","path":"/kb/secret/plan"}`); res.IsError {
		t.Errorf("a refused mv/rm moved or removed /kb/secret/plan: %s", res.Text)
	}
}

func TestPathACL_ReadDenyHidesFromLsAndRefusesResolve(t *testing.T) {
	d, base, st := documentFixture(t)
	seedACLDocs(t, d, tenantDocCtx(base, nil))
	p := &Path{Store: st}
	ctx := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))

	for _, ls := range []string{
		`{"op":"ls","scope":"tenant","path":"/kb"}`,
		`{"op":"ls","scope":"tenant","path":"/","recursive":true}`,
		`{"op":"ls","scope":"tenant","path":"/kb/secret"}`,
	} {
		res := pathACLExec(t, p, ctx, ls)
		if res.IsError {
			t.Fatalf("%s: %s", ls, res.Text)
		}
		var out struct {
			Entries []pathEntry `json:"entries"`
		}
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatal(err)
		}
		for _, e := range out.Entries {
			if strings.Contains(e.FullPath, "secret") {
				t.Errorf("%s lists a read-denied name: %s", ls, e.FullPath)
			}
		}
	}
	if res := pathACLExec(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb"}`); !strings.Contains(res.Text, "public") {
		t.Errorf("ls /kb dropped the readable directory: %s", res.Text)
	}
	refusedByACL(t, "resolve /kb/secret/plan", pathACLExec(t, p, ctx, `{"op":"resolve","scope":"tenant","path":"/kb/secret/plan"}`))
	refusedByACL(t, "stat /kb/secret/plan", pathACLExec(t, p, ctx, `{"op":"stat","scope":"tenant","path":"/kb/secret/plan"}`))
	allowed(t, "resolve /kb/public/notes", pathACLExec(t, p, ctx, `{"op":"resolve","scope":"tenant","path":"/kb/public/notes"}`))
}

// Enforce mode keeps only what a rule allows: an allow on /kb/public reads
// that document and nothing else.
func TestDocumentACL_EnforceKeepsOnlyTheAllowedPart(t *testing.T) {
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	s := seedACLDocs(t, d, tenantDocCtx(base, nil))
	ctx := authz.WithMode(tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/public/**"}})), authz.ModeEnforce)

	_, res := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.secretDoc+`"}`)
	refusedByACL(t, "enforce: get_document with no allow", res)
	_, res = docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+s.publicDoc+`"}`)
	allowed(t, "enforce: get_document under the allow", res)
	_, res = docExec(t, d, ctx, `{"op":"update_chunk","scope":"tenant","id":"`+s.publicChunk+`","body":"x","revision":1}`)
	refusedByACL(t, "enforce: write with only a read allow", res)
	_, res = docExec(t, d, ctx, `{"op":"query_documents","scope":"tenant"}`)
	if res.IsError || strings.Contains(res.Text, s.secretDoc) || !strings.Contains(res.Text, s.publicDoc) {
		t.Errorf("enforce: query_documents = %s", res.Text)
	}
}

// Every op the Document tool accepts has an entry in the ACL table, so a new
// op cannot ship without saying what it asks.
func TestDocumentACL_EveryOpIsMapped(t *testing.T) {
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(documentInputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties.Op.Enum) == 0 {
		t.Fatal("no op enum in the Document schema")
	}
	for _, op := range schema.Properties.Op.Enum {
		if _, ok := docACLOps[op]; !ok {
			t.Errorf("Document op %q has no entry in docACLOps", op)
		}
	}
	for op := range docACLOps {
		found := false
		for _, e := range schema.Properties.Op.Enum {
			found = found || e == op
		}
		if !found {
			t.Errorf("docACLOps names %q, which the Document schema does not accept", op)
		}
	}
}

// A deny under a condition only an instance can answer (owner_is_self) is
// not settled by the bare scope: whole-scope ops are refused, and listings
// fall back to asking each document, which here (tenant documents have no
// owner) leaves them all readable.
func TestDocumentACL_ConditionalDenyFallsBackToEachDocument(t *testing.T) {
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	s := seedACLDocs(t, d, tenantDocCtx(base, nil))
	self := true
	ctx := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/**"}, When: &authz.Condition{OwnerIsSelf: &self}}))

	_, res := docExec(t, d, ctx, `{"op":"list_facts","scope":"tenant"}`)
	refusedByACL(t, "list_facts under a conditional deny", res)
	_, res = docExec(t, d, ctx, `{"op":"search","scope":"tenant","query":"savepoint nesting"}`)
	if res.IsError || !strings.Contains(res.Text, s.secretDoc) || !strings.Contains(res.Text, s.publicDoc) {
		t.Errorf("search under an owner_is_self deny on unowned documents = %s", res.Text)
	}
}
