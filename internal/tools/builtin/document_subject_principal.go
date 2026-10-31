package builtin

// A subject root may carry `fields.principal`: the subject id of a user principal in
// the same tenant, meaning "this subject is that account's person". Erasing the
// account then finds the subject, clears the binding and announces `erased` on
// memory/subjects, so whatever indexes relations by subject can re-check them.
//
// ONLY AN OPERATOR SETS OR CLEARS IT. An agent able to bind a subject to an account
// could point an erasure report at the wrong person, and one able to clear it could
// make an erasure miss the subject. So every write that can replace a subject root's
// fields — update_chunk and upsert_chunk, which every other caller (sync, set_remote,
// reimport, classification, ontology instances) goes through — keeps the stored
// binding and refuses a change to it. The operator path is BindSubjectPrincipal,
// marked on the context with a key only this package can set.
//
// The binding lives in the root's `fields`, the Memory k/v plane, not in SQL, so
// finding the subjects bound to one principal reads every subject root's body in the
// scope. That is an operator action (binding, erasure) and never a run's hot path.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// subjectPrincipalField is the subject root field naming the bound principal.
const subjectPrincipalField = "principal"

// principalBindingCode leads the refusal text, as other document refusals lead with
// their code.
const principalBindingCode = "principal_binding_operator_only"

// principalLeasePrefix names the leases subject bindings serialise on. The Document
// lease op refuses it (documentLeaseInput), so no caller can hold one and stall a
// bind, a clear or an erasure; Memory SQL cannot name the lease table at all.
const principalLeasePrefix = "principal:"

// subjectPrincipalLease serialises bindings in one scope, so two concurrent binds
// cannot both pass the one-subject-per-principal check.
const subjectPrincipalLease = principalLeasePrefix + "binding"

// How long a subject lease is waited for. A holder keeps it for one read and one
// write, so a wait is short; the operator path waits longer, since an erasure that
// gives up leaves a binding in place.
const (
	agentSubjectLeaseWait    = 500 * time.Millisecond
	operatorSubjectLeaseWait = 5 * time.Second
)

// principalBindingWriteKey marks a context as the operator binding path. Unexported, so
// no caller outside this package — and no tool input — can set it.
type principalBindingWriteKey struct{}

func withPrincipalBindingWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalBindingWriteKey{}, true)
}

func isPrincipalBindingWrite(ctx context.Context) bool {
	v, _ := ctx.Value(principalBindingWriteKey{}).(bool)
	return v
}

// ErrSubjectNotFound: the reference names no document, root chunk or natural key in
// the scope.
var ErrSubjectNotFound = errors.New("no subject with that document id, root chunk id or natural key in this scope")

// ErrNotASubject: the reference names a document or chunk that is not a subject (a
// document whose root chunk carries a natural key).
var ErrNotASubject = errors.New("that document is not a subject: its root chunk carries no natural key")

// SubjectBindingConflictError refuses a binding the one-to-one rule forbids. Holder
// is the subject that holds the conflicting binding.
type SubjectBindingConflictError struct {
	// SubjectBound: the subject already names another principal (clear it first).
	// Otherwise the principal is already bound to Holder, another subject.
	SubjectBound bool
	Principal    string
	Holder       SubjectBinding
}

func (e *SubjectBindingConflictError) Error() string {
	if e.SubjectBound {
		return fmt.Sprintf("subject %s is already bound to principal %q; clear that binding before binding another",
			e.Holder.NaturalKey, e.Holder.Principal)
	}
	return fmt.Sprintf("principal %q is already bound to subject %s (document %s); one subject per principal",
		e.Principal, e.Holder.NaturalKey, e.Holder.DocumentID)
}

