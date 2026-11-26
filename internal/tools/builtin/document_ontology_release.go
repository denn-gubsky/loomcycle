package builtin

// Ontology releases (RFC GQ P3.3): a copy of the tenant ontology taken when it is
// confirmed and when an accepted change is applied, so that every content version a row
// was stamped with (P3.2) has a release that says what it declared.
//
// WHY A COPY. The ontology's bodies and fields live in the Memory key/value plane, not in
// SQL (document.go's chunkBody), and an edit overwrites them; chunk_revisions keeps
// bodies only, and only for some writers. A release therefore copies every chunk it needs
// into three per-scope tables in the tenant scope:
//
//	ontology_releases         one header per release; readers see complete ones only
//	ontology_release_entries  one row per chunk of the ontology document
//	ontology_release_bodies   each (body, fields) once, by its hash
//
// Bodies are kept once per hash because a release copies the whole document: a scheme
// of thousands of concepts across 50 releases would otherwise be stored 50 times.
//
// WHEN (plan 3.2, owner ruling Q2 of 2026-11-25). A confirm that flips the ontology to
// confirmed takes a `confirm` release; an accepted change proposal takes an `apply`
// release. Before either, when the content version in force is not the newest release's
// (or there is no release yet), a `catch_up` release is taken first, so a direct edit is
// never attributed to the next batch. A release is a record, not a gate: a failure to
// take one is reported and never undoes the confirm or the accept.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Release reasons, as stored.
const (
	OntologyReleaseConfirm  = "confirm"
	OntologyReleaseApply    = "apply"
	OntologyReleaseRollback = "rollback"
	OntologyReleaseCatchUp  = "catch_up"
)

const (
	// ontologyReleasesKept is how many complete releases are kept; older ones go, with
	// their entries and the bodies no kept entry shares.
	ontologyReleasesKept = 50
	// ontologyReleaseBatch is how many entries one transaction writes.
	ontologyReleaseBatch = 500
	// ontologyReleaseSummaryCap bounds a release's plan_summary.
	ontologyReleaseSummaryCap = 8 << 10
	// ontologyReleaseRetries is how many times a copy the ontology moved under is taken
	// again before the release gives up.
	ontologyReleaseRetries = 2
)

// ontologyReleasesDDL: one header per release. complete is 0 while the entries are being
// written and 1 once they all are; a reader lists complete releases only, and the next
// release removes an incomplete one. seq is max+1, taken under the release lock, which
// holds across replicas (holdOntologyReleaseLock).
const ontologyReleasesDDL = `CREATE TABLE IF NOT EXISTS ontology_releases (
		seq BIGINT PRIMARY KEY, content_version TEXT NOT NULL, fingerprint TEXT NOT NULL,
		reason TEXT NOT NULL, actor TEXT, created_at BIGINT NOT NULL,
		plan_summary TEXT, entries INTEGER NOT NULL, complete INTEGER NOT NULL,
		export_md TEXT)`

// ontologyReleaseEntriesDDL: one row per chunk of the ontology document at the release.
// document_id is there for schemes kept in documents of their own (RFC GM D16c), which
// do not exist yet: today every entry names the one ontology document.
const ontologyReleaseEntriesDDL = `CREATE TABLE IF NOT EXISTS ontology_release_entries (
		release_seq BIGINT NOT NULL, document_id TEXT NOT NULL, chunk_id TEXT NOT NULL,
		parent_id TEXT, position INTEGER NOT NULL, type TEXT, status TEXT,
		title TEXT NOT NULL, body_sha TEXT NOT NULL,
		PRIMARY KEY (release_seq, chunk_id))`

// ontologyReleaseEntriesBodyDDL serves retention's "which bodies does any entry still
// use".
const ontologyReleaseEntriesBodyDDL = `CREATE INDEX IF NOT EXISTS ontology_release_entries_body
		ON ontology_release_entries(body_sha)`

// ontologyReleaseBodiesDDL: each (body, fields) once. fields is the chunk's fields JSON
// as stored, "" when it has none.
const ontologyReleaseBodiesDDL = `CREATE TABLE IF NOT EXISTS ontology_release_bodies (
		sha TEXT PRIMARY KEY, body TEXT NOT NULL, fields TEXT NOT NULL)`

