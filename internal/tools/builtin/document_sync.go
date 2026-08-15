package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/docremote"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// document_sync.go implements RFC CE document federation: bind a local document
// to a peer loomcycle (set_remote), then reconcile keyed chunks between them
// (sync). direction=pull (default) pulls the peer document's keyed chunks into
// the local one; direction=push writes the LOCAL keyed chunks up to the peer.
// Both reconcile on natural_key and are content-only (new chunks land under the
// target document's root; full tree reconciliation is a follow-on). A diverged
// chunk is updated via update_chunk on the LOSING side, so the overwritten body
// is preserved in chunk_revisions (retire-not-delete via history) — on the local
// document for pull, on the peer document for push. A chunk without a natural_key
// is EXCLUDED and counted.

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

// ---- sync: reconcile keyed chunks between the local document and its peer ----

func (d *Document) syncDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	direction := in.Direction
	if direction == "" {
		direction = "pull"
	}
	if direction != "pull" && direction != "push" {
		return errResult(fmt.Sprintf("sync: direction must be \"pull\" (default) or \"push\", got %q", direction)), nil
	}
	b, errRes := d.resolveRemoteBinding(ctx, key, mscope, in, "sync")
	if errRes != nil {
		return *errRes, nil
	}
	if direction == "push" {
		return d.syncPush(ctx, key, mscope, b.client, b.source, b.ref, b.localDocID, b.remoteDocID, b.scope)
	}
	return d.syncPull(ctx, key, mscope, b.client, b.source, b.ref, b.localDocID, b.remoteDocID, b.remoteRoot, b.scope)
}

// remoteBinding is the resolved connection to a bound peer document, shared by
// sync and diff_remote: an authed client, the source/ref, and the local + remote
// document ids (the remote root distinguishes structural chunks from content).
type remoteBinding struct {
	client      *docremote.Client
	source, ref string
	localDocID  string
	remoteDocID string
	remoteRoot  string
	scope       string
}

// resolveRemoteBinding runs the prologue both sync and diff_remote need: resolve
// the local document, read its _remote binding, build the peer client, and fetch
// the peer document id. On any user-facing error it returns a non-nil *Result to
// return verbatim (op names the caller so the message reads right).
func (d *Document) resolveRemoteBinding(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, op string) (*remoteBinding, *tools.Result) {
	fail := func(msg string) (*remoteBinding, *tools.Result) {
		r := errResult(op + ": " + msg)
		return nil, &r
	}
	if d.Cfg == nil {
		return fail("document sources are not configured")
	}
	localDocID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return fail(err.Error())
	}
	source, ref, err := d.readRemoteBinding(ctx, key, mscope, localDocID)
	if err != nil {
		return fail(err.Error())
	}
	if source == "" {
		return fail("this document is not bound to a remote (call set_remote first)")
	}
	ds, ok := d.Cfg.DocumentSources[source]
	if !ok {
		return fail(fmt.Sprintf("unknown document source %q (was it removed from document_sources:?)", source))
	}
	client, err := newRemoteDocumentClient(ds)
	if err != nil {
		return fail(err.Error())
	}
	scope := in.Scope
	if scope == "" {
		scope = "user"
	}
	rawDoc, err := client.Do(ctx, map[string]any{"op": "get_document", "path": ref, "scope": scope})
	if err != nil {
		return fail("fetch remote document: " + err.Error())
	}
	var remoteDoc struct {
		DocumentID  string `json:"document_id"`
		RootChunkID string `json:"root_chunk_id"`
	}
	if uerr := json.Unmarshal(rawDoc, &remoteDoc); uerr != nil || remoteDoc.DocumentID == "" {
		return fail("remote document not found at " + ref)
	}
	return &remoteBinding{
		client: client, source: source, ref: ref, localDocID: localDocID,
		remoteDocID: remoteDoc.DocumentID, remoteRoot: remoteDoc.RootChunkID, scope: scope,
	}, nil
}