// SubjectBinding is one subject and the principal bound to it.
type SubjectBinding struct {
	DocumentID  string `json:"document_id"`
	RootChunkID string `json:"root_chunk_id"`
	NaturalKey  string `json:"natural_key"`
	// Path is the document's first Path-tree name, "" when it has none.
	Path string `json:"path,omitempty"`
	// Principal is the binding after the call; Previous the one before it.
	Principal string `json:"principal,omitempty"`
	Previous  string `json:"previous,omitempty"`
	// Changed is false when the call found the binding already as asked.
	Changed bool `json:"changed"`
	// Readable reports whether the caller's access-control rules let it read the
	// subject's document, so a caller can withhold the names of ones it may not.
	Readable bool `json:"-"`
}

// principalOf reads the principal from a fields blob: "" when there is none or the
// fields are not an object.
func principalOf(fields json.RawMessage) string {
	var m map[string]json.RawMessage
	if len(fields) == 0 || json.Unmarshal(fields, &m) != nil {
		return ""
	}
	var p string
	_ = json.Unmarshal(m[subjectPrincipalField], &p)
	return strings.TrimSpace(p)
}

// subjectPrincipal returns the principal bound to the subject whose root chunk is
// rootChunkID, or "" when none is. It does not check that the chunk is a subject root.
// A subject merge uses it with setSubjectPrincipal to move a duplicate's binding to the
// survivor, and to refuse the merge when the survivor names a different principal.
func (d *Document) subjectPrincipal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, rootChunkID string) (string, error) {
	cb, err := d.readBody(ctx, mscope, key, rootChunkID)
	if err != nil {
		return "", err
	}
	return principalOf(cb.Fields), nil
}

// principalChange reports whether a write of fields would set the principal to
// something other than stored. An absent field changes nothing (the stored binding is
// kept, see keepStoredPrincipal); null or "" asks for no binding; anything that is not
// a string, or fields that are not an object while a binding is stored, is a change.
func principalChange(stored string, fields json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(fields, &m) != nil {
		return stored != ""
	}
	raw, present := m[subjectPrincipalField]
	if !present {
		return false
	}
	var asked string
	if string(raw) != "null" && json.Unmarshal(raw, &asked) != nil {
		return true
	}
	return strings.TrimSpace(asked) != stored
}

// holdPrincipalField refuses an agent write whose fields would set, change or clear
// the `principal` field, on ANY chunk: chunkID names the chunk being rewritten, or is
// "" for one being created. Guarding only subject roots would leave a promotion path —
// a chunk carrying the field becomes a subject later (homing, adoption, a root gaining
// a key) and erasure trusts a binding no operator wrote. Reserved everywhere, every
// value in the store is one BindSubjectPrincipal wrote. Repeating the stored value
// passes, so an agent's read-modify-write of a subject's fields still works.
//
// This is the refusal the caller reads. What a write actually stores is decided again
// at the write, under the subject's lease (keepStoredPrincipal), because the value read
// here may be stale by then.
func (d *Document) holdPrincipalField(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op, chunkID string, fields json.RawMessage) (*tools.Result, error) {
	trimmed := bytes.TrimSpace(fields)
	if len(trimmed) == 0 || isPrincipalBindingWrite(ctx) {
		return nil, nil
	}
	var stored string
	// An object without the field changes nothing, whatever is stored: no read.
	if chunkID != "" && (bytes.Contains(fields, []byte(`"`+subjectPrincipalField+`"`)) || trimmed[0] != '{') {
		var err error
		if stored, err = d.subjectPrincipal(ctx, key, mscope, chunkID); err != nil {
			return nil, err
		}
	}
	if !principalChange(stored, fields) {
		return nil, nil
	}
	r := errPermission(principalBindingCode+": "+op+": the `"+subjectPrincipalField+"` field is reserved: on a "+
		"subject it binds the subject to a user account, and only an operator sets or clears that binding. "+
		"Nothing was written.",
		"Resend the write without the `"+subjectPrincipalField+"` field (a stored binding is kept), or ask an "+
			"operator to bind or unbind the subject.")
	return &r, nil
}