// OntologyRelease is one complete release's header.
type OntologyRelease struct {
	Seq            int64  `json:"seq"`
	ContentVersion string `json:"content_version"`
	Fingerprint    string `json:"fingerprint"`
	Reason         string `json:"reason"`
	Actor          string `json:"actor,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	Entries        int    `json:"entries"`
}

// ErrOntologyMovedDuringRelease: every attempt at the copy saw the ontology change under
// it, so no release was taken.
var ErrOntologyMovedDuringRelease = errors.New("the ontology kept changing while the release was copied")

// releaseEntry is one chunk as a release keeps it.
type releaseEntry struct {
	chunkID, parentID, typ, status, title string
	position                              int
	body, fields, sha                     string
}

// releaseCopy is everything one release writes, read while the ontology stood still.
type releaseCopy struct {
	docID, contentVersion, fingerprint string
	entries                            []releaseEntry
	markdown                           string
}

// TakeOntologyRelease copies the tenant ontology of ctx's tenant into a new release with
// the given reason. summary, when not nil, is stored as the release's plan_summary (JSON,
// at most 8 KiB; a larger one is replaced by a note of its size).
func (d *Document) TakeOntologyRelease(ctx context.Context, reason string, summary any) (OntologyRelease, error) {
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return OntologyRelease{}, err
	}
	held, err := d.holdOntologyReleaseLock(ctx, key)
	if err != nil {
		return OntologyRelease{}, err
	}
	defer held.unlock()
	return d.takeOntologyRelease(ctx, key, mscope, held, reason, summary)
}

// CatchUpOntologyRelease takes a catch_up release when the content version in force is
// not the newest complete release's — or no release exists — and reports whether it took
// one. Nothing is taken when the content version cannot be read (no ontology document, a
// partial read): there is then no version to explain.
func (d *Document) CatchUpOntologyRelease(ctx context.Context) (OntologyRelease, bool, error) {
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return OntologyRelease{}, false, err
	}
	held, err := d.holdOntologyReleaseLock(ctx, key)
	if err != nil {
		return OntologyRelease{}, false, err
	}
	defer held.unlock()
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.ContentVersion == "" {
		return OntologyRelease{}, false, nil
	}
	newest, err := d.ontologyReleases(ctx, key, 1)
	if err != nil {
		return OntologyRelease{}, false, err
	}
	if len(newest) == 1 && newest[0].ContentVersion == read.ContentVersion {
		return OntologyRelease{}, false, nil
	}
	rel, err := d.takeOntologyRelease(ctx, key, mscope, held, OntologyReleaseCatchUp, nil)
	if err != nil {
		return OntologyRelease{}, false, err
	}
	return rel, true, nil
}

// ontologyReleases lists the newest complete releases, newest first; limit <= 0 lists
// them all. An incomplete release (one whose copy was cut off) is never listed.
func (d *Document) ontologyReleases(ctx context.Context, key sqlmem.ScopeKey, limit int) ([]OntologyRelease, error) {
	q := `SELECT seq, content_version, fingerprint, reason, coalesce(actor, ''), created_at, entries
	        FROM ontology_releases WHERE complete = 1 ORDER BY seq DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	res, err := d.query(ctx, key, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]OntologyRelease, 0, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) < 7 {
			continue
		}
		out = append(out, OntologyRelease{
			Seq: asInt64Val(r[0]), ContentVersion: asStr(r[1]), Fingerprint: asStr(r[2]),
			Reason: asStr(r[3]), Actor: asStr(r[4]), CreatedAt: asInt64Val(r[5]), Entries: asInt(r[6]),
		})
	}
	return out, nil
}

// takeOntologyRelease copies, then writes. The caller holds the release lock, held.
func (d *Document) takeOntologyRelease(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, held *releaseLock, reason string, summary any) (OntologyRelease, error) {
	var cp releaseCopy
	for attempt := 0; ; attempt++ {
		c, moved, err := d.copyOntologyForRelease(ctx, key, mscope)
		if err != nil {
			return OntologyRelease{}, err
		}
		if !moved {
			cp = c
			break
		}
		if attempt == ontologyReleaseRetries {
			return OntologyRelease{}, ErrOntologyMovedDuringRelease
		}
	}
	return d.writeOntologyRelease(ctx, key, held, cp, reason, releaseSummary(summary))
}

