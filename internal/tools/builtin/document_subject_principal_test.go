package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// subjectFields reads a chunk's fields and revision through get_chunk.
func subjectFields(t *testing.T, d *Document, ctx context.Context, scope, id string) (map[string]any, int) {
	t.Helper()
	g, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":%q,"id":%q}`, scope, id))
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	f, _ := g["fields"].(map[string]any)
	return f, int(g["revision"].(float64))
}

// updateFields runs update_chunk with fields at the chunk's current revision.
func updateFields(t *testing.T, d *Document, ctx context.Context, scope, id, fields string) tools.Result {
	t.Helper()
	_, rev := subjectFields(t, d, ctx, scope, id)
	_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":%q,"id":%q,"revision":%d,"fields":%s}`,
		scope, id, rev, fields))
	return r
}

func bindInUser(t *testing.T, d *Document, ctx context.Context, ref, principal string) (SubjectBinding, error) {
	t.Helper()
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	return d.BindSubjectPrincipal(ctx, key, mscope, ref, principal)
}

// TestSubjectPrincipal_AgentWritesCannotSetOrChangeTheBinding.
//
// An agent able to bind a subject to an account could point an erasure report at the
// wrong person. Both writes that replace a subject root's fields refuse a principal
// that differs from the stored one, setting or changing it, and write nothing.
func TestSubjectPrincipal_AgentWritesCannotSetOrChangeTheBinding(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")

	for _, c := range []struct{ name, fields string }{
		{"set", `{"principal":"u-ada"}`},
		{"set alongside other fields", `{"color":"blue","principal":"u-ada"}`},
		{"not a string", `{"principal":7}`},
	} {
		r := updateFields(t, d, ctx, "user", root, c.fields)
		if !r.IsError || !strings.HasPrefix(r.Text, "principal_binding_operator_only:") {
			t.Errorf("update_chunk %s: got %v %q, want a principal_binding_operator_only refusal", c.name, r.IsError, r.Text)
		}
	}
	_, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"person:ada","title":"Ada","fields":{"principal":"u-ada"}}`)
	if !r.IsError || !strings.HasPrefix(r.Text, "principal_binding_operator_only:") {
		t.Errorf("upsert_chunk set: got %v %q, want a principal_binding_operator_only refusal", r.IsError, r.Text)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != nil || f["color"] != nil {
		t.Errorf("a refused write left fields %v", f)
	}

	// Bound by the operator: an agent can neither change nor clear it.
	if _, err := bindInUser(t, d, ctx, root, "u-ada"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, fields string }{
		{"change", `{"principal":"u-eve"}`},
		{"clear with null", `{"principal":null}`},
		{"clear with empty", `{"principal":""}`},
		{"fields that are not an object", `["x"]`},
	} {
		if r := updateFields(t, d, ctx, "user", root, c.fields); !r.IsError || !strings.HasPrefix(r.Text, "principal_binding_operator_only:") {
			t.Errorf("update_chunk %s: got %v %q, want a principal_binding_operator_only refusal", c.name, r.IsError, r.Text)
		}
	}
	_, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"person:ada","title":"Ada","fields":{"principal":"u-eve"}}`)
	if !r.IsError || !strings.HasPrefix(r.Text, "principal_binding_operator_only:") {
		t.Errorf("upsert_chunk change: got %v %q, want a principal_binding_operator_only refusal", r.IsError, r.Text)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != "u-ada" {
		t.Errorf("binding after refused writes = %v, want u-ada", f["principal"])
	}
}