// syncPull pulls the peer document's keyed chunks into the local document.
func (d *Document) syncPull(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, client *docremote.Client, source, ref, localDocID, remoteDocID, remoteRootChunkID, scope string) (tools.Result, error) {
	// The remote document's keyed chunks (list_facts returns only entity-tier
	// chunks, each carrying its natural_key).
	rawFacts, err := client.Do(ctx, map[string]any{"op": "list_facts", "document_id": remoteDocID, "scope": scope, "limit": 10000})
	if err != nil {
		return errResult("sync: list remote facts: " + err.Error()), nil
	}
	facts, err := decodeRemoteFacts(rawFacts)
	if err != nil {
		return errResult("sync: decode remote facts: " + err.Error()), nil
	}

	// All remote chunk ids, to report how many were excluded (unkeyed). The keyed
	// ids (reconciled below) and the document ROOT chunk are structural, not
	// excluded content — every document has exactly one root, so counting it as
	// "unkeyed content" would over-report by one on every sync.
	rawAll, err := client.Do(ctx, map[string]any{"op": "query_chunks", "document_id": remoteDocID, "scope": scope, "limit": 10000})
	if err != nil {
		return errResult("sync: count remote chunks: " + err.Error()), nil
	}
	var all struct {
		Chunks []struct {
			ID string `json:"id"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal(rawAll, &all)

	created, updated, unchanged := 0, 0, 0
	keyedIDs := map[string]bool{}
	for _, f := range facts {
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
		if ch.ID == remoteRootChunkID || keyedIDs[ch.ID] {
			continue // the root is structural; keyed chunks were reconciled above
		}
		excludedUnkeyed++
	}
	return okJSON(map[string]any{
		"direction":          "pull",
		"source":             source,
		"remote_ref":         ref,
		"remote_document_id": remoteDocID,
		"local_document_id":  localDocID,
		"created":            created,
		"updated":            updated,
		"unchanged":          unchanged,
		"excluded_unkeyed":   excludedUnkeyed,
	})
}

// syncPush writes the LOCAL document's keyed chunks up to the peer document. It
// mirrors syncPull inverted: a new key is created on the peer (upsert_chunk), a
// diverged one is updated on the peer via update_chunk so the peer keeps the
// overwritten body in its history, and a local chunk without a natural_key is
// excluded and counted. Local always wins here — this is a push.
func (d *Document) syncPush(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, client *docremote.Client, source, ref, localDocID, remoteDocID, scope string) (tools.Result, error) {
	// The local document's keyed chunks.
	local, err := d.localKeyedChunks(ctx, key, localDocID)
	if err != nil {
		return errResult("sync push: list local facts: " + err.Error()), nil
	}

	// The peer's existing keyed chunks (natural_key -> peer chunk id), so a
	// divergent push updates in place rather than creating a duplicate.
	rawPeer, err := client.Do(ctx, map[string]any{"op": "list_facts", "document_id": remoteDocID, "scope": scope, "limit": 10000})
	if err != nil {
		return errResult("sync push: list remote facts: " + err.Error()), nil
	}
	peerFacts, err := decodeRemoteFacts(rawPeer)
	if err != nil {
		return errResult("sync push: decode remote facts: " + err.Error()), nil
	}
	peerByNK := map[string]string{}
	for _, f := range peerFacts {
		if f.Entity.NaturalKey != "" {
			peerByNK[f.Entity.NaturalKey] = f.ID
		}
	}

	created, updated, unchanged := 0, 0, 0
	for _, lc := range local {
		lb, berr := d.readBody(ctx, mscope, key.ScopeID, lc.ID)
		if berr != nil {
			return errResult("sync push: read local " + lc.NaturalKey + ": " + berr.Error()), nil
		}
		title := lc.Title
		if title == "" {
			title = lc.NaturalKey
		}

		peerID, exists := peerByNK[lc.NaturalKey]
		if !exists {
			// New on the peer: upsert_chunk creates it under the remote doc root
			// and writes its natural_key meta.
			if _, cerr := client.Do(ctx, map[string]any{
				"op": "upsert_chunk", "document_id": remoteDocID, "scope": scope,
				"natural_key": lc.NaturalKey, "title": title, "type": lc.Type, "status": lc.Status, "body": lb.Body,
			}); cerr != nil {
				return errResult("sync push: create " + lc.NaturalKey + ": " + cerr.Error()), nil
			}
			created++
			continue
		}
		// Existing on the peer: no-op if identical, else update in place so the
		// peer preserves the overwritten body in its own chunk history.
		rawPeerChunk, gerr := client.Do(ctx, map[string]any{"op": "get_chunk", "id": peerID, "scope": scope})
		if gerr != nil {
			return errResult("sync push: fetch remote chunk " + lc.NaturalKey + ": " + gerr.Error()), nil
		}
		var pc struct {
			Body     string `json:"body"`
			Revision int    `json:"revision"`
		}
		_ = json.Unmarshal(rawPeerChunk, &pc)
		if pc.Body == lb.Body {
			unchanged++
			continue
		}
		if _, uerr := client.Do(ctx, map[string]any{
			"op": "update_chunk", "id": peerID, "revision": pc.Revision, "scope": scope,
			"body": lb.Body, "title": title, "type": lc.Type, "status": lc.Status,
		}); uerr != nil {
			return errResult("sync push: update " + lc.NaturalKey + ": " + uerr.Error()), nil
		}
		updated++
	}

	// Local non-root chunks without a natural_key can't be pushed. Every document
	// has exactly one root chunk (counted in the total), so subtract it plus the
	// keyed chunks reconciled above.
	total, err := d.documentChunkCount(ctx, key, localDocID)
	if err != nil {
		return errResult("sync push: " + err.Error()), nil
	}
	excludedUnkeyed := total - 1 - len(local)
	if excludedUnkeyed < 0 {
		excludedUnkeyed = 0
	}
	return okJSON(map[string]any{
		"direction":          "push",
		"source":             source,
		"remote_ref":         ref,
		"remote_document_id": remoteDocID,
		"local_document_id":  localDocID,
		"created":            created,
		"updated":            updated,
		"unchanged":          unchanged,
		"excluded_unkeyed":   excludedUnkeyed,
	})
}

// ---- diff_remote: dry-run change set between the local document and its peer ----

// diffEntry names one keyed chunk in a diff bucket.
type diffEntry struct {
	NaturalKey string `json:"natural_key"`
	Title      string `json:"title"`
}

// diffRemote reports what a sync WOULD change, without touching either side. It
// aligns keyed chunks by natural_key into only_local (a push would create these
// on the peer), only_remote (a pull would create these locally), diverged (the
// same key with different bodies — pull would overwrite local, push the peer),
// and same. Unkeyed chunks on each side are counted (never reconcilable).
func (d *Document) diffRemote(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	b, errRes := d.resolveRemoteBinding(ctx, key, mscope, in, "diff_remote")
	if errRes != nil {
		return *errRes, nil
	}

	// Local keyed chunks (natural_key -> body).
	localChunks, err := d.localKeyedChunks(ctx, key, b.localDocID)
	if err != nil {
		return errResult("diff_remote: list local facts: " + err.Error()), nil
	}
	type kv struct{ title, body string }
	localByNK := make(map[string]kv, len(localChunks))
	for _, lc := range localChunks {
		lb, berr := d.readBody(ctx, mscope, key.ScopeID, lc.ID)
		if berr != nil {
			return errResult("diff_remote: read local " + lc.NaturalKey + ": " + berr.Error()), nil
		}
		localByNK[lc.NaturalKey] = kv{title: lc.Title, body: lb.Body}
	}

	// Remote keyed chunks (natural_key -> body), plus the ids to exclude the root
	// and the keyed chunks from the unkeyed count.
	rawFacts, err := b.client.Do(ctx, map[string]any{"op": "list_facts", "document_id": b.remoteDocID, "scope": b.scope, "limit": 10000})
	if err != nil {
		return errResult("diff_remote: list remote facts: " + err.Error()), nil
	}
	facts, err := decodeRemoteFacts(rawFacts)
	if err != nil {
		return errResult("diff_remote: decode remote facts: " + err.Error()), nil
	}
	remoteByNK := make(map[string]kv, len(facts))
	keyedRemoteIDs := map[string]bool{}
	for _, f := range facts {
		if f.Entity.NaturalKey == "" {
			continue
		}
		keyedRemoteIDs[f.ID] = true
		rawChunk, cerr := b.client.Do(ctx, map[string]any{"op": "get_chunk", "id": f.ID, "scope": b.scope})
		if cerr != nil {
			return errResult("diff_remote: fetch remote chunk " + f.ID + ": " + cerr.Error()), nil
		}
		var rc struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(rawChunk, &rc)
		remoteByNK[f.Entity.NaturalKey] = kv{title: f.Title, body: rc.Body}
	}

	onlyLocal, onlyRemote, diverged := []diffEntry{}, []diffEntry{}, []diffEntry{}
	same := 0
	for nk, lv := range localByNK {
		rv, ok := remoteByNK[nk]
		if !ok {
			onlyLocal = append(onlyLocal, diffEntry{NaturalKey: nk, Title: lv.title})
			continue
		}
		if rv.body == lv.body {
			same++
		} else {
			diverged = append(diverged, diffEntry{NaturalKey: nk, Title: lv.title})
		}
	}
	for nk, rv := range remoteByNK {
		if _, ok := localByNK[nk]; !ok {
			onlyRemote = append(onlyRemote, diffEntry{NaturalKey: nk, Title: rv.title})
		}
	}
	sortDiff := func(s []diffEntry) { sort.Slice(s, func(i, j int) bool { return s[i].NaturalKey < s[j].NaturalKey }) }
	sortDiff(onlyLocal)
	sortDiff(onlyRemote)
	sortDiff(diverged)

	// Unkeyed (non-root) chunks on each side — never reconcilable.
	localTotal, err := d.documentChunkCount(ctx, key, b.localDocID)
	if err != nil {
		return errResult("diff_remote: " + err.Error()), nil
	}
	excludedLocal := localTotal - 1 - len(localChunks)
	if excludedLocal < 0 {
		excludedLocal = 0
	}
	rawAll, err := b.client.Do(ctx, map[string]any{"op": "query_chunks", "document_id": b.remoteDocID, "scope": b.scope, "limit": 10000})
	if err != nil {
		return errResult("diff_remote: count remote chunks: " + err.Error()), nil
	}
	var all struct {
		Chunks []struct {
			ID string `json:"id"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal(rawAll, &all)
	excludedRemote := 0
	for _, ch := range all.Chunks {
		if ch.ID == b.remoteRoot || keyedRemoteIDs[ch.ID] {
			continue
		}
		excludedRemote++
	}

	return okJSON(map[string]any{
		"source":                  b.source,
		"remote_ref":              b.ref,
		"remote_document_id":      b.remoteDocID,
		"local_document_id":       b.localDocID,
		"only_local":              onlyLocal,
		"only_remote":             onlyRemote,
		"diverged":                diverged,
		"same":                    same,
		"excluded_unkeyed_local":  excludedLocal,
		"excluded_unkeyed_remote": excludedRemote,
	})
}

// ---- helpers ----

// remoteFact is one entity-tier chunk as list_facts returns it on either side.
type remoteFact struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Entity struct {
		NaturalKey string `json:"natural_key"`
	} `json:"entity"`
}

func decodeRemoteFacts(raw json.RawMessage) ([]remoteFact, error) {
	var facts struct {
		Facts []remoteFact `json:"facts"`
	}
	if err := json.Unmarshal(raw, &facts); err != nil {
		return nil, err
	}
	return facts.Facts, nil
}

// keyedChunk is one local entity-tier chunk push reconciles from.
type keyedChunk struct {
	ID, NaturalKey, Title, Type, Status string
}

// localKeyedChunks returns the document's entity-tier chunks (those with a
// natural_key), excluding refuted ones — the same withhold floor list_facts
// applies, so push propagates only the facts the local store itself surfaces.
func (d *Document) localKeyedChunks(ctx context.Context, key sqlmem.ScopeKey, docID string) ([]keyedChunk, error) {
	where := "c.document_id = ?"
	args := []any{docID}
	if wc := withholdClause("m.confidence", false); wc != "" {
		where += " AND " + wc
	}
	stmt := `SELECT c.id, coalesce(c.title,''), coalesce(c.type,''), coalesce(c.status,''), m.natural_key
	           FROM chunks c JOIN chunk_memory_meta m ON m.chunk_id = c.id
	          WHERE ` + where
	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		return nil, err
	}
	out := make([]keyedChunk, 0, len(res.Rows))
	for _, r := range res.Rows {
		kc := keyedChunk{ID: asStr(r[0]), Title: asStr(r[1]), Type: asStr(r[2]), Status: asStr(r[3]), NaturalKey: asStr(r[4])}
		if kc.NaturalKey == "" {
			continue
		}
		out = append(out, kc)
	}
	return out, nil
}

func (d *Document) documentChunkCount(ctx context.Context, key sqlmem.ScopeKey, docID string) (int, error) {
	res, err := d.query(ctx, key, `SELECT count(*) FROM chunks WHERE document_id = ?`, docID)
	if err != nil {
		return 0, err
	}
	if len(res.Rows) == 0 {
		return 0, nil
	}
	n, _ := asInt64(res.Rows[0][0])
	return int(n), nil
}

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