// copyOntologyForRelease reads the ontology document whole: every chunk that is not a
// proposal or a rejection, its body and fields, and the content version in force. moved
// is true when the ontology's fingerprint differs before and after, so the copy may mix
// two states and must be taken again.
func (d *Document) copyOntologyForRelease(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope) (releaseCopy, bool, error) {
	docID, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		return releaseCopy{}, false, err
	}
	if docID == "" {
		return releaseCopy{}, false, errNoOntology
	}
	before, err := d.ontologyFingerprint(ctx, key, docID)
	if err != nil {
		return releaseCopy{}, false, err
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.ContentVersion == "" {
		return releaseCopy{}, false, errors.New("the ontology could not be read whole, so it has no content version to release")
	}
	// Materialised before any body is read: a SQLite scope has one connection.
	res, err := d.query(ctx, key, `SELECT `+chunkSelectCols+` FROM chunks WHERE document_id = ? ORDER BY parent_id, position, id`, docID)
	if err != nil {
		return releaseCopy{}, false, err
	}
	if res.Truncated {
		return releaseCopy{}, false, errors.New("the ontology document has more chunks than one read returns")
	}
	kids := map[string][]chunkRow{}
	for _, r := range res.Rows {
		row := scanChunkRow(res.Columns, r)
		kids[row.ParentID] = append(kids[row.ParentID], row)
	}
	for k := range kids {
		sort.SliceStable(kids[k], func(i, j int) bool { return kids[k][i].Position < kids[k][j].Position })
	}

	cp := releaseCopy{docID: docID, contentVersion: read.ContentVersion}
	var md strings.Builder
	var walkErr error
	// Depth-first in document order. A proposal or a rejection is not copied, but what is
	// under it is, rendered at its depth: the reader keeps such children in force under
	// the nearest in-force ancestor.
	var walk func(rows []chunkRow, depth int)
	walk = func(rows []chunkRow, depth int) {
		for _, row := range rows {
			if walkErr != nil {
				return
			}
			if memrank.IsInertEntityStatus(row.Status) {
				walk(kids[row.ID], depth)
				continue
			}
			b, err := d.readBody(ctx, mscope, key, row.ID)
			if err != nil {
				walkErr = fmt.Errorf("read the body of %s: %w", row.ID, err)
				return
			}
			e := releaseEntry{chunkID: row.ID, parentID: row.ParentID, typ: row.Type, status: row.Status,
				title: row.Title, position: row.Position, body: b.Body, fields: string(b.Fields)}
			e.sha = releaseBodySHA(e.body, e.fields)
			cp.entries = append(cp.entries, e)
			level := min(depth+1, 6)
			md.WriteString(strings.Repeat("#", level) + " " + headingReplacer.Replace(row.Title) + "\n")
			if strings.TrimSpace(b.Body) != "" {
				md.WriteString("\n" + strings.TrimRight(b.Body, "\n") + "\n")
			}
			md.WriteString("\n")
			walk(kids[row.ID], depth+1)
		}
	}
	walk(kids[""], 0)
	if walkErr != nil {
		return releaseCopy{}, false, walkErr
	}
	cp.markdown = md.String()

	if d.duringReleaseCopy != nil {
		d.duringReleaseCopy()
	}
	after, err := d.ontologyFingerprint(ctx, key, docID)
	if err != nil {
		return releaseCopy{}, false, err
	}
	if after != before {
		return releaseCopy{}, true, nil
	}
	sum := sha256.Sum256([]byte(before))
	cp.fingerprint = hex.EncodeToString(sum[:]) // the form a plan reports
	return cp, false, nil
}

