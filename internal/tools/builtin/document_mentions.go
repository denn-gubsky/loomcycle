package builtin

// document_mentions.go — `mentions`: prose that names a subject (RFC FN P3b §6.1).
//
// A fact is ABOUT its subject (`about`); a passage that names the subject in a
// [[link]] MENTIONS it. Keeping the two apart is what lets a subject's dossier say
// "these are the facts about Ada" and, separately, "Ada is mentioned in these
// passages" — and lets a walk skip mentions by default, since a name in passing is
// not a relation worth following.

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// subjectNodesWhere selects the chunk ids that are subject (entity) nodes: a chunk
// carrying a subject whose natural key is an entity key (`<type>:<slug>`), not a
// fact key (`s|p|o`) — and not a fact a person asked to remember, which carries
// the subject it is about under a key of its own (rememberedKeyPrefix).
const subjectNodesWhere = `SELECT chunk_id FROM chunk_memory_meta
	WHERE subject IS NOT NULL AND subject <> '' AND natural_key IS NOT NULL AND natural_key NOT LIKE '%|%'
	  AND natural_key NOT LIKE '` + rememberedKeyPrefix + `%'`

// subjectNodes reports which of ids are subject nodes. A fault answers "none", so
// the link keeps today's kind.
func (d *Document) subjectNodes(ctx context.Context, key sqlmem.ScopeKey, ids []string) map[string]bool {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := d.query(ctx, key, subjectNodesWhere+` AND chunk_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return out
	}
	for _, r := range res.Rows {
		out[asStr(r[0])] = true
	}
	return out
}

// mentionEvidence records where in the body the subject is named: the first
// [[…]] that resolves to it, with its character span. Best-effort, like the link.
//
// The link is matched whole, as the parser reads it — `[[Ada]]` or `[[Ada|…]]`,
// never the `[[Ada Lovelace notes]]` that merely starts with the name.
func (d *Document) mentionEvidence(ctx context.Context, key sqlmem.ScopeKey, edgeID, fromChunkID, body, to string) {
	// The guarded insert writes no edge when the pair already has a mention, and
	// evidence for an edge that was never written would be an orphan.
	if res, err := d.query(ctx, key, `SELECT 1 FROM chunk_edges WHERE id = ?`, edgeID); err != nil || len(res.Rows) == 0 {
		return
	}
	resolved := map[string]bool{}
	for _, m := range nameLinkRe.FindAllStringSubmatchIndex(body, -1) {
		if m[0] > 0 && body[m[0]-1] == '!' {
			continue // an embed, which parseNameLinks skips too
		}
		name := body[m[2]:m[3]]
		if i := strings.IndexByte(name, '|'); i >= 0 {
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		hit, seen := resolved[name]
		if !seen {
			id, ok := d.resolveLinkTarget(ctx, key, name)
			hit = ok && id == to
			resolved[name] = hit
		}
		if !hit {
			continue
		}
		quote := body[m[0]:m[1]]
		start := utf8.RuneCountInString(body[:m[0]])
		end := start + utf8.RuneCountInString(quote)
		_ = d.exec(ctx, key, `INSERT INTO chunk_edge_evidence (id, edge_id, chunk_id, quote, char_start, char_end, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, newDocID(), edgeID, fromChunkID, quote, start, end, time.Now().UnixNano())
		return
	}
}

// migrateMentionKinds re-kinds [[name]] links that were written as `references`
// to a subject node before mentions existed. Idempotent: a link that already has
// a `mentions` twin is dropped rather than re-kinded, so the unique key holds.
func (d *Document) migrateMentionKinds(ctx context.Context, key sqlmem.ScopeKey) error {
	if err := d.exec(ctx, key, `UPDATE chunk_edges SET kind = 'mentions'
		WHERE kind = 'references' AND auto = 1 AND to_id IN (`+subjectNodesWhere+`)
		  AND NOT EXISTS (SELECT 1 FROM chunk_edges m
		                   WHERE m.from_id = chunk_edges.from_id AND m.to_id = chunk_edges.to_id AND m.kind = 'mentions')`); err != nil {
		return err
	}
	return d.deleteEdgesWhere(ctx, key, "", `kind = 'references' AND auto = 1 AND to_id IN (`+subjectNodesWhere+`)`)
}

