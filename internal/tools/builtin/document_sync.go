package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/docremote"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// document_sync.go implements RFC CE document federation (CE-1, PULL): bind a
// local document to a peer loomcycle (set_remote), then pull the peer document's
// keyed chunks into it (sync). Reconcile keys on natural_key; a diverged chunk
// is updated via update_chunk (the losing body is preserved in chunk_revisions —
// retire-not-delete via history); a chunk without a natural_key is EXCLUDED and
// counted. CE-1 is content-only (new chunks land under the doc root; full tree
// reconciliation is a follow-on).

const remoteDocumentTimeout = 30 * time.Second

// newRemoteDocumentClient builds a peer-document client from a source def,
// reusing the RFC CD Part B remote plumbing: an SSRF-guarded client whose
// private-host allowlist is the peer's own host, and the credential-env
// allowlist gate (resolveCredentialEnv, defined in memory.go).
func newRemoteDocumentClient(ds config.DocumentSource) (*docremote.Client, error) {
	host := ""
	if u, err := url.Parse(ds.Config.BaseURL); err == nil {
		host = u.Hostname()
	}
	var allow []string
	if host != "" {
		allow = []string{host}
	}
	client := netguard.NewGuardedClient(remoteDocumentTimeout, allow)
	return docremote.New(docremote.Options{
		BaseURL:          ds.Config.BaseURL,
		APIVersion:       ds.Config.APIVersion,
		DefaultAPIKeyEnv: ds.Config.APIKeyEnv,
		TenancyKind:      ds.TenancyStrategy.Kind,
		EnvPattern:       ds.TenancyStrategy.EnvPattern,
		KeyResolver:      resolveCredentialEnv,
		HTTPClient:       client,
	})
}

// ---- set_remote: bind a local document to a peer source ----

func (d *Document) setRemote(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if d.Cfg == nil {
		return errResult("set_remote: document sources are not configured (no document_sources: in operator yaml)"), nil
	}
	if in.Source == "" {
		return errResult("set_remote: missing required field: source (a document_sources name)"), nil
	}
	if _, ok := d.Cfg.DocumentSources[in.Source]; !ok {
		return errResult(fmt.Sprintf("set_remote: unknown document source %q (declare it under document_sources:)", in.Source)), nil
	}
	if in.RemoteRef == "" {
		return errResult("set_remote: missing required field: remote_ref (the remote document's path or id)"), nil
	}
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return errResult("set_remote: " + err.Error()), nil
	}
	root, err := d.documentRootChunk(ctx, key, docID)
	if err != nil {
		return errResult("set_remote: " + err.Error()), nil
	}
	// Merge the binding into the root chunk's fields (round-trips via readBody /
	// get_document). Read the current fields so we don't blank a color scheme.
	cb, err := d.readBody(ctx, mscope, key.ScopeID, root)
	if err != nil {
		return errResult("set_remote: read root fields: " + err.Error()), nil
	}
	fields := map[string]any{}
	if len(cb.Fields) > 0 {
		_ = json.Unmarshal(cb.Fields, &fields)
	}
	fields["_remote"] = map[string]any{"source": in.Source, "ref": in.RemoteRef}
	fj, _ := json.Marshal(fields)
	rev, err := d.chunkRevision(ctx, key, root)
	if err != nil {
		return errResult("set_remote: " + err.Error()), nil
	}
	// raw carries only the keys we set, so update_chunk touches ONLY fields.
	raw, _ := json.Marshal(map[string]any{"id": root, "revision": rev, "fields": json.RawMessage(fj)})
	res, uerr := d.updateChunk(ctx, key, mscope, docInput{ID: root, Revision: &rev, Fields: json.RawMessage(fj)}, raw)
	if uerr != nil {
		return errResult("set_remote: " + uerr.Error()), nil
	}
	if res.IsError {
		return res, nil
	}
	return okJSON(map[string]any{"document_id": docID, "source": in.Source, "remote_ref": in.RemoteRef, "bound": true})
}

// ---- sync: pull the bound peer document's keyed chunks into the local one ----