// releaseBodySHA names one (body, fields) pair. JSON so no pair of strings can join into
// another's bytes.
func releaseBodySHA(body, fields string) string {
	b, _ := json.Marshal([2]string{body, fields})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// releaseSummary renders a plan summary for storage: nil for none, and a note of the
// size in place of one over the cap (cutting JSON would leave it unreadable).
func releaseSummary(summary any) any {
	if summary == nil {
		return nil
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return nil
	}
	if len(b) > ontologyReleaseSummaryCap {
		b, _ = json.Marshal(map[string]any{"truncated": true, "bytes": len(b)})
	}
	return string(b)
}

// writeOntologyRelease stores a copy: the header incomplete, the entries and their bodies
// in batches, the header complete, then retention. Each step is its own statement or a
// batch's own transaction, so no transaction is held across the whole copy.
func (d *Document) writeOntologyRelease(ctx context.Context, key sqlmem.ScopeKey, held *releaseLock, cp releaseCopy, reason string, summary any) (OntologyRelease, error) {
	// The copy may have taken a while; the lease must still be this release's before it
	// removes an incomplete header (which, if the lease had been lost, could be another
	// replica's copy in progress) and allocates a seq.
	if err := held.renew(ctx); err != nil {
		return OntologyRelease{}, err
	}
	// A copy cut off before it completed is not listed; it goes now.
	if err := d.exec(ctx, key, `DELETE FROM ontology_release_entries
		WHERE release_seq IN (SELECT seq FROM ontology_releases WHERE complete = 0)`); err != nil {
		return OntologyRelease{}, err
	}
	if err := d.exec(ctx, key, `DELETE FROM ontology_releases WHERE complete = 0`); err != nil {
		return OntologyRelease{}, err
	}
	res, err := d.query(ctx, key, `SELECT coalesce(max(seq), 0) FROM ontology_releases`)
	if err != nil {
		return OntologyRelease{}, err
	}
	var seq int64 = 1
	if len(res.Rows) == 1 && len(res.Rows[0]) == 1 {
		seq = asInt64Val(res.Rows[0][0]) + 1
	}
	rel := OntologyRelease{Seq: seq, ContentVersion: cp.contentVersion, Fingerprint: cp.fingerprint,
		Reason: reason, Actor: tools.RunIdentity(ctx).UserID, CreatedAt: time.Now().UnixNano(), Entries: len(cp.entries)}
	if err := d.exec(ctx, key, `INSERT INTO ontology_releases
		(seq, content_version, fingerprint, reason, actor, created_at, plan_summary, entries, complete, export_md)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		rel.Seq, rel.ContentVersion, rel.Fingerprint, rel.Reason, rel.Actor, rel.CreatedAt, summary, rel.Entries, cp.markdown); err != nil {
		return OntologyRelease{}, err
	}
	for start := 0; start < len(cp.entries); start += ontologyReleaseBatch {
		batch := cp.entries[start:min(start+ontologyReleaseBatch, len(cp.entries))]
		err := d.withSqlTxn(ctx, key, func(txnID string) error {
			for _, e := range batch {
				if err := d.execTxn(ctx, txnID, `INSERT INTO ontology_release_bodies (sha, body, fields)
					VALUES (?, ?, ?) ON CONFLICT (sha) DO NOTHING`, e.sha, e.body, e.fields); err != nil {
					return err
				}
				if err := d.execTxn(ctx, txnID, `INSERT INTO ontology_release_entries
					(release_seq, document_id, chunk_id, parent_id, position, type, status, title, body_sha)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					rel.Seq, cp.docID, e.chunkID, nullIfEmpty(e.parentID), e.position, nullIfEmpty(e.typ),
					nullIfEmpty(e.status), e.title, e.sha); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return OntologyRelease{}, fmt.Errorf("write release %d's entries: %w", rel.Seq, err)
		}
		if d.afterReleaseBatch != nil {
			if err := d.afterReleaseBatch(); err != nil {
				return OntologyRelease{}, err
			}
		}
		if err := held.renew(ctx); err != nil {
			return OntologyRelease{}, err
		}
	}
	if err := d.exec(ctx, key, `UPDATE ontology_releases SET complete = 1 WHERE seq = ?`, rel.Seq); err != nil {
		return OntologyRelease{}, err
	}
	if err := d.pruneOntologyReleases(ctx, key); err != nil {
		// The release is taken; retention runs again with the next one.
		slog.WarnContext(ctx, "ontology release: retention failed", "tenant", key.Tenant, "seq", rel.Seq, "err", err)
	}
	return rel, nil
}