// keepStoredPrincipal runs at every body write (putBody) that is not the operator
// binding path, and makes the written fields carry exactly the stored principal: kept
// when the write omits it, restored when it names another. holdPrincipalField already
// refused an agent's change; this closes the race behind it. A write reads the fields
// (to keep the half it did not send, or to repeat the binding) and writes later, so an
// operator clear landing in between — an erasure — would be undone by the stale copy
// and re-bind the erased person. On a subject root the stored value is read and the
// body written under the subject's lease, which the binding path also holds.
//
// Elsewhere no binding can be stored (the field is reserved), so a write without the
// field is passed through without a read. The returned release must be called after
// the write.
func (d *Document) keepStoredPrincipal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, chunkID string, fields json.RawMessage) (json.RawMessage, func(), error) {
	noop := func() {}
	if isPrincipalBindingWrite(ctx) {
		return fields, noop, nil
	}
	isRoot, err := d.isSubjectRootChunk(ctx, key, chunkID)
	if err != nil {
		return nil, noop, err
	}
	if !isRoot && !bytes.Contains(fields, []byte(`"`+subjectPrincipalField+`"`)) {
		return fields, noop, nil
	}
	release := noop
	if isRoot {
		if release, err = d.holdSubjectLease(ctx, key, chunkID, agentSubjectLeaseWait); err != nil {
			return nil, noop, err
		}
	}
	stored, err := d.subjectPrincipal(ctx, key, mscope, chunkID)
	if err != nil {
		release()
		return nil, noop, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(fields, &m) != nil || m == nil {
		if stored == "" {
			return fields, release, nil
		}
		m = map[string]json.RawMessage{}
	}
	if stored == "" {
		if _, ok := m[subjectPrincipalField]; !ok {
			return fields, release, nil
		}
		delete(m, subjectPrincipalField)
	} else {
		m[subjectPrincipalField], _ = json.Marshal(stored)
	}
	out, err := json.Marshal(m)
	if err != nil {
		release()
		return nil, noop, err
	}
	return out, release, nil
}

// isSubjectRootChunk reports whether chunkID is a subject root: its document's root,
// carrying a natural key. Point lookups only, since every body write asks.
func (d *Document) isSubjectRootChunk(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (bool, error) {
	res, err := d.query(ctx, key, `SELECT 1 FROM chunks c
		  JOIN documents doc ON doc.id = c.document_id AND doc.root_chunk_id = c.id
		  JOIN chunk_memory_meta m ON m.chunk_id = c.id
		 WHERE c.id = ? AND coalesce(m.natural_key, '') <> '' LIMIT 1`, chunkID)
	if err != nil {
		return false, err
	}
	return len(res.Rows) > 0, nil
}

// errSubjectBusy: the subject's binding is being written; the write may be retried.
var errSubjectBusy = errors.New("this subject's binding is being written by an operator; retry the write")

// holdSubjectLease takes the subject's own lease, which serialises the binding path
// with every other body write on that subject root. It waits briefly: both sides hold
// it only for one read and one write.
func (d *Document) holdSubjectLease(ctx context.Context, key sqlmem.ScopeKey, rootChunkID string, wait time.Duration) (func(), error) {
	return d.holdBindingLease(ctx, key, principalLeasePrefix+rootChunkID, wait)
}

// holdBindingLease takes one of the binding path's leases (a principalLeasePrefix
// name), waiting up to wait for a holder to let go. errSubjectBusy when it does not.
func (d *Document) holdBindingLease(ctx context.Context, key sqlmem.ScopeKey, name string, wait time.Duration) (func(), error) {
	owner := randomLeaseOwner()
	deadline := time.Now().Add(wait)
	for {
		lease, err := d.acquireDocumentLease(ctx, key, name, owner, time.Now(), 30*time.Second)
		if err != nil {
			return nil, err
		}
		if lease.Acquired {
			return func() { _, _ = d.releaseDocumentLease(context.WithoutCancel(ctx), key, name, owner) }, nil
		}
		if time.Now().After(deadline) {
			return nil, errSubjectBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// resolveSubjectRef finds a subject by document id, root chunk id or natural key.
func (d *Document) resolveSubjectRef(ctx context.Context, key sqlmem.ScopeKey, ref string) (subjectRoot, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return subjectRoot{}, ErrSubjectNotFound
	}
	for _, byDocument := range []bool{true, false} {
		s, found, err := d.subjectRootOf(ctx, key, ref, byDocument)
		if err != nil || found {
			return s, err
		}
	}
	chunkID, err := d.chunkIDByNaturalKey(ctx, key, ref)
	if err != nil {
		return subjectRoot{}, err
	}
	if chunkID != "" {
		s, found, err := d.subjectRootOf(ctx, key, chunkID, false)
		if err != nil || found {
			return s, err
		}
		// A keyed chunk below a root: a fact or an unhomed entity, not a subject.
		return subjectRoot{}, ErrNotASubject
	}
	// A plain document (no key on its root), or a chunk below a root, is named but is
	// not a subject.
	if res, err := d.query(ctx, key, `SELECT 1 FROM documents WHERE id = ?
		UNION ALL SELECT 1 FROM chunks WHERE id = ? LIMIT 1`, ref, ref); err != nil {
		return subjectRoot{}, err
	} else if len(res.Rows) > 0 {
		return subjectRoot{}, ErrNotASubject
	}
	return subjectRoot{}, ErrSubjectNotFound
}

// BindSubjectPrincipal binds the subject subjectRef (a document id, root chunk id or
// natural key) to principal, or clears its binding when principal is "". It is the
// operator path: transports reach it after their own gate, never from a tool call.
//
// One principal per subject: a subject bound to someone else is refused until that
// binding is cleared. One subject per principal per scope: a principal already bound to
// another subject is refused, naming that subject. Binding as already bound, or clearing
// an unbound subject, changes nothing and reports Changed=false.
//
// It does NOT check that principal is a user of the tenant: which identities count (a
// pending user, a config-declared principal) is the transport's rule, and the transport
// holds the user store. A transport must check before calling.
func (d *Document) BindSubjectPrincipal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, subjectRef, principal string) (SubjectBinding, error) {
	principal = strings.TrimSpace(principal)
	s, err := d.resolveSubjectRef(ctx, key, subjectRef)
	if err != nil {
		return SubjectBinding{}, err
	}
	if acl := d.newDocACL(ctx, key); acl != nil {
		if refused, err := acl.document(ctx, "bind_subject_principal", "write", s.DocumentID); err != nil {
			return SubjectBinding{}, err
		} else if refused != nil {
			return SubjectBinding{}, errors.New(refused.Text)
		}
	}
	owner := randomLeaseOwner()
	lease, err := d.acquireDocumentLease(ctx, key, subjectPrincipalLease, owner, time.Now(), 30*time.Second)
	if err != nil {
		return SubjectBinding{}, err
	}
	if !lease.Acquired {
		return SubjectBinding{}, errors.New("another subject binding is being written in this scope; retry shortly")
	}
	defer func() { _, _ = d.releaseDocumentLease(context.WithoutCancel(ctx), key, subjectPrincipalLease, owner) }()

	current, err := d.subjectPrincipal(ctx, key, mscope, s.RootChunkID)
	if err != nil {
		return SubjectBinding{}, err
	}
	out := SubjectBinding{DocumentID: s.DocumentID, RootChunkID: s.RootChunkID, NaturalKey: s.NaturalKey,
		Path: d.firstDocumentPath(ctx, key, s.DocumentID), Principal: principal, Previous: current}
	if current == principal {
		return out, nil
	}
	if principal != "" {
		if current != "" {
			out.Principal = current
			return SubjectBinding{}, &SubjectBindingConflictError{SubjectBound: true, Principal: principal, Holder: out}
		}
		holders, err := d.SubjectsBoundTo(ctx, key, mscope, principal)
		if err != nil {
			return SubjectBinding{}, err
		}
		for _, h := range holders {
			if h.RootChunkID != s.RootChunkID {
				return SubjectBinding{}, &SubjectBindingConflictError{Principal: principal, Holder: h}
			}
		}
	}
	if err := d.setSubjectPrincipal(ctx, key, mscope, s.RootChunkID, principal); err != nil {
		return SubjectBinding{}, err
	}
	out.Changed = true
	return out, nil
}

// BindTenantSubjectPrincipal is BindSubjectPrincipal in the caller's tenant scope, where
// subjects are adopted. The tenant comes from the context's run identity.
func (d *Document) BindTenantSubjectPrincipal(ctx context.Context, subjectRef, principal string) (SubjectBinding, error) {
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return SubjectBinding{}, err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return SubjectBinding{}, err
	}
	return d.BindSubjectPrincipal(ctx, key, mscope, subjectRef, principal)
}

// setSubjectPrincipal writes (or, for "", removes) the binding on a subject root through
// update_chunk, so the revision moves as for any edit. It skips the operator-only guard:
// callers are the operator paths in this package (binding, erasure, a subject merge).
func (d *Document) setSubjectPrincipal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, rootChunkID, principal string) error {
	release, err := d.holdSubjectLease(ctx, key, rootChunkID, operatorSubjectLeaseWait)
	if err != nil {
		return err
	}
	defer release()
	cb, err := d.readBody(ctx, mscope, key, rootChunkID)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	if len(cb.Fields) > 0 {
		if err := json.Unmarshal(cb.Fields, &fields); err != nil {
			return fmt.Errorf("subject %s: its fields are not an object: %w", rootChunkID, err)
		}
	}
	if principal == "" {
		delete(fields, subjectPrincipalField)
	} else {
		fields[subjectPrincipalField] = principal
	}
	fj, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	rev, err := d.chunkRevision(ctx, key, rootChunkID)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"id": rootChunkID, "revision": rev, "fields": json.RawMessage(fj)})
	res, err := d.updateChunk(withPrincipalBindingWrite(ctx), key, mscope, docInput{ID: rootChunkID, Revision: &rev, Fields: fj}, raw)
	if err != nil {
		return err
	}
	if res.IsError {
		return errors.New(res.Text)
	}
	return nil
}

