package builtin

// document_ontology_rename_steps.go — the class-rename job's steps whose names are not in
// SQL columns (RFC GQ plan P5.4), and the release it takes when it is done.
//
//	ontology_mentions       the ontology's own mentions of the class (RFC GQ 9b C12,
//	                        owner ruling 15.1), rewritten in the tenant scope before any
//	                        row moves, so the rename and its mentions are one change
//	classification_records  a document root's fields.classification applied_type and
//	                        proposed_type, in every scope, last

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// renameMention is an ontology entry whose body names a class being renamed.
type renameMention struct {
	chunkID, kind, name string
	// what says where, for the plan's notes.
	what string
}

// ontologyMentions lists the entries of read that name one of classes where the
// readers take a class name: an instance's @a, a scheme's @projects, a relation's
// @domain or @range, and a class's own ref field (an inherited one is its parent's
// mention). classes are matched under Unicode case folding.
func ontologyMentions(read OntologyRead, classes []string) []renameMention {
	var out []renameMention
	for _, in := range read.Instances {
		if in.ChunkID != "" && containsFold(classes, in.Class) {
			out = append(out, renameMention{in.ChunkID, memrank.MentionKindInstance, in.Name,
				"the instance " + strconv.Quote(in.Name) + " is of the class (@a)"})
		}
	}
	for _, s := range read.Schemes {
		if s.ChunkID != "" && anyFold(classes, s.Projects) {
			out = append(out, renameMention{s.ChunkID, memrank.MentionKindScheme, s.Name,
				"the scheme " + strconv.Quote(s.Name) + " projects the class (@projects)"})
		}
	}
	for _, r := range read.Relations {
		if r.ChunkID != "" && (anyFold(classes, r.Domain) || anyFold(classes, r.Range)) {
			out = append(out, renameMention{r.ChunkID, memrank.MentionKindRelation, r.Name,
				"the relation " + strconv.Quote(r.Name) + " names the class in @domain or @range"})
		}
	}
	for _, t := range read.Terms {
		id := t.ChunkID
		if id == "" {
			id = read.classChunks[strings.ToLower(t.Name)]
		}
		if id == "" {
			continue
		}
		for _, fs := range t.FieldSpecs {
			if fs.Type == memrank.FieldRef && containsFold(classes, fs.Range) {
				out = append(out, renameMention{id, memrank.MentionKindClass, t.Name,
					"the field " + strconv.Quote(fs.Name) + " of the class " + strconv.Quote(t.Name) + " refers to the class"})
				break
			}
		}
	}
	return out
}

func anyFold(classes, names []string) bool {
	for _, n := range names {
		if containsFold(classes, n) {
			return true
		}
	}
	return false
}

func leftOntologyMentions(d *Document, ctx context.Context, sc renameScope, from []string, _ string) (int, error) {
	read, err := d.ontologyForKey(noWrites(ctx), sc.key, sc.mscope, memrank.OntologyPath, nil)
	if err != nil {
		return 0, err
	}
	return len(ontologyMentions(read, from)), nil
}

// renameOntologyMentions rewrites every entry that names an old name so it names the
// new one. Each entry is one write, with its revision and history, like an edit.
func renameOntologyMentions(d *Document, ctx context.Context, sc renameScope, j *ontologyJob, deadline time.Time) (int, bool, error) {
	read, err := d.ontologyForKey(noWrites(ctx), sc.key, sc.mscope, memrank.OntologyPath, nil)
	if err != nil {
		return 0, false, err
	}
	moved := 0
	for _, m := range ontologyMentions(read, j.From) {
		if pastDeadline(ctx, deadline) {
			return moved, false, nil
		}
		n, err := d.rewriteMention(ctx, sc, m, j.From, j.To)
		if err != nil {
			return moved, false, err
		}
		moved += n
	}
	return moved, true, nil
}