// pruneOntologyReleases keeps the newest ontologyReleasesKept complete releases: older
// headers go, then every entry whose header is gone, then every body no entry uses. One
// statement each. An incomplete header is left to the next release, which removes it.
func (d *Document) pruneOntologyReleases(ctx context.Context, key sqlmem.ScopeKey) error {
	if err := d.exec(ctx, key, `DELETE FROM ontology_releases WHERE complete = 1 AND seq NOT IN (
		SELECT seq FROM ontology_releases WHERE complete = 1 ORDER BY seq DESC LIMIT ?)`, ontologyReleasesKept); err != nil {
		return err
	}
	if err := d.exec(ctx, key, `DELETE FROM ontology_release_entries
		WHERE release_seq NOT IN (SELECT seq FROM ontology_releases)`); err != nil {
		return err
	}
	return d.exec(ctx, key, `DELETE FROM ontology_release_bodies
		WHERE sha NOT IN (SELECT body_sha FROM ontology_release_entries)`)
}

// ontologyReleaseLease is the tenant scope's lease while a release is taken. The
// in-process lock serialises releases within one replica only; two replicas sharing
// one database could otherwise both allocate the same seq, one could remove the
// other's incomplete copy as left over, or both could catch up the same content
// version. A document_leases row is shared by every replica on both tiers, and its
// acquisition is one conditional UPDATE, so only one replica holds it.
//
// It carries principalLeasePrefix: the lease op refuses that prefix, so no caller can
// hold releases off the tenant, and heldScopeLease does not count it as a pass, so a
// move of the tenant scope is not refused while a release is taken. Subject leases are
// named by chunk ids, which never take this form.
const ontologyReleaseLease = principalLeasePrefix + "ontology-release"

const (
	// ontologyReleaseLeaseTTL bounds how long a replica that died holding the lease
	// keeps others waiting. The holder renews it before it writes and after each batch.
	ontologyReleaseLeaseTTL = 60 * time.Second
	// ontologyReleaseLeaseWait is how long a release waits for another replica's to
	// finish: past the TTL, so a dead holder's lease has expired by then.
	ontologyReleaseLeaseWait = ontologyReleaseLeaseTTL + 15*time.Second
	ontologyReleaseLeasePoll = 25 * time.Millisecond
)

// ErrOntologyReleaseBusy: another replica held the release lease for longer than a
// release waits. ErrOntologyReleaseLeaseLost: the lease expired under a slow release
// and another replica took it, so this one stops before writing over that one's.
var (
	ErrOntologyReleaseBusy      = errors.New("another replica is taking an ontology release; it was not taken")
	ErrOntologyReleaseLeaseLost = errors.New("the ontology release lease expired while the release was taken; it was not completed")
)

// releaseLock is a held release lock: the in-process one and the scope lease.
type releaseLock struct {
	d      *Document
	key    sqlmem.ScopeKey
	owner  string
	unlock func()
}

// holdOntologyReleaseLock takes the release lock of key's tenant: first the in-process
// lock, so releases of one replica queue without polling the database, then the scope
// lease, waiting up to ontologyReleaseLeaseWait for another replica to let go.
func (d *Document) holdOntologyReleaseLock(ctx context.Context, key sqlmem.ScopeKey) (*releaseLock, error) {
	unlock, err := d.editions.lock(ctx, key, "ontology_release", memrank.OntologyPath)
	if err != nil {
		return nil, err
	}
	owner := randomLeaseOwner()
	deadline := time.Now().Add(ontologyReleaseLeaseWait)
	for {
		lease, err := d.acquireDocumentLease(ctx, key, ontologyReleaseLease, owner, time.Now(), ontologyReleaseLeaseTTL)
		if err != nil {
			unlock()
			return nil, err
		}
		if lease.Acquired {
			break
		}
		if time.Now().After(deadline) {
			unlock()
			return nil, ErrOntologyReleaseBusy
		}
		select {
		case <-ctx.Done():
			unlock()
			return nil, ctx.Err()
		case <-time.After(ontologyReleaseLeasePoll):
		}
	}
	l := &releaseLock{d: d, key: key, owner: owner}
	l.unlock = func() {
		_, _ = d.releaseDocumentLease(context.WithoutCancel(ctx), key, ontologyReleaseLease, owner)
		unlock()
	}
	return l, nil
}

// renew extends the lease; ErrOntologyReleaseLeaseLost when another replica holds it.
func (l *releaseLock) renew(ctx context.Context) error {
	lease, err := l.d.acquireDocumentLease(ctx, l.key, ontologyReleaseLease, l.owner, time.Now(), ontologyReleaseLeaseTTL)
	if err != nil {
		return err
	}
	if !lease.Acquired {
		return ErrOntologyReleaseLeaseLost
	}
	return nil
}