// SubjectsBoundTo lists the scope's subjects bound to principal, ordered by natural key.
// Each subject root's body is read: the binding is a field, not a column.
func (d *Document) SubjectsBoundTo(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, principal string) ([]SubjectBinding, error) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return nil, nil
	}
	res, err := d.query(ctx, key,
		`SELECT m.natural_key, doc.id, doc.root_chunk_id FROM documents doc
		   JOIN chunk_memory_meta m ON m.chunk_id = doc.root_chunk_id
		  WHERE coalesce(m.natural_key, '') <> ''
		  ORDER BY m.natural_key`)
	if err != nil {
		return nil, err
	}
	acl := d.newDocACL(ctx, key)
	var out []SubjectBinding
	var paths map[string][]string
	for _, r := range res.Rows {
		if len(r) < 3 {
			continue
		}
		root := asStr(r[2])
		p, err := d.subjectPrincipal(ctx, key, mscope, root)
		if err != nil {
			return nil, err
		}
		if p != principal {
			continue
		}
		if paths == nil {
			paths = d.documentPaths(ctx, key)
		}
		b := SubjectBinding{NaturalKey: asStr(r[0]), DocumentID: asStr(r[1]), RootChunkID: root, Principal: p, Readable: true}
		if names := paths[b.DocumentID]; len(names) > 0 {
			b.Path = names[0]
		}
		if acl != nil {
			ok, err := acl.readable(ctx, b.DocumentID)
			if err != nil {
				return nil, err
			}
			b.Readable = ok
		}
		out = append(out, b)
	}
	return out, nil
}

