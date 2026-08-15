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
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// document_sync.go implements RFC CE document federation: bind a local document
// to a peer loomcycle (set_remote), then reconcile keyed chunks between them
// (sync). direction=pull (default) copies the peer document's keyed chunks into
// the local one; direction=push writes the LOCAL keyed chunks up to the peer.
//
// Reconciliation keys on natural_key and, per keyed chunk, carries its body +
// tags (content), its parent + sibling position (hierarchy — the parent is
// resolved by the parent's natural_key, so a keyed chunk lands under its keyed
// parent; a chunk whose parent is unkeyed re-homes to the root), and the manual
// cross-reference edges among keyed chunks (additive; auto [[name]] edges are
// regenerated from bodies on each side, so they are not synced). A chunk without
// a natural_key is EXCLUDED and counted (never reconcilable across instances).
// A diverged chunk is updated via update_chunk on the LOSING side, so the
// overwritten body is preserved in chunk_revisions (retire-not-delete via
// history) — the local document for pull, the peer for push.

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
	if in.Source == "" {
		return errResult("set_remote: missing required field: source (a document_sources name)"), nil
	}
	// Resolve against BOTH static document_sources: yaml AND the tenant-scoped
	// DocumentSourceDef substrate (dynamic, runtime-authored).
	tenantID := tools.RunIdentity(ctx).TenantID
	if _, ok := lookup.DocumentSource(ctx, d.Store, d.Cfg, tenantID, in.Source); !ok {
		return errResult(fmt.Sprintf("set_remote: unknown document source %q (declare it under document_sources: or author a DocumentSourceDef)", in.Source)), nil
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
		return d.syncPush(ctx, key, mscope, b)
	}
	return d.syncPull(ctx, key, mscope, b)
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
	// Resolve against BOTH static document_sources: yaml AND the tenant-scoped
	// DocumentSourceDef substrate (dynamic, runtime-authored).
	tenantID := tools.RunIdentity(ctx).TenantID
	ds, ok := lookup.DocumentSource(ctx, d.Store, d.Cfg, tenantID, source)
	if !ok {
		return fail(fmt.Sprintf("unknown document source %q (was it removed from document_sources: / retired?)", source))
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

// ---- side snapshots: one document's reconcilable keyed structure ----

// syncChunk is one keyed chunk's full reconcilable state on one side. `id` is
// side-local (a local UUID or a peer id); `parentNK` is the parent's natural_key
// ("" when the parent is the root or an unkeyed chunk — the reconcile re-homes
// such a chunk to the root).
type syncChunk struct {
	id       string
	nk       string
	title    string
	ctype    string
	status   string
	body     string
	parentNK string
	position int
	revision int
	tags     []string // sorted
	// withheld is true when this keyed chunk is a REFUTED fact (confidence below
	// the withhold floor). It stays in the snapshot for EXISTENCE (so the
	// reconcile never spuriously "creates" a chunk that already exists and would
	// churn its revision every run), but a withheld SOURCE chunk is not
	// propagated — the reconcile skips it, mirroring list_facts' withhold floor.
	withheld bool
}

// edgeKey identifies a manual cross-reference edge portably (both endpoints by
// natural_key). Auto [[name]]-link edges are excluded — each side regenerates
// them from chunk bodies, so syncing them would duplicate.
type edgeKey struct{ fromNK, toNK, kind string }

// sideSnapshot is one document's keyed chunks + manual edges + a count of the
// unkeyed (non-root) chunks that cannot be reconciled.
type sideSnapshot struct {
	chunks  map[string]syncChunk
	edges   map[edgeKey]bool
	unkeyed int
}

// loadLocalSnapshot reads the local document's keyed structure.
func (d *Document) loadLocalSnapshot(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID string) (*sideSnapshot, error) {
	keyed, err := d.localKeyedChunks(ctx, key, docID)
	if err != nil {
		return nil, err
	}
	idToNK := make(map[string]string, len(keyed))
	for _, kc := range keyed {
		idToNK[kc.ID] = kc.NaturalKey
	}
	snap := &sideSnapshot{chunks: make(map[string]syncChunk, len(keyed)), edges: map[edgeKey]bool{}}
	for _, kc := range keyed {
		row, ok, rerr := d.getChunkRow(ctx, key, kc.ID)
		if rerr != nil {
			return nil, rerr
		}
		if !ok {
			continue
		}
		cb, berr := d.readBody(ctx, mscope, key.ScopeID, kc.ID)
		if berr != nil {
			return nil, berr
		}
		tags, terr := d.listChunkTags(ctx, key, kc.ID)
		if terr != nil {
			return nil, terr
		}
		sort.Strings(tags)
		snap.chunks[kc.NaturalKey] = syncChunk{
			id: kc.ID, nk: kc.NaturalKey, title: kc.Title, ctype: kc.Type, status: kc.Status,
			body: cb.Body, parentNK: idToNK[row.ParentID], position: row.Position, revision: row.Revision, tags: tags,
			withheld: kc.Withheld,
		}
	}
	rows, err := d.query(ctx, key, `SELECT from_id, to_id, kind, auto FROM chunk_edges
		WHERE from_id IN (SELECT id FROM chunks WHERE document_id = ?) OR to_id IN (SELECT id FROM chunks WHERE document_id = ?)`, docID, docID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows.Rows {
		if asInt(r[3]) == 1 {
			continue // auto [[name]] edge — regenerated from bodies, not synced
		}
		fromNK, ok1 := idToNK[asStr(r[0])]
		toNK, ok2 := idToNK[asStr(r[1])]
		if ok1 && ok2 {
			snap.edges[edgeKey{fromNK: fromNK, toNK: toNK, kind: asStr(r[2])}] = true
		}
	}
	total, err := d.documentChunkCount(ctx, key, docID)
	if err != nil {
		return nil, err
	}
	snap.unkeyed = total - 1 - len(keyed)
	if snap.unkeyed < 0 {
		snap.unkeyed = 0
	}
	return snap, nil
}

// loadRemoteSnapshot reads the peer document's keyed structure over the client.
func (d *Document) loadRemoteSnapshot(ctx context.Context, client *docremote.Client, docID, rootID, scope string) (*sideSnapshot, error) {
	// include_refuted:true so the snapshot carries EVERY keyed chunk for
	// existence — a refuted remote chunk that matches a local key must be seen as
	// "exists" (else the reconcile churns it). Refuted chunks are skipped as a
	// propagation SOURCE by applyReconcile, keyed on the per-fact `withheld` flag.
	rawFacts, err := client.Do(ctx, map[string]any{"op": "list_facts", "document_id": docID, "scope": scope, "include_refuted": true, "limit": 10000})
	if err != nil {
		return nil, fmt.Errorf("list remote facts: %w", err)
	}
	facts, err := decodeRemoteFacts(rawFacts)
	if err != nil {
		return nil, fmt.Errorf("decode remote facts: %w", err)
	}
	idToNK := map[string]string{}
	for _, f := range facts {
		if f.Entity.NaturalKey != "" {
			idToNK[f.ID] = f.Entity.NaturalKey
		}
	}
	snap := &sideSnapshot{chunks: make(map[string]syncChunk, len(facts)), edges: map[edgeKey]bool{}}
	for _, f := range facts {
		nk := f.Entity.NaturalKey
		if nk == "" {
			continue
		}
		rawChunk, cerr := client.Do(ctx, map[string]any{"op": "get_chunk", "id": f.ID, "scope": scope})
		if cerr != nil {
			return nil, fmt.Errorf("fetch remote chunk %s: %w", f.ID, cerr)
		}
		var rc struct {
			Body     string   `json:"body"`
			ParentID string   `json:"parent_id"`
			Position int      `json:"position"`
			Revision int      `json:"revision"`
			Tags     []string `json:"tags"`
		}
		_ = json.Unmarshal(rawChunk, &rc)
		sort.Strings(rc.Tags)
		snap.chunks[nk] = syncChunk{
			id: f.ID, nk: nk, title: f.Title, ctype: f.Type, status: f.Status,
			body: rc.Body, parentNK: idToNK[rc.ParentID], position: rc.Position, revision: rc.Revision, tags: rc.Tags,
			withheld: f.Entity.Withheld,
		}
	}
	rawEdges, err := client.Do(ctx, map[string]any{"op": "get_edges", "document_id": docID, "scope": scope})
	if err != nil {
		return nil, fmt.Errorf("list remote edges: %w", err)
	}
	var ge struct {
		Edges []struct {
			FromID string `json:"from_id"`
			ToID   string `json:"to_id"`
			Kind   string `json:"kind"`
			Auto   bool   `json:"auto"`
		} `json:"edges"`
	}
	_ = json.Unmarshal(rawEdges, &ge)
	for _, e := range ge.Edges {
		if e.Auto {
			continue
		}
		fromNK, ok1 := idToNK[e.FromID]
		toNK, ok2 := idToNK[e.ToID]
		if ok1 && ok2 {
			snap.edges[edgeKey{fromNK: fromNK, toNK: toNK, kind: e.Kind}] = true
		}
	}
	rawAll, err := client.Do(ctx, map[string]any{"op": "query_chunks", "document_id": docID, "scope": scope, "limit": 10000})
	if err != nil {
		return nil, fmt.Errorf("count remote chunks: %w", err)
	}
	var all struct {
		Chunks []struct {
			ID string `json:"id"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal(rawAll, &all)
	for _, ch := range all.Chunks {
		if ch.ID == rootID || idToNK[ch.ID] != "" {
			continue
		}
		snap.unkeyed++
	}
	return snap, nil
}

// ---- side writers: apply a reconcile to one side (local store or peer) ----

// sideWriter mutates one side during a reconcile. Its methods are id-level; the
// direction-agnostic applyReconcile resolves natural_keys to ids.
type sideWriter interface {
	createKeyed(ctx context.Context, sc syncChunk) (id string, err error)
	updateKeyed(ctx context.Context, id string, sc syncChunk, revision int) error
	move(ctx context.Context, id, parentID string, position int) error
	ensureEdge(ctx context.Context, fromID, toID, kind string) error
}

// localWriter applies a reconcile to the local document store.
type localWriter struct {
	d      *Document
	key    sqlmem.ScopeKey
	mscope store.MemoryScope
	docID  string
}

func (w localWriter) createKeyed(ctx context.Context, sc syncChunk) (string, error) {
	res, err := w.d.upsertChunk(ctx, w.key, w.mscope, docInput{
		NaturalKey: sc.nk, DocumentID: w.docID, Title: sc.title, Type: sc.ctype, Status: sc.status, Body: sc.body, Tags: sc.tags,
	})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s", res.Text)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	return out.ID, nil
}

func (w localWriter) updateKeyed(ctx context.Context, id string, sc syncChunk, revision int) error {
	raw, _ := json.Marshal(map[string]any{"id": id, "revision": revision, "body": sc.body, "title": sc.title, "type": sc.ctype, "status": sc.status, "tags": nonNilTags(sc.tags)})
	rev := revision
	res, err := w.d.updateChunk(ctx, w.key, w.mscope, docInput{
		ID: id, Revision: &rev, Body: sc.body, Title: sc.title, Type: sc.ctype, Status: sc.status, Tags: nonNilTags(sc.tags),
	}, raw)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

func (w localWriter) move(ctx context.Context, id, parentID string, position int) error {
	pos := position
	res, err := w.d.moveChunk(ctx, w.key, docInput{ID: id, NewParentID: parentID, Position: &pos})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

func (w localWriter) ensureEdge(ctx context.Context, fromID, toID, kind string) error {
	res, err := w.d.linkChunks(ctx, w.key, docInput{FromID: fromID, ToID: toID, Kind: kind})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

// remoteWriter applies a reconcile to the peer document over the client.
type remoteWriter struct {
	client *docremote.Client
	docID  string
	scope  string
}

func (w remoteWriter) createKeyed(ctx context.Context, sc syncChunk) (string, error) {
	raw, err := w.client.Do(ctx, map[string]any{
		"op": "upsert_chunk", "document_id": w.docID, "scope": w.scope,
		"natural_key": sc.nk, "title": sc.title, "type": sc.ctype, "status": sc.status, "body": sc.body, "tags": nonNilTags(sc.tags),
	})
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.ID, nil
}

func (w remoteWriter) updateKeyed(ctx context.Context, id string, sc syncChunk, revision int) error {
	_, err := w.client.Do(ctx, map[string]any{
		"op": "update_chunk", "id": id, "revision": revision, "scope": w.scope,
		"body": sc.body, "title": sc.title, "type": sc.ctype, "status": sc.status, "tags": nonNilTags(sc.tags),
	})
	return err
}

func (w remoteWriter) move(ctx context.Context, id, parentID string, position int) error {
	_, err := w.client.Do(ctx, map[string]any{"op": "move_chunk", "id": id, "new_parent_id": parentID, "position": position, "scope": w.scope})
	return err
}

func (w remoteWriter) ensureEdge(ctx context.Context, fromID, toID, kind string) error {
	_, err := w.client.Do(ctx, map[string]any{"op": "link_chunks", "from_id": fromID, "to_id": toID, "kind": kind, "scope": w.scope})
	return err
}

// ---- the direction-agnostic reconcile ----

type reconcileCounts struct {
	created, updated, unchanged, reparented, edgesAdded, excludedWithheld int
}

// applyReconcile reconciles `source` INTO the writer's side, using `target` as
// the writer side's current state. Three passes: (1) content + tags, (2)
// hierarchy (parent + position, resolving parents by natural_key), (3) manual
// edges (additive). It never deletes — a chunk/edge present only on the target
// is left in place (mirroring the content reconcile's create-or-update posture).
func applyReconcile(ctx context.Context, w sideWriter, source, target *sideSnapshot) (reconcileCounts, error) {
	var c reconcileCounts
	// idByNK maps a natural_key to its id on the writer's side; seeded from the
	// target snapshot and extended as pass 1 creates chunks.
	idByNK := make(map[string]string, len(target.chunks))
	for nk, tc := range target.chunks {
		idByNK[nk] = tc.id
	}

	// Pass 1 — content (body, title, type, status) + tags.
	for _, nk := range sortedChunkKeys(source.chunks) {
		sc := source.chunks[nk]
		if sc.withheld {
			// A refuted SOURCE fact is not propagated (it exists in the snapshot
			// only so the target's copy isn't spuriously re-created). Skipping it
			// here — rather than filtering it out of the snapshot — is what keeps
			// sync convergent when the same key is refuted on one side.
			c.excludedWithheld++
			continue
		}
		tc, exists := target.chunks[nk]
		if !exists {
			id, err := w.createKeyed(ctx, sc)
			if err != nil {
				return c, fmt.Errorf("create %s: %w", nk, err)
			}
			idByNK[nk] = id
			c.created++
		} else if contentDiffers(sc, tc) {
			if err := w.updateKeyed(ctx, tc.id, sc, tc.revision); err != nil {
				return c, fmt.Errorf("update %s: %w", nk, err)
			}
			c.updated++
		} else {
			c.unchanged++
		}
	}

	// Pass 2 — hierarchy. Place each keyed chunk under its keyed parent at the
	// source position. A parent that isn't keyed (or isn't present on the target)
	// re-homes the child to the root — a deliberate hole, not an error. A chunk
	// created in pass 1 is ALWAYS placed here: create assigns its own position, so
	// setting the source position explicitly is what makes a sync converge in one
	// pass (and stay idempotent) rather than drift on the next run.
	for _, nk := range sortedChunkKeys(source.chunks) {
		sc := source.chunks[nk]
		if sc.withheld {
			continue // not propagated (see pass 1)
		}
		wantParentNK := ""
		if sc.parentNK != "" {
			if _, ok := idByNK[sc.parentNK]; ok {
				wantParentNK = sc.parentNK
			}
		}
		if tc, existed := target.chunks[nk]; existed && wantParentNK == tc.parentNK && sc.position == tc.position {
			continue // an existing chunk already in its source-matching place
		}
		parentID := ""
		if wantParentNK != "" {
			parentID = idByNK[wantParentNK]
		}
		if err := w.move(ctx, idByNK[nk], parentID, sc.position); err != nil {
			return c, fmt.Errorf("move %s: %w", nk, err)
		}
		c.reparented++
	}

	// Pass 3 — manual edges, additive (both endpoints must be keyed + present).
	for ek := range source.edges {
		if target.edges[ek] {
			continue
		}
		fromID, ok1 := idByNK[ek.fromNK]
		toID, ok2 := idByNK[ek.toNK]
		if !ok1 || !ok2 {
			continue
		}
		if err := w.ensureEdge(ctx, fromID, toID, ek.kind); err != nil {
			return c, fmt.Errorf("edge %s->%s: %w", ek.fromNK, ek.toNK, err)
		}
		c.edgesAdded++
	}
	return c, nil
}

// syncPull copies the peer document's keyed structure into the local document.
func (d *Document) syncPull(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, b *remoteBinding) (tools.Result, error) {
	source, err := d.loadRemoteSnapshot(ctx, b.client, b.remoteDocID, b.remoteRoot, b.scope)
	if err != nil {
		return errResult("sync: " + err.Error()), nil
	}
	target, err := d.loadLocalSnapshot(ctx, key, mscope, b.localDocID)
	if err != nil {
		return errResult("sync: " + err.Error()), nil
	}
	w := localWriter{d: d, key: key, mscope: mscope, docID: b.localDocID}
	c, err := applyReconcile(ctx, w, source, target)
	if err != nil {
		return errResult("sync: " + err.Error()), nil
	}
	return okJSON(reconcileReport("pull", b, source.unkeyed, c))
}

// syncPush writes the local document's keyed structure up to the peer document.
func (d *Document) syncPush(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, b *remoteBinding) (tools.Result, error) {
	source, err := d.loadLocalSnapshot(ctx, key, mscope, b.localDocID)
	if err != nil {
		return errResult("sync push: " + err.Error()), nil
	}
	target, err := d.loadRemoteSnapshot(ctx, b.client, b.remoteDocID, b.remoteRoot, b.scope)
	if err != nil {
		return errResult("sync push: " + err.Error()), nil
	}
	w := remoteWriter{client: b.client, docID: b.remoteDocID, scope: b.scope}
	c, err := applyReconcile(ctx, w, source, target)
	if err != nil {
		return errResult("sync push: " + err.Error()), nil
	}
	return okJSON(reconcileReport("push", b, source.unkeyed, c))
}

// reconcileReport is the shared sync envelope. excludedUnkeyed is the SOURCE's
// unkeyed count — the chunks the reconcile could not carry (remote's for pull,
// local's for push).
func reconcileReport(direction string, b *remoteBinding, excludedUnkeyed int, c reconcileCounts) map[string]any {
	return map[string]any{
		"direction":          direction,
		"source":             b.source,
		"remote_ref":         b.ref,
		"remote_document_id": b.remoteDocID,
		"local_document_id":  b.localDocID,
		"created":            c.created,
		"updated":            c.updated,
		"unchanged":          c.unchanged,
		"reparented":         c.reparented,
		"edges_added":        c.edgesAdded,
		"excluded_unkeyed":   excludedUnkeyed,
		"excluded_withheld":  c.excludedWithheld,
	}
}

// contentDiffers reports whether a source keyed chunk's content — body, title,
// type, status, or tags — differs from the target's. Title/type/status are
// included so a rename or a status change on the source actually propagates (and
// diff_remote reports it), not only a body/tag edit.
func contentDiffers(sc, tc syncChunk) bool {
	return sc.body != tc.body ||
		sc.title != tc.title ||
		sc.ctype != tc.ctype ||
		sc.status != tc.status ||
		!sameTags(sc.tags, tc.tags)
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
// same key with different bodies), same, retagged (present both, tags differ),
// and reparented (present both, parent/position differ). Manual edges present on
// only one side are counted. Unkeyed chunks on each side are counted (never
// reconcilable).
func (d *Document) diffRemote(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	b, errRes := d.resolveRemoteBinding(ctx, key, mscope, in, "diff_remote")
	if errRes != nil {
		return *errRes, nil
	}
	local, err := d.loadLocalSnapshot(ctx, key, mscope, b.localDocID)
	if err != nil {
		return errResult("diff_remote: " + err.Error()), nil
	}
	remote, err := d.loadRemoteSnapshot(ctx, b.client, b.remoteDocID, b.remoteRoot, b.scope)
	if err != nil {
		return errResult("diff_remote: " + err.Error()), nil
	}

	onlyLocal, onlyRemote, diverged, retagged, reparented := []diffEntry{}, []diffEntry{}, []diffEntry{}, []diffEntry{}, []diffEntry{}
	same := 0
	for _, nk := range sortedChunkKeys(local.chunks) {
		lc := local.chunks[nk]
		if lc.withheld {
			continue // refuted local fact — sync won't propagate it, so it's not in the diff
		}
		rc, ok := remote.chunks[nk]
		if !ok || rc.withheld { // a refuted remote fact reads as "not present" for the diff
			onlyLocal = append(onlyLocal, diffEntry{NaturalKey: nk, Title: lc.title})
			continue
		}
		// diverged = body/title/type/status differs; same = all of those identical.
		if lc.body == rc.body && lc.title == rc.title && lc.ctype == rc.ctype && lc.status == rc.status {
			same++
		} else {
			diverged = append(diverged, diffEntry{NaturalKey: nk, Title: lc.title})
		}
		if !sameTags(lc.tags, rc.tags) {
			retagged = append(retagged, diffEntry{NaturalKey: nk, Title: lc.title})
		}
		// Effective parent on each side (a parent absent on the other side is a
		// hole that resolves to the root), plus sibling position.
		if effectiveParentNK(lc, remote) != effectiveParentNK(rc, local) || lc.position != rc.position {
			reparented = append(reparented, diffEntry{NaturalKey: nk, Title: lc.title})
		}
	}
	for _, nk := range sortedChunkKeys(remote.chunks) {
		rc := remote.chunks[nk]
		if rc.withheld {
			continue
		}
		if lc, ok := local.chunks[nk]; !ok || lc.withheld {
			onlyRemote = append(onlyRemote, diffEntry{NaturalKey: nk, Title: rc.title})
		}
	}

	edgesOnlyLocal, edgesOnlyRemote := 0, 0
	for ek := range local.edges {
		if !remote.edges[ek] {
			edgesOnlyLocal++
		}
	}
	for ek := range remote.edges {
		if !local.edges[ek] {
			edgesOnlyRemote++
		}
	}

	return okJSON(map[string]any{
		"source":                  b.source,
		"remote_ref":              b.ref,
		"remote_document_id":      b.remoteDocID,
		"local_document_id":       b.localDocID,
		"only_local":              onlyLocal,
		"only_remote":             onlyRemote,
		"diverged":                diverged,
		"retagged":                retagged,
		"reparented":              reparented,
		"same":                    same,
		"edges_only_local":        edgesOnlyLocal,
		"edges_only_remote":       edgesOnlyRemote,
		"excluded_unkeyed_local":  local.unkeyed,
		"excluded_unkeyed_remote": remote.unkeyed,
	})
}

// effectiveParentNK is a chunk's parent natural_key AS IT WOULD RESOLVE against
// `other` — a parent that isn't keyed on the other side is a hole that resolves
// to the root (""), so the two sides are compared on the same footing.
func effectiveParentNK(sc syncChunk, other *sideSnapshot) string {
	if sc.parentNK == "" {
		return ""
	}
	if _, ok := other.chunks[sc.parentNK]; ok {
		return sc.parentNK
	}
	return ""
}

// ---- helpers ----

func sortedChunkKeys(m map[string]syncChunk) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameTags compares two ALREADY-SORTED tag slices as sets.
func sameTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// nonNilTags returns []string{} for a nil slice so a replace-set clears the tag
// set (update_chunk's present-key rule treats null and [] alike, but a concrete
// empty slice is unambiguous across the wire).
func nonNilTags(t []string) []string {
	if t == nil {
		return []string{}
	}
	return t
}

// remoteFact is one entity-tier chunk as list_facts returns it on either side.
type remoteFact struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Entity struct {
		NaturalKey string `json:"natural_key"`
		Withheld   bool   `json:"withheld"`
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

// keyedChunk is one local entity-tier chunk the snapshot loader reads from.
type keyedChunk struct {
	ID, NaturalKey, Title, Type, Status string
	Withheld                            bool
}

// localKeyedChunks returns ALL of the document's entity-tier chunks (those with
// a natural_key), each flagged `Withheld` when it is a refuted fact (confidence
// below the withhold floor). It deliberately does NOT filter refuted chunks out:
// the snapshot needs every keyed chunk for EXISTENCE (chunkIDByNaturalKey sees
// refuted rows, so filtering them here made the reconcile "create" an existing
// chunk on every run and churn its revision). A refuted SOURCE chunk is instead
// skipped by the reconcile itself, so it is still not propagated.
func (d *Document) localKeyedChunks(ctx context.Context, key sqlmem.ScopeKey, docID string) ([]keyedChunk, error) {
	stmt := `SELECT c.id, coalesce(c.title,''), coalesce(c.type,''), coalesce(c.status,''), m.natural_key, m.confidence
	           FROM chunks c JOIN chunk_memory_meta m ON m.chunk_id = c.id
	          WHERE c.document_id = ?`
	res, err := d.query(ctx, key, stmt, docID)
	if err != nil {
		return nil, err
	}
	out := make([]keyedChunk, 0, len(res.Rows))
	for _, r := range res.Rows {
		kc := keyedChunk{ID: asStr(r[0]), Title: asStr(r[1]), Type: asStr(r[2]), Status: asStr(r[3]), NaturalKey: asStr(r[4])}
		if kc.NaturalKey == "" {
			continue
		}
		if conf := asFloat64Ptr(r[5]); conf != nil && *conf < withholdBelowConfidence {
			kc.Withheld = true
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