// rewriteMention writes one entry with its mentions of from renamed to to, and an
// instance's recorded registry_key re-keyed with its subject (the subject_keys step moves
// the subject's own key). An entry the reader takes as naming the class but whose text
// the rewrite cannot find stops the job: it would otherwise never finish.
func (d *Document) rewriteMention(ctx context.Context, sc renameScope, m renameMention, from []string, to string) (int, error) {
	row, ok, err := d.getChunkRow(ctx, sc.key, m.chunkID)
	if err != nil || !ok {
		return 0, err
	}
	cb, err := d.readBody(ctx, sc.mscope, sc.key, m.chunkID)
	if err != nil {
		return 0, err
	}
	body, n := memrank.RewriteClassMentions(m.kind, cb.Body, from, to)
	if n == 0 {
		return 0, fmt.Errorf("the %s %s names the class in a form the job cannot rewrite; edit it to name %s", m.kind, strconv.Quote(m.name), strconv.Quote(to))
	}
	fields := cb.Fields
	if m.kind == memrank.MentionKindInstance {
		fields = rekeyRegistryKey(fields, from, to)
	}
	bumped, err := d.execCount(ctx, sc.key, `UPDATE chunks SET revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
		time.Now().UnixNano(), m.chunkID, row.Revision)
	if err != nil || bumped == 0 {
		// Written by someone else meanwhile: the finish check finds the mention and the
		// step runs again.
		return 0, err
	}
	if err := d.writeBody(ctx, sc.mscope, sc.key, m.chunkID, row.Type, body, fields); err != nil {
		return 0, err
	}
	if err := d.recordRevision(ctx, sc.key, m.chunkID, row.Revision+1, body); err != nil {
		return 0, err
	}
	d.publishChange(ctx, sc.mscope, docMemScopeID(sc.mscope, sc.key), row.DocumentID, "update_chunk", m.chunkID)
	return 1, nil
}

// rekeyRegistryKey moves an instance's recorded registry_key off an old class prefix.
// Other fields keep their bytes (an int64 would not survive a map[string]any).
func rekeyRegistryKey(fields json.RawMessage, from []string, to string) json.RawMessage {
	var f map[string]json.RawMessage
	if len(fields) == 0 || json.Unmarshal(fields, &f) != nil {
		return fields
	}
	var key string
	if json.Unmarshal(f["registry_key"], &key) != nil {
		return fields
	}
	i := strings.Index(key, ":")
	if i <= 0 || !containsFold(from, key[:i]) {
		return fields
	}
	f["registry_key"], _ = json.Marshal(to + key[i:])
	out, err := json.Marshal(f)
	if err != nil {
		return fields
	}
	return out
}

// renameClassificationRecords walks the scope's documents in id order from j.Doc and
// rewrites each root's classification record that names an old class. The record lives
// in the root's fields, in the Memory plane, so no SQL predicate finds it: the cursor is
// what makes the step resumable, and the step is not recounted by the finish check.
func renameClassificationRecords(d *Document, ctx context.Context, sc renameScope, j *ontologyJob, deadline time.Time) (int, bool, error) {
	moved := 0
	for {
		if pastDeadline(ctx, deadline) {
			return moved, false, nil
		}
		res, err := d.query(ctx, sc.key, `SELECT id, root_chunk_id FROM documents WHERE id > ? ORDER BY id LIMIT `+
			strconv.Itoa(renameBatch), j.Doc)
		if schemaMissing(err) {
			return moved, true, nil
		}
		if err != nil {
			return moved, false, err
		}
		if len(res.Rows) == 0 {
			j.Doc = ""
			return moved, true, nil
		}
		for _, r := range res.Rows {
			if pastDeadline(ctx, deadline) {
				return moved, false, nil
			}
			n, err := d.rewriteClassificationRecord(ctx, sc, asStr(r[1]), j.From, j.To)
			if err != nil {
				return moved, false, err
			}
			moved += n
			j.Doc = asStr(r[0])
		}
	}
}

// classificationTypeFields are the record's fields that hold a class name.
var classificationTypeFields = []string{"applied_type", "proposed_type"}

// rewriteClassificationRecord renames the class in one root's record. The fields are
// patched key by key, the rest kept as stored, and written without a new embedding (the
// body is unchanged). A root another writer changes meanwhile is read again; one that
// keeps changing is left, to read through @renamed_from.
func (d *Document) rewriteClassificationRecord(ctx context.Context, sc renameScope, root string, from []string, to string) (int, error) {
	for attempt := 0; attempt < 3; attempt++ {
		// Only the columns every scope has had from the start: a pre-split scope from an
		// older build may lack later ones (valid_at), and a missing column must not stop
		// the job there.
		res, err := d.query(ctx, sc.key, `SELECT revision, coalesce(type, '') FROM chunks WHERE id = ?`, root)
		if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
			return 0, err
		}
		row := chunkRow{Revision: asInt(res.Rows[0][0]), Type: asStr(res.Rows[0][1])}
		cb, err := d.readBody(ctx, sc.mscope, sc.key, root)
		if err != nil {
			return 0, err
		}
		fields, changed := renamedClassification(cb.Fields, from, to)
		if !changed {
			return 0, nil
		}
		bumped, err := d.execCount(ctx, sc.key, `UPDATE chunks SET revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
			time.Now().UnixNano(), root, row.Revision)
		if err != nil {
			return 0, err
		}
		if bumped == 0 {
			continue
		}
		if err := d.putFieldsKeepIndex(ctx, sc.mscope, sc.key, root, row.Type, cb.Body, fields); err != nil {
			return 0, err
		}
		return 1, nil
	}
	return 0, nil
}