func (d *Document) syncDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if d.Cfg == nil {
		return errResult("sync: document sources are not configured"), nil
	}
	localDocID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return errResult("sync: " + err.Error()), nil
	}
	source, ref, err := d.readRemoteBinding(ctx, key, mscope, localDocID)
	if err != nil {
		return errResult("sync: " + err.Error()), nil
	}
	if source == "" {
		return errResult("sync: this document is not bound to a remote (call set_remote first)"), nil
	}
	ds, ok := d.Cfg.DocumentSources[source]
	if !ok {
		return errResult(fmt.Sprintf("sync: unknown document source %q (was it removed from document_sources:?)", source)), nil
	}
	client, err := newRemoteDocumentClient(ds)
	if err != nil {
		return errResult("sync: " + err.Error()), nil
	}

	scope := in.Scope
	if scope == "" {
		scope = "user"
	}

	// 1. Resolve the remote document id.
	rawDoc, err := client.Do(ctx, map[string]any{"op": "get_document", "path": ref, "scope": scope})
	if err != nil {
		return errResult("sync: fetch remote document: " + err.Error()), nil
	}
	var remoteDoc struct {
		DocumentID  string `json:"document_id"`
		RootChunkID string `json:"root_chunk_id"`
	}
	if uerr := json.Unmarshal(rawDoc, &remoteDoc); uerr != nil || remoteDoc.DocumentID == "" {
		return errResult("sync: remote document not found at " + ref), nil
	}

	// 2. The remote document's keyed chunks (list_facts returns only entity-tier
	//    chunks, each carrying its natural_key).
	rawFacts, err := client.Do(ctx, map[string]any{"op": "list_facts", "document_id": remoteDoc.DocumentID, "scope": scope, "limit": 10000})
	if err != nil {
		return errResult("sync: list remote facts: " + err.Error()), nil
	}
	var facts struct {
		Facts []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Type   string `json:"type"`
			Status string `json:"status"`
			Entity struct {
				NaturalKey string `json:"natural_key"`
			} `json:"entity"`
		} `json:"facts"`
	}
	if uerr := json.Unmarshal(rawFacts, &facts); uerr != nil {
		return errResult("sync: decode remote facts: " + uerr.Error()), nil
	}

	// 3. All remote chunk ids, to report how many were excluded (unkeyed). The
	//    keyed ids (reconciled below) and the document ROOT chunk are structural,
	//    not excluded content — every document has exactly one root, so counting it
	//    as "unkeyed content" would over-report by one on every sync.
	rawAll, err := client.Do(ctx, map[string]any{"op": "query_chunks", "document_id": remoteDoc.DocumentID, "scope": scope, "limit": 10000})
	if err != nil {
		return errResult("sync: count remote chunks: " + err.Error()), nil
	}
	var all struct {
		Chunks []struct {
			ID string `json:"id"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal(rawAll, &all)

	// 4. Reconcile each keyed remote chunk into the local document.
	created, updated, unchanged := 0, 0, 0
	keyedIDs := map[string]bool{}
	for _, f := range facts.Facts {
		nk := f.Entity.NaturalKey
		if nk == "" {
			continue // an entity chunk without a natural_key is not reconcilable
		}
		keyedIDs[f.ID] = true

		rawChunk, cerr := client.Do(ctx, map[string]any{"op": "get_chunk", "id": f.ID, "scope": scope})
		if cerr != nil {
			return errResult("sync: fetch remote chunk " + f.ID + ": " + cerr.Error()), nil
		}
		var rc struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(rawChunk, &rc)

		title := f.Title
		if title == "" {
			title = nk
		}

		localID, lerr := d.chunkIDByNaturalKey(ctx, key, nk)
		if lerr != nil {
			return errResult("sync: local lookup: " + lerr.Error()), nil
		}
		if localID == "" {
			// New: create the keyed chunk under the local document root.
			res, _ := d.upsertChunk(ctx, key, mscope, docInput{
				NaturalKey: nk, DocumentID: localDocID, Title: title, Type: f.Type, Status: f.Status, Body: rc.Body,
			})
			if res.IsError {
				return errResult("sync: create " + nk + ": " + res.Text), nil
			}
			created++
			continue
		}
		// Existing: no-op if identical, else update in place (history-preserving).
		lb, berr := d.readBody(ctx, mscope, key.ScopeID, localID)
		if berr != nil {
			return errResult("sync: read local " + nk + ": " + berr.Error()), nil
		}
		if lb.Body == rc.Body {
			unchanged++
			continue
		}
		rev, rerr := d.chunkRevision(ctx, key, localID)
		if rerr != nil {
			return errResult("sync: " + rerr.Error()), nil
		}
		raw, _ := json.Marshal(map[string]any{"id": localID, "revision": rev, "body": rc.Body, "title": title, "type": f.Type, "status": f.Status})
		res, _ := d.updateChunk(ctx, key, mscope, docInput{
			ID: localID, Revision: &rev, Body: rc.Body, Title: title, Type: f.Type, Status: f.Status,
		}, raw)
		if res.IsError {
			return errResult("sync: update " + nk + ": " + res.Text), nil
		}
		updated++
	}

	excludedUnkeyed := 0
	for _, ch := range all.Chunks {
		if ch.ID == remoteDoc.RootChunkID || keyedIDs[ch.ID] {
			continue // the root is structural; keyed chunks were reconciled above
		}
		excludedUnkeyed++
	}
	return okJSON(map[string]any{
		"source":             source,
		"remote_ref":         ref,
		"remote_document_id": remoteDoc.DocumentID,
		"local_document_id":  localDocID,
		"created":            created,
		"updated":            updated,
		"unchanged":          unchanged,
		"excluded_unkeyed":   excludedUnkeyed,
	})
}

// ---- helpers ----

func (d *Document) documentRootChunk(ctx context.Context, key sqlmem.ScopeKey, docID string) (string, error) {
	res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ?`, docID)
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 {
		return "", fmt.Errorf("document %q not found in this scope", docID)
	}
	return asStr(res.Rows[0][0]), nil
}

func (d *Document) chunkRevision(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (int, error) {
	res, err := d.query(ctx, key, `SELECT revision FROM chunks WHERE id = ?`, chunkID)
	if err != nil {
		return 0, err
	}
	if len(res.Rows) == 0 {
		return 0, fmt.Errorf("chunk %q not found", chunkID)
	}
	rev, _ := asInt64(res.Rows[0][0])
	return int(rev), nil
}

func (d *Document) readRemoteBinding(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID string) (source, ref string, err error) {
	root, err := d.documentRootChunk(ctx, key, docID)
	if err != nil {
		return "", "", err
	}
	cb, err := d.readBody(ctx, mscope, key.ScopeID, root)
	if err != nil {
		return "", "", err
	}
	if len(cb.Fields) == 0 {
		return "", "", nil
	}
	var f struct {
		Remote struct {
			Source string `json:"source"`
			Ref    string `json:"ref"`
		} `json:"_remote"`
	}
	_ = json.Unmarshal(cb.Fields, &f)
	return f.Remote.Source, f.Remote.Ref, nil
}