// mentionedIn lists the chunks that mention a subject node (newest first,
// bounded), for a dossier read.
func (d *Document) mentionedIn(ctx context.Context, key sqlmem.ScopeKey, subjectID string, limit int) ([]map[string]any, error) {
	res, err := d.query(ctx, key, `SELECT c.id, c.title, c.document_id FROM chunk_edges e JOIN chunks c ON c.id = e.from_id
		WHERE e.to_id = ? AND e.kind = 'mentions' AND `+edgeLiveClause+`
		ORDER BY e.created_at DESC LIMIT ?`, subjectID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, map[string]any{"chunk_id": asStr(r[0]), "title": asStr(r[1]), "document_id": asStr(r[2])})
	}
	return out, nil
}

// subjectEvent is one message on a scope's `memory/subjects` channel: a subject
// changed identity (RFC FR §2). The ids are what a consumer acts on — it selects
// statements by document — and it trusts nothing else in the payload.
type subjectEvent struct {
	Op                  string // adopted (merged, retyped, removed, erased: later phases)
	NaturalKey          string
	DocumentID          string
	RootChunkID         string
	SurvivingKey        string // merged only
	SurvivingDocumentID string // merged only
	OldType, NewType    string // retyped only
}

// publishSubjectEvent tells the scope's `memory/subjects` channel that a subject
// changed identity, so whatever indexes relations by subject (a links agent) can
// re-point or expire them rather than poll for it. It is called after the write
// that changed the subject has committed.
//
// Everyone on the scope's channel reads the event, and it names the subject. So,
// as for documents/ingested, it is published only when the caller's access-control
// rules hide no part of the scope from reading. Returns why nothing was published,
// or "" (also when there is no bus: a tool built without one publishes nothing).
func (d *Document) publishSubjectEvent(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, ev subjectEvent) string {
	if d.Bus == nil || ev.NaturalKey == "" {
		return ""
	}
	if limited, err := d.newDocACL(ctx, key).limitsReading(ctx); err != nil || limited {
		return "memory/subjects was not published: the caller's access-control rules hide part of this scope from reading, so the event could name this subject to subscribers who may not read it"
	}
	if ev.DocumentID == "" && ev.RootChunkID != "" {
		if res, err := d.query(ctx, key, `SELECT document_id FROM chunks WHERE id = ?`, ev.RootChunkID); err == nil && len(res.Rows) > 0 {
			ev.DocumentID = asStr(res.Rows[0][0])
		}
	}
	// The scope it is published at, as on documents/ingested.
	msg := map[string]any{"op": ev.Op, "subject_natural_key": ev.NaturalKey, "scope": string(mscope), "timestamp": time.Now().UnixNano()}
	for k, v := range map[string]string{
		"document_id": ev.DocumentID, "root_chunk_id": ev.RootChunkID,
		"surviving_key": ev.SurvivingKey, "surviving_document_id": ev.SurvivingDocumentID,
		"old_type": ev.OldType, "new_type": ev.NewType,
	} {
		if v != "" {
			msg[k] = v
		}
	}
	payload, _ := json.Marshal(msg)
	w := d.Writer
	if w == nil {
		w = &channels.StorePublisher{Store: d.Store, Bus: d.Bus}
	}
	_, _ = w.Write(ctx, channels.WriteRequest{
		Channel:     "memory/subjects",
		TenantID:    tools.RunIdentity(ctx).TenantID,
		Scope:       mscope,
		ScopeID:     docMemScopeID(mscope, key),
		Payload:     payload,
		PublishedBy: tools.RunIdentity(ctx).UserID,
		MaxMessages: 256,
	})
	return ""
}