// TenantSubjectsBoundTo is SubjectsBoundTo in the context's tenant scope. A tenant with
// no document scope yet has no subjects, and asking does not create one.
func (d *Document) TenantSubjectsBoundTo(ctx context.Context, principal string) ([]SubjectBinding, error) {
	key, mscope, ok, err := d.existingTenantScope(ctx)
	if err != nil || !ok {
		return nil, err
	}
	return d.SubjectsBoundTo(ctx, key, mscope, principal)
}

// ClearTenantSubjectBindings removes principal's binding from every subject in the
// context's tenant scope and returns the subjects it cleared — the ones cleared before a
// failure too, with the error. It deletes no subject.
func (d *Document) ClearTenantSubjectBindings(ctx context.Context, principal string) ([]SubjectBinding, error) {
	key, mscope, ok, err := d.existingTenantScope(ctx)
	if err != nil || !ok {
		return nil, err
	}
	// Under the scope's binding lease, as a bind and a subject merge are: a merge moves
	// a binding from one subject to another, and a clear that listed the subjects
	// before the move would unbind the first and leave the erased id on the second.
	release, err := d.holdBindingLease(ctx, key, subjectPrincipalLease, operatorSubjectLeaseWait)
	if err != nil {
		return nil, err
	}
	defer release()
	bound, err := d.SubjectsBoundTo(ctx, key, mscope, principal)
	if err != nil {
		return nil, err
	}
	var cleared []SubjectBinding
	var failed int
	for _, b := range bound {
		// One subject that cannot be unbound (its lease busy past the wait, a store
		// fault) does not stop the rest.
		if err := d.setSubjectPrincipal(ctx, key, mscope, b.RootChunkID, ""); err != nil {
			failed++
			continue
		}
		b.Previous, b.Principal, b.Changed = b.Principal, "", true
		cleared = append(cleared, b)
	}
	if failed > 0 {
		// Counted, not named: an erasure's errors reach its audit record.
		return cleared, fmt.Errorf("%d of %d bound subject(s) could not be unbound", failed, len(bound))
	}
	return cleared, nil
}