// renamedClassification is fields with the classification record's class names moved
// from an old name to the new one; changed is false when the record names none.
func renamedClassification(fields json.RawMessage, from []string, to string) (json.RawMessage, bool) {
	var f map[string]json.RawMessage
	if len(fields) == 0 || json.Unmarshal(fields, &f) != nil {
		return fields, false
	}
	var rec map[string]json.RawMessage
	if raw, ok := f[classifyField]; !ok || json.Unmarshal(raw, &rec) != nil {
		return fields, false
	}
	changed := false
	for _, k := range classificationTypeFields {
		var name string
		if json.Unmarshal(rec[k], &name) == nil && name != to && containsFold(from, name) {
			rec[k], _ = json.Marshal(to)
			changed = true
		}
	}
	if !changed {
		return fields, false
	}
	var err error
	if f[classifyField], err = json.Marshal(rec); err != nil {
		return fields, false
	}
	out, err := json.Marshal(f)
	if err != nil {
		return fields, false
	}
	return out, true
}

// renameReleaseByCallerKey marks a context whose caller takes a release itself once the
// change it applies is written (an accepted change proposal takes an `apply` release),
// so a rename job finishing inside it takes none of its own.
type renameReleaseByCallerKey struct{}

func withRenameReleaseByCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, renameReleaseByCallerKey{}, true)
}

// renameJobRelease takes the `apply` release of a finished rename job (plan 4.2 step 7):
// the ontology as the job left it, with the job's state as the plan summary. None when
// the caller takes one, or when the newest release already holds this content version
// (the rename was released when it was accepted). A record, not a gate: a failure is
// logged and the job stays done.
func (d *Document) renameJobRelease(ctx context.Context, key sqlmem.ScopeKey, j *ontologyJob) {
	if by, _ := ctx.Value(renameReleaseByCallerKey{}).(bool); by {
		return
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.ContentVersion == "" {
		return
	}
	if newest, err := d.ontologyReleases(ctx, key, 1); err == nil && len(newest) == 1 && newest[0].ContentVersion == read.ContentVersion {
		return
	}
	if _, err := d.TakeOntologyRelease(ctx, OntologyReleaseApply, j.status()); err != nil {
		// The store error stays out of the log: its text can carry bound values.
		slog.WarnContext(ctx, "ontology rename job: no release was taken when it finished", "tenant", key.Tenant, "job", j.ID)
	}
}