// TestSubjectPrincipal_AgentFieldWritesKeepTheBinding.
//
// The refusal is of a CHANGE: an agent's read-modify-write repeats the stored value
// and goes through, and a write that omits the field keeps the binding rather than
// clearing it — otherwise rewriting a subject's fields would silently unbind it and an
// erasure would miss the subject.
func TestSubjectPrincipal_AgentFieldWritesKeepTheBinding(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	if _, err := bindInUser(t, d, ctx, root, "u-ada"); err != nil {
		t.Fatal(err)
	}
	if r := updateFields(t, d, ctx, "user", root, `{"color":"blue","principal":"u-ada"}`); r.IsError {
		t.Fatalf("read-modify-write repeating the binding: %s", r.Text)
	}
	if r := updateFields(t, d, ctx, "user", root, `null`); r.IsError {
		t.Fatalf("fields write of null: %s", r.Text)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != "u-ada" || f["color"] != nil {
		t.Errorf("fields after a null write = %v, want only the binding", f)
	}
	if r := updateFields(t, d, ctx, "user", root, `{"color":"green"}`); r.IsError {
		t.Fatalf("fields write omitting the binding: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"person:ada","title":"Ada","fields":{"size":"l"}}`); r.IsError {
		t.Fatalf("upsert omitting the binding: %s", r.Text)
	}
	f, _ := subjectFields(t, d, ctx, "user", root)
	if f["principal"] != "u-ada" || f["size"] != "l" {
		t.Errorf("fields = %v, want the new fields with the binding kept", f)
	}
}

// TestSubjectPrincipal_ReservedOnEveryChunkForAgentWrites.
//
// Guarding only subject roots left a promotion path: an agent sets the field on a
// chunk that is not a subject yet, the chunk later becomes one (homing, adoption, a
// root gaining a key), and erasure trusts a binding no operator wrote — which also
// blocks the operator from binding the real subject. So no agent write puts the field
// on ANY chunk: every value in the store is one the operator path wrote.
func TestSubjectPrincipal_ReservedOnEveryChunkForAgentWrites(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	plain, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	plainRoot := plain["root_chunk_id"].(string)
	refused := func(name string, r tools.Result) {
		t.Helper()
		if !r.IsError || !strings.HasPrefix(r.Text, "principal_binding_operator_only:") {
			t.Errorf("%s: got %v %q, want a principal_binding_operator_only refusal", name, r.IsError, r.Text)
		}
	}
	_, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`",
		"title":"Ada owns the repo","natural_key":"person:ada-planted","type":"person","subject":"Ada P","fields":{"principal":"u-x"}}`)
	refused("upsert of a new keyed chunk", r)
	_, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+`","title":"note","fields":{"principal":"u-x"}}`)
	refused("create_chunk", r)
	refused("update_chunk on a plain document root", updateFields(t, d, ctx, "user", plainRoot, `{"principal":"u-x"}`))
	_, r = docExec(t, d, ctx, `{"op":"import_md","scope":"user","path":"/notes/planted",
		"markdown":"# Planted\n<!-- loom: {\"fields\":{\"principal\":\"u-x\"}} -->\n\nbody\n## Child\n<!-- loom: {\"fields\":{\"principal\":\"u-y\"}} -->\n\nchild\n"}`)
	refused("import_md", r)

	// A field that is absent, null or empty sets nothing and passes.
	if r := updateFields(t, d, ctx, "user", plainRoot, `{"principal":null,"color":"blue"}`); r.IsError {
		t.Errorf("null principal on a plain root: %s", r.Text)
	}
	if f, _ := subjectFields(t, d, ctx, "user", plainRoot); f["principal"] != nil {
		t.Errorf("plain root fields = %v, want no principal", f)
	}
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"u-x", "u-y"} {
		if bound, err := d.SubjectsBoundTo(ctx, key, mscope, p); err != nil || len(bound) != 0 {
			t.Errorf("subjects bound to %s = %+v, %v; want none", p, bound, err)
		}
	}
	// The operator can still bind the real subject.
	if _, err := bindInUser(t, d, ctx, "person:ada", "u-x"); err != nil {
		t.Errorf("operator bind after refused plants: %v", err)
	}
}