// AnnounceTenantSubjectsErased publishes `erased` on the tenant's memory/subjects for
// each subject, after the erasure that cleared their bindings. The event names the
// subject and never the principal: the erased id must not outlive the erasure on a
// channel. Returns why nothing was announced, or "".
func (d *Document) AnnounceTenantSubjectsErased(ctx context.Context, subjects []SubjectBinding) string {
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return ""
	}
	for _, s := range subjects {
		if why := d.publishSubjectEvent(ctx, key, mscope, subjectEvent{
			Op: "erased", NaturalKey: s.NaturalKey, DocumentID: s.DocumentID, RootChunkID: s.RootChunkID,
		}); why != "" {
			// The rule is the caller's, not the subject's: every later one is withheld too.
			return why
		}
	}
	return ""
}

// existingTenantScope resolves the tenant scope, ok=false when it was never provisioned.
func (d *Document) existingTenantScope(ctx context.Context) (sqlmem.ScopeKey, store.MemoryScope, bool, error) {
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return key, mscope, false, err
	}
	exists, err := d.docSQL().ScopeExists(ctx, key)
	if err != nil || !exists {
		return key, mscope, false, err
	}
	return key, mscope, true, d.ensureSchema(ctx, key)
}

// firstDocumentPath is a document's first Path-tree name, or "".
func (d *Document) firstDocumentPath(ctx context.Context, key sqlmem.ScopeKey, docID string) string {
	if names := d.documentPaths(ctx, key)[docID]; len(names) > 0 {
		return names[0]
	}
	return ""
}

func randomLeaseOwner() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "bind:" + hex.EncodeToString(b[:])
}
