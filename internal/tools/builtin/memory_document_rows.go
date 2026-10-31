package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Document rows in the Memory keyspace, as the Memory tool may touch them.
//
// A chunk body (doc.chunk:) and a derived unit (doc.unit:) share the keyspace with
// an agent's own rows, so the Memory tool reaches them by key. Two rules keep it
// from becoming a second, weaker door to a document:
//
//   - WRITES. A raw value write to a chunk body skips the Document tool's revision
//     check and, in the tenant scope, its second grant. The value ops refuse the
//     namespace outright. Delete and supersede refuse a PROSE body; a consolidated
//     fact's body row carries provenance, and the consolidation pass retires and
//     removes facts by the id recall reported — that stays.
//   - TENANT READS. A tenant document is opened with memory_scopes AND sql_scopes
//     on the Document tool, so reading its rows through Memory takes the same two.
//     Without sql_scopes: [tenant], asking for documents is refused, and a read
//     that did not ask simply leaves them out — they were never in the Memory
//     tenant keyspace before, so such a read is unchanged. Facts, notes and traces
//     are not documents and are unaffected.

// docNamespaceHint is the next step for a refused write to a document row.
const docNamespaceHint = "Edit the chunk with the Document tool (update_chunk, delete_chunk), or use an ordinary key."

// refuseChunkBodyWrite refuses a value write (set, incr, merge, append_dedupe,
// bounded_list) to a chunk body key.
func refuseChunkBodyWrite(op, key string) (tools.Result, bool) {
	if !strings.HasPrefix(key, memrank.DocumentChunkKeyPrefix) {
		return tools.Result{}, false
	}
	return errValidation(op+": the "+memrank.DocumentChunkKeyPrefix+" namespace holds Document chunk bodies, "+
		"which are written through the Document tool so its revision check and scope grants apply", docNamespaceHint), true
}

// refuseDocumentRowRemoval refuses a delete or supersede of a derived unit or of a
// prose chunk body. A chunk body with provenance is a consolidated fact and passes;
// so does a key with no row, whose removal is a no-op.
func (m *Memory) refuseDocumentRowRemoval(ctx context.Context, op string, scope store.MemoryScope, scopeID, key string) (tools.Result, bool) {
	if res, refused := refuseUnitWrite(op, key); refused {
		return res, true
	}
	if !strings.HasPrefix(key, memrank.DocumentChunkKeyPrefix) {
		return tools.Result{}, false
	}
	// A subject root re-asserted through upsert_chunk carries provenance and passes
	// as a fact below, so removing its row would clear its operator-set binding.
	if bound, err := m.isBoundSubjectBody(ctx, scope, scopeID, key); err != nil {
		return errFrom(op+": could not read "+key+": "+err.Error(), err), true
	} else if bound {
		return errPermission(principalBindingCode+": "+op+": "+key+" is the body of a subject bound to a user "+
			"account, and only an operator clears that binding", docNamespaceHint), true
	}
	prose, err := m.isProseChunkBody(ctx, scope, scopeID, key)
	if err != nil {
		// Fail closed: the row cannot be classified, so it is not removed.
		return errFrom(op+": could not tell whether "+key+" is a document chunk: "+err.Error(), err), true
	}
	if !prose {
		return tools.Result{}, false
	}
	return errValidation(op+": "+key+" is a Document chunk body, which is removed through the Document tool "+
		"so the chunk and its text go together", docNamespaceHint), true
}

// DocumentRowWriteRefusal is the Memory tool's refusal of a value write to a
// document row — a derived unit or a chunk body — for the off-run memory key
// routes, which write the same keyspace and must refuse what this tool refuses.
func DocumentRowWriteRefusal(op, key string) (tools.Result, bool) {
	if res, refused := refuseUnitWrite(op, key); refused {
		return res, true
	}
	return refuseChunkBodyWrite(op, key)
}

// DocumentRowRemovalRefusal is the Memory tool's refusal of a removal of a
// derived unit or a prose chunk body, for the off-run memory key routes: the row
// is read from st in tenantID. A fact's body row passes, and so does a key with
// no row. A row that cannot be classified is refused with a failure that is not
// a validation one, so the caller answers it as a fault and removes nothing.
func DocumentRowRemovalRefusal(ctx context.Context, st store.Store, tenantID string, scope store.MemoryScope, scopeID, op, key string) (tools.Result, bool) {
	// isProseChunkBody reads the tenant off the run identity; this caller has no
	// run, so the identity carries only the tenant the route resolved.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: tenantID})
	return (&Memory{Store: st}).refuseDocumentRowRemoval(ctx, op, scope, scopeID, key)
}