// TestSubjectPrincipal_StaleAgentWriteCannotRestoreAClearedBinding.
//
// An agent write reads the fields (to keep the half it did not send, or to carry the
// binding) and writes later. An operator clear — an erasure — landing in between must
// not be undone by the write: after an erasure that would re-bind the erased person.
// Driven deterministically by handing the write path the fields as they were read
// before the clear.
func TestSubjectPrincipal_StaleAgentWriteCannotRestoreAClearedBinding(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bindInUser(t, d, ctx, root, "u-ada"); err != nil {
		t.Fatal(err)
	}
	stale := json.RawMessage(`{"principal":"u-ada","size":"l"}`)
	if _, err := bindInUser(t, d, ctx, root, ""); err != nil { // the erasure's clear
		t.Fatal(err)
	}
	if err := d.updateChunkForUpsert(ctx, key, mscope, root, docInput{Fields: stale}); err != nil {
		t.Fatalf("upsert write: %v", err)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != nil || f["size"] != "l" {
		t.Errorf("after a stale upsert write fields = %v, want size kept and no binding", f)
	}
	if err := d.writeBody(ctx, mscope, key, root, "person", "edited", stale); err != nil {
		t.Fatalf("body write: %v", err)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != nil {
		t.Errorf("after a stale body write fields = %v, want no binding", f)
	}
	if bound, _ := d.SubjectsBoundTo(ctx, key, mscope, "u-ada"); len(bound) != 0 {
		t.Errorf("the cleared binding came back: %+v", bound)
	}
}

// TestBindSubjectPrincipal_BindsAndClears.
func TestBindSubjectPrincipal_BindsAndClears(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")

	b, err := bindInUser(t, d, ctx, "person:ada", "u-ada")
	if err != nil {
		t.Fatal(err)
	}
	if !b.Changed || b.Principal != "u-ada" || b.Previous != "" || b.DocumentID != docID ||
		b.RootChunkID != root || b.NaturalKey != "person:ada" || b.Path != "/facts/ada" {
		t.Errorf("bind = %+v", b)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != "u-ada" {
		t.Errorf("fields after bind = %v", f)
	}
	if b, err := bindInUser(t, d, ctx, docID, "u-ada"); err != nil || b.Changed {
		t.Errorf("re-bind by document id = %+v, %v; want an unchanged success", b, err)
	}
	b, err = bindInUser(t, d, ctx, root, "")
	if err != nil || !b.Changed || b.Previous != "u-ada" || b.Principal != "" {
		t.Fatalf("clear = %+v, %v", b, err)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f != nil && f["principal"] != nil {
		t.Errorf("fields after clear = %v", f)
	}
	if b, err := bindInUser(t, d, ctx, root, ""); err != nil || b.Changed {
		t.Errorf("clearing an unbound subject = %+v, %v; want an unchanged success", b, err)
	}
}

// TestBindSubjectPrincipal_RefusesASecondPrincipalOnABoundSubject.
func TestBindSubjectPrincipal_RefusesASecondPrincipalOnABoundSubject(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	if _, err := bindInUser(t, d, ctx, root, "u-ada"); err != nil {
		t.Fatal(err)
	}
	_, err := bindInUser(t, d, ctx, root, "u-eve")
	var conflict *SubjectBindingConflictError
	if !errors.As(err, &conflict) || !conflict.SubjectBound || !strings.Contains(err.Error(), "u-ada") {
		t.Fatalf("second principal = %v, want a conflict naming the existing binding", err)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != "u-ada" {
		t.Errorf("binding after the refusal = %v", f["principal"])
	}
}

// TestBindSubjectPrincipal_RefusesASecondSubjectForAPrincipal: one subject per
// principal per scope, and the refusal names the subject holding it.
func TestBindSubjectPrincipal_RefusesASecondSubjectForAPrincipal(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	adaDoc, _ := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	_, lovelace := newSubject(t, d, ctx, "user", "Lovelace", "person", "person:lovelace")
	if _, err := bindInUser(t, d, ctx, "person:ada", "u-ada"); err != nil {
		t.Fatal(err)
	}
	_, err := bindInUser(t, d, ctx, "person:lovelace", "u-ada")
	var conflict *SubjectBindingConflictError
	if !errors.As(err, &conflict) || conflict.SubjectBound || conflict.Holder.DocumentID != adaDoc ||
		!strings.Contains(err.Error(), "person:ada") {
		t.Fatalf("second subject = %v, want a conflict naming person:ada", err)
	}
	if f, _ := subjectFields(t, d, ctx, "user", lovelace); f != nil && f["principal"] != nil {
		t.Errorf("refused subject was bound: %v", f)
	}
}

// TestBindSubjectPrincipal_RefusesANonSubject.
func TestBindSubjectPrincipal_RefusesANonSubject(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	plain, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	fact, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+`","parent_id":"`+root+`",
		"title":"Ada owns the repo","natural_key":"fact:ada-repo","type":"fact","subject":"Ada"}`)
	if r.IsError {
		t.Fatalf("upsert fact: %s", r.Text)
	}
	for _, ref := range []string{plain["document_id"].(string), plain["root_chunk_id"].(string), "fact:ada-repo", fact["id"].(string)} {
		if _, err := bindInUser(t, d, ctx, ref, "u-ada"); !errors.Is(err, ErrNotASubject) {
			t.Errorf("bind %s = %v, want ErrNotASubject", ref, err)
		}
	}
	if _, err := bindInUser(t, d, ctx, "person:nobody", "u-ada"); !errors.Is(err, ErrSubjectNotFound) {
		t.Errorf("bind unknown key = %v, want ErrSubjectNotFound", err)
	}
}

// TestSubjectErasure_ClearsAndAnnouncesWithoutThePrincipal: the erasure half in the
// tenant scope. Clearing deletes no subject, and the event names the subject only.
func TestSubjectErasure_ClearsAndAnnouncesWithoutThePrincipal(t *testing.T) {
	d, base, _ := documentFixture(t)
	w := withRecorder(d)
	ctx := grantedTenantCtx(base)
	docID, root := newSubject(t, d, ctx, "tenant", "Ada", "person", "person:ada")
	if _, err := d.BindTenantSubjectPrincipal(ctx, "person:ada", "u-ada"); err != nil {
		t.Fatal(err)
	}
	bound, err := d.TenantSubjectsBoundTo(ctx, "u-ada")
	if err != nil || len(bound) != 1 || bound[0].DocumentID != docID || !bound[0].Readable {
		t.Fatalf("bound = %+v, %v", bound, err)
	}
	cleared, err := d.ClearTenantSubjectBindings(ctx, "u-ada")
	if err != nil || len(cleared) != 1 || cleared[0].RootChunkID != root {
		t.Fatalf("cleared = %+v, %v", cleared, err)
	}
	if why := d.AnnounceTenantSubjectsErased(ctx, cleared); why != "" {
		t.Fatalf("not announced: %s", why)
	}
	if left, _ := d.TenantSubjectsBoundTo(ctx, "u-ada"); len(left) != 0 {
		t.Errorf("still bound after clearing: %+v", left)
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+docID+`"}`); r.IsError {
		t.Errorf("the subject document is gone: %s", r.Text)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var n int
	for _, m := range w.msgs {
		if m.Channel != "memory/subjects" {
			continue
		}
		n++
		if strings.Contains(string(m.Payload), "u-ada") || m.PublishedBy == "u-ada" {
			t.Errorf("the erased principal's id is on the channel: %s (published by %q)", m.Payload, m.PublishedBy)
		}
		var ev map[string]any
		_ = json.Unmarshal(m.Payload, &ev)
		if ev["op"] != "erased" || ev["document_id"] != docID || ev["subject_natural_key"] != "person:ada" {
			t.Errorf("event = %v", ev)
		}
	}
	if n != 1 {
		t.Errorf("memory/subjects messages = %d, want one erased", n)
	}
}

// TestSubjectErasure_WithheldAndUnnamedForAReadingLimitedCaller.
func TestSubjectErasure_WithheldAndUnnamedForAReadingLimitedCaller(t *testing.T) {
	d, base, _ := documentFixture(t)
	w := withRecorder(d)
	ctx := grantedTenantCtx(base)
	newSubject(t, d, ctx, "tenant", "Ada", "person", "person:ada")
	if _, err := d.BindTenantSubjectPrincipal(ctx, "person:ada", "u-ada"); err != nil {
		t.Fatal(err)
	}
	limited := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/facts/**"}}))
	bound, err := d.TenantSubjectsBoundTo(limited, "u-ada")
	if err != nil || len(bound) != 1 || bound[0].Readable {
		t.Fatalf("bound = %+v, %v; want the subject, marked unreadable", bound, err)
	}
	if why := d.AnnounceTenantSubjectsErased(limited, bound); why == "" {
		t.Error("announced for a caller whose rules hide part of the scope")
	}
	if evs := w.subjectEvents(t); len(evs) != 0 {
		t.Errorf("events = %v", evs)
	}
}

// TestSubjectPrincipal_MemoryCannotRemoveABoundSubjectsBody.
//
// The Memory tool removes a fact's body row by key, and a subject root re-asserted
// through upsert_chunk carries provenance, so it reads as a fact. Removing it would
// clear the binding by the back door — an erasure would then miss the subject.
func TestSubjectPrincipal_MemoryCannotRemoveABoundSubjectsBody(t *testing.T) {
	d, ctx, st := documentFixture(t)
	_, root := newSubject(t, d, ctx, "user", "Ada", "person", "person:ada")
	if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","natural_key":"person:ada","title":"Ada","body":"Ada, a mathematician."}`); r.IsError {
		t.Fatalf("re-assert the subject: %s", r.Text)
	}
	if _, err := bindInUser(t, d, ctx, root, "u-ada"); err != nil {
		t.Fatal(err)
	}
	m := &Memory{Store: st, SqlMem: d.SqlMem}
	r, err := m.Execute(ctx, json.RawMessage(`{"op":"delete","scope":"user","key":"`+chunkBodyKey(root)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError || !strings.HasPrefix(r.Text, "principal_binding_operator_only:") {
		t.Errorf("Memory delete of a bound subject's body: got %v %q, want a principal_binding_operator_only refusal", r.IsError, r.Text)
	}
	if f, _ := subjectFields(t, d, ctx, "user", root); f["principal"] != "u-ada" {
		t.Errorf("binding after the delete = %v", f["principal"])
	}
}

// TestSubjectPrincipal_AgentLeaseCannotBlockAnErasure.
//
// The binding path serialises on a lease in the scope's lease table, which the
// Document lease op also writes. An agent holding that name would make every bind,
// clear and erasure wait out and fail; erasure must not be blockable by an agent. So
// the names are reserved, and an agent holding any other lease changes nothing.
func TestSubjectPrincipal_AgentLeaseCannotBlockAnErasure(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := grantedTenantCtx(base)
	_, root := newSubject(t, d, ctx, "tenant", "Ada", "person", "person:ada")
	if _, err := d.BindTenantSubjectPrincipal(ctx, "person:ada", "u-ada"); err != nil {
		t.Fatal(err)
	}
	agent := tools.WithRunID(tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.StatementMethods = []string{"linker"}
	}), "run-agent")
	for _, name := range []string{"principal:" + root, "principal:binding", "linker"} {
		_, r := docExec(t, d, agent, `{"op":"lease","scope":"tenant","name":"`+name+`","ttl_ms":600000}`)
		if reserved := strings.HasPrefix(name, "principal:"); reserved != r.IsError {
			t.Errorf("lease %s: error=%v %q, want refused only for a reserved name", name, r.IsError, r.Text)
		}
	}
	cleared, err := d.ClearTenantSubjectBindings(ctx, "u-ada")
	if err != nil || len(cleared) != 1 {
		t.Fatalf("erasure clear while an agent holds leases = %+v, %v; want the subject unbound", cleared, err)
	}
	if _, err := d.BindTenantSubjectPrincipal(ctx, "person:ada", "u-eve"); err != nil {
		t.Errorf("operator bind while an agent holds leases: %v", err)
	}
}