// isBoundSubjectBody reports whether key is a chunk body whose fields carry the
// operator-set `principal`. A missing row is not one.
func (m *Memory) isBoundSubjectBody(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	if m.Store == nil {
		return false, nil
	}
	entry, err := m.Store.MemoryGet(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, key)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, err
	}
	var cb chunkBody
	_ = json.Unmarshal(entry.Value, &cb)
	return principalOf(cb.Fields) != "", nil
}

// isProseChunkBody reports whether key is a chunk body row with no provenance.
// A missing row is not one.
func (m *Memory) isProseChunkBody(ctx context.Context, scope store.MemoryScope, scopeID, key string) (bool, error) {
	if m.Store == nil {
		return false, nil
	}
	prov, err := m.Store.MemoryProvenanceGet(ctx, tools.RunIdentity(ctx).TenantID, scope, scopeID, key)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, err
	}
	return prov.IsZero(), nil
}

// tenantDocumentsReadable reports whether this call may read tenant document rows:
// any non-tenant scope, or the tenant scope with sql_scopes: [tenant] as well.
func tenantDocumentsReadable(ctx context.Context, scope store.MemoryScope) bool {
	return scope != store.MemoryScopeTenant || contains(tools.PolicyOf(ctx).SqlMem.AllowedScopes, "tenant")
}

func tenantDocumentReadRefusal(op string) tools.Result {
	return errPermission(op+": reading tenant documents through Memory needs sql_scopes: [tenant] as well as "+
		"memory_scopes: [tenant] — the same two grants the Document tool needs for a tenant document. "+
		"Missing on the agent: sql_scopes: [tenant]",
		"Search facts and notes only (leave documents out of sources), or ask an operator to grant sql_scopes: [tenant].")
}

// inDocumentNamespace reports whether key (or a key prefix) lies inside the chunk
// or unit namespace — an explicit ask for document rows.
func inDocumentNamespace(key string) bool {
	return strings.HasPrefix(key, memrank.DocumentChunkKeyPrefix) || strings.HasPrefix(key, memrank.DocumentUnitKeyPrefix)
}

// gateTenantDocumentSearch applies the tenant read rule to a search or recall. It
// returns the sources to search with, or a refusal when the call asked for
// documents explicitly: a documents source, a prefix inside a document namespace,
// or a document filter. A call that did not ask has the document class left out.
// defaultExcludesDocs is true for recall, whose empty selector already means facts
// and notes.
func gateTenantDocumentSearch(ctx context.Context, op string, scope store.MemoryScope, prefix string,
	sources []memrank.Source, filtered, defaultExcludesDocs bool) ([]memrank.Source, *tools.Result) {
	if tenantDocumentsReadable(ctx, scope) {
		return sources, nil
	}
	asked := filtered || inDocumentNamespace(prefix)
	for _, s := range sources {
		if s == memrank.SourceDocuments {
			asked = true
		}
	}
	if asked {
		r := tenantDocumentReadRefusal(op)
		return nil, &r
	}
	if len(sources) == 0 && !defaultExcludesDocs {
		return []memrank.Source{memrank.SourceFacts, memrank.SourceNotes}, nil
	}
	return sources, nil
}

// gateTenantDocumentGet applies the tenant read rule to a read by key: a derived
// unit or a prose chunk body is a document. A fact's chunk body is not.
func (m *Memory) gateTenantDocumentGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (tools.Result, bool) {
	if tenantDocumentsReadable(ctx, scope) || !inDocumentNamespace(key) {
		return tools.Result{}, false
	}
	if strings.HasPrefix(key, memrank.DocumentChunkKeyPrefix) {
		if prose, err := m.isProseChunkBody(ctx, scope, scopeID, key); err == nil && !prose {
			return tools.Result{}, false
		}
	}
	return tenantDocumentReadRefusal("get"), true
}

// dropTenantDocumentRows removes document-class rows from a listing the caller may
// not read them in: derived units, and chunk bodies with no provenance. A chunk
// body that cannot be classified is dropped too.
func (m *Memory) dropTenantDocumentRows(ctx context.Context, scope store.MemoryScope, scopeID string, entries []store.MemoryEntry) []store.MemoryEntry {
	if tenantDocumentsReadable(ctx, scope) {
		return entries
	}
	out := entries[:0:0]
	for _, e := range entries {
		if strings.HasPrefix(e.Key, memrank.DocumentUnitKeyPrefix) {
			continue
		}
		if strings.HasPrefix(e.Key, memrank.DocumentChunkKeyPrefix) {
			if prose, err := m.isProseChunkBody(ctx, scope, scopeID, e.Key); err != nil || prose {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}
