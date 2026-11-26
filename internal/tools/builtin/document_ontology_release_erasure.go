package builtin

// Subject erasure reaching ontology releases (RFC GQ P3.3 follow-up).
//
// A release copies the whole tenant ontology and is kept for up to 50 releases, so an
// instance entry adopted as a subject — its fields.registry_key the subject's natural key,
// the subject root's fields.instance_chunk its chunk id — keeps that subject's name,
// aliases and meaning in every release taken while it was declared, after the entry
// itself has been edited or removed.
//
// An erasure keeps the subjects bound to the erased principal and never edits the
// ontology in force: both are the operator's call (erasure.retainedBoundSubjects, RFC FO
// §3.5). The release copies are records, not the ontology, and an erasure redacts them:
//
//   - the entry keeps its row, release, chunk id, parent, position, type and status, so
//     the release's shape and header stay; its title becomes releaseErasedTitle and its
//     body_sha releaseErasedSHA, the mark a reader goes by (no sha256 is that string);
//   - its (body, fields) row goes when no other entry still uses it;
//   - the release's export_md loses the entry's heading and text.
//
// A redacted release keeps the content_version it recorded, so its copy no longer
// hashes to that version: the header says what the ontology declared then, and the copy
// no longer holds all of it. The release diff reports an erased entry as `erased`, never
// as a change, and a rollback leaves it as it is.
//
// Matched by the rule the plan sets (3.2): the entry's fields.registry_key is a bound
// subject's natural key, or its chunk id is the instance chunk the subject records;
// either way never an entry its release shows to be something else (a subject root's
// fields are not runtime-only, so an instance_chunk naming a class must not erase it),
// while an entry the release cannot classify is redacted (instanceEntriesOnly). Not
// reached, and reported: a release's plan_summary (the accept or rollback report it was
// taken with) may name the entry and is not rewritten; another entry whose own text names
// the subject is that entry's, and stays; and a subject deleted before the erasure is no
// longer bound, so nothing ties its release copies to the principal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// releaseErasedSHA marks an erased release entry in body_sha. No body row has it:
	// a sha is 64 hex characters.
	releaseErasedSHA = "erased"
	// releaseErasedTitle replaces an erased entry's title, in the entry and in the
	// release's export_md.
	releaseErasedTitle = "[erased]"
)

// ReleaseErasure is what an erasure did to the tenant's ontology releases, or, in a dry
// run, would do. Counts only: an erasure's result reaches its audit record, which never
// names a subject.
type ReleaseErasure struct {
	// Entries is the release entries redacted; Releases the releases they are in.
	Entries  int `json:"entries"`
	Releases int `json:"releases"`
	// InForce counts the bound subjects' instance entries still in the ontology in
	// force, which an erasure does not edit; a later release copies them again.
	InForce int `json:"in_force"`
	// Summaries counts the releases whose plan_summary named the subjects and was
	// scrubbed (or would be).
	Summaries int `json:"summaries"`
}

// migrateReleaseRedacted adds ontology_releases.redacted: 1 once a subject erasure
// changed the release (an entry or its plan summary), NULL otherwise. Not declared in
// the CREATE TABLE, so a scope has one shape whichever release created it; probe then
// ALTER, like migrateOntologyContent.
func (d *Document) migrateReleaseRedacted(ctx context.Context, key sqlmem.ScopeKey) error {
	if d.releaseHasRedacted(ctx, key) {
		return nil
	}
	if err := d.exec(ctx, key, `ALTER TABLE ontology_releases ADD COLUMN redacted INTEGER`); err != nil {
		return fmt.Errorf("add ontology_releases.redacted: %w", err)
	}
	return nil
}

// releaseHasRedacted reports whether the scope's ontology_releases has the column. A
// read runs under noWrites and never adds it: a scope without it has no redacted release.
func (d *Document) releaseHasRedacted(ctx context.Context, key sqlmem.ScopeKey) bool {
	_, err := d.query(ctx, key, `SELECT redacted FROM ontology_releases WHERE 1=0`)
	return err == nil
}

// redactedCol is the select expression for a release's flag: the column where the
// scope has it, else 0.
func (d *Document) redactedCol(ctx context.Context, key sqlmem.ScopeKey) string {
	if d.releaseHasRedacted(ctx, key) {
		return `coalesce(redacted, 0)`
	}
	return `0`
}

// releaseMatch is one release entry an erasure redacts.
type releaseMatch struct {
	seq            int64
	chunkID, title string
	sha            string
}

// EraseSubjectsFromReleases redacts, from every ontology release of ctx's tenant, the
// entries that hold the given subjects' text (see the file comment); with dryRun it
// counts them and writes nothing. A tenant with no scope, or no release, has nothing
// to redact.
func (d *Document) EraseSubjectsFromReleases(ctx context.Context, subjects []SubjectBinding, dryRun bool) (ReleaseErasure, error) {
	var out ReleaseErasure
	if len(subjects) == 0 {
		return out, nil
	}
	key, mscope, ok, err := d.existingTenantScope(ctx)
	if err != nil || !ok {
		return out, err
	}
	keys, chunks, err := d.subjectInstanceRefs(ctx, key, mscope, subjects)
	if err != nil {
		return out, err
	}
	// The release lock, held across replicas: a release being taken must not copy or
	// prune around the redaction.
	held, err := d.holdOntologyReleaseLock(ctx, key)
	if err != nil {
		return out, err
	}
	defer held.unlock()
	candidates, err := d.releaseEntriesOf(ctx, key, keys, chunks)
	if err != nil {
		return out, err
	}
	matches, err := d.instanceEntriesOnly(ctx, key, candidates)
	if err != nil {
		return out, err
	}
	bySeq := map[int64][]releaseMatch{}
	for _, m := range matches {
		bySeq[m.seq] = append(bySeq[m.seq], m)
		chunks[m.chunkID] = true
	}
	out.Entries, out.Releases = len(matches), len(bySeq)
	inForce := d.inForceOf(ctx, chunks)
	out.InForce = len(inForce)
	summaries, err := d.summariesNaming(ctx, key, summaryNeedles(keys, matches, inForce))
	if err != nil {
		return out, err
	}
	out.Summaries = len(summaries)
	if dryRun || (len(matches) == 0 && len(summaries) == 0) {
		return out, nil
	}
	// A scope from before the flag gains it here: provisioning runs once per process.
	if err := d.migrateReleaseRedacted(ctx, key); err != nil {
		return out, err
	}
	for _, s := range summaries {
		if err := d.exec(ctx, key, `UPDATE ontology_releases SET plan_summary = ?, redacted = 1 WHERE seq = ?`, s.summary, s.seq); err != nil {
			return out, fmt.Errorf("scrub release %d's plan summary: %w", s.seq, err)
		}
	}
	seqs := make([]int64, 0, len(bySeq))
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for _, seq := range seqs {
		if err := d.redactRelease(ctx, key, seq, bySeq[seq]); err != nil {
			return out, fmt.Errorf("redact release %d: %w", seq, err)
		}
	}
	// The bodies no entry uses any more; one shared with an entry not erased stays.
	shas := map[string]bool{}
	for _, m := range matches {
		shas[m.sha] = true
	}
	for sha := range shas {
		if err := d.exec(ctx, key, `DELETE FROM ontology_release_bodies WHERE sha = ?
			AND NOT EXISTS (SELECT 1 FROM ontology_release_entries WHERE body_sha = ?)`, sha, sha); err != nil {
			return out, err
		}
	}
	return out, nil
}

// subjectInstanceRefs reads, for each subject, its natural key and the instance chunk
// its root records (an adopted instance's subject has one).
func (d *Document) subjectInstanceRefs(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, subjects []SubjectBinding) (map[string]bool, map[string]bool, error) {
	keys, chunks := map[string]bool{}, map[string]bool{}
	for _, s := range subjects {
		if k := strings.TrimSpace(s.NaturalKey); k != "" {
			keys[k] = true
		}
		if s.RootChunkID == "" {
			continue
		}
		b, err := d.readBody(ctx, mscope, key, s.RootChunkID)
		if err != nil {
			return nil, nil, err
		}
		var f struct {
			InstanceChunk string `json:"instance_chunk"`
		}
		if len(b.Fields) > 0 && json.Unmarshal(b.Fields, &f) == nil && f.InstanceChunk != "" {
			chunks[f.InstanceChunk] = true
		}
	}
	return keys, chunks, nil
}

// releaseBodyPage bounds one read of the release bodies.
const releaseBodyPage = 500

// releaseEntriesOf lists the release entries, not yet erased, whose body's
// fields.registry_key is one of keys or whose chunk id is one of chunks.
func (d *Document) releaseEntriesOf(ctx context.Context, key sqlmem.ScopeKey, keys, chunks map[string]bool) ([]releaseMatch, error) {
	shas := map[string]bool{}
	// Bodies are kept once per hash, so this reads each instance's text once, not
	// once per release. Paged by sha, each page materialised (one SQLite connection).
	for after := ""; ; {
		res, err := d.query(ctx, key, `SELECT sha, fields FROM ontology_release_bodies
			WHERE sha > ? AND fields LIKE '%registry_key%' ORDER BY sha LIMIT ?`, after, releaseBodyPage)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			if len(r) < 2 {
				continue
			}
			after = asStr(r[0])
			var f struct {
				RegistryKey string `json:"registry_key"`
			}
			if json.Unmarshal([]byte(asStr(r[1])), &f) == nil && keys[strings.TrimSpace(f.RegistryKey)] {
				shas[after] = true
			}
		}
		if len(res.Rows) < releaseBodyPage {
			break
		}
	}
	var out []releaseMatch
	seen := map[string]bool{}
	add := func(q string, arg string) error {
		res, err := d.query(ctx, key, q, arg, releaseErasedSHA)
		if err != nil {
			return err
		}
		if res.Truncated {
			return fmt.Errorf("more release entries hold this subject than one read returns")
		}
		for _, r := range res.Rows {
			if len(r) < 4 {
				continue
			}
			m := releaseMatch{seq: asInt64Val(r[0]), chunkID: asStr(r[1]), title: asStr(r[2]), sha: asStr(r[3])}
			if id := fmt.Sprintf("%d\x00%s", m.seq, m.chunkID); !seen[id] {
				seen[id] = true
				out = append(out, m)
			}
		}
		return nil
	}
	const cols = `SELECT release_seq, chunk_id, title, body_sha FROM ontology_release_entries`
	for sha := range shas {
		if err := add(cols+` WHERE body_sha = ? AND body_sha <> ?`, sha); err != nil {
			return nil, err
		}
	}
	for id := range chunks {
		if err := add(cols+` WHERE chunk_id = ? AND body_sha <> ?`, id); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].seq != out[j].seq {
			return out[i].seq < out[j].seq
		}
		return out[i].chunkID < out[j].chunkID
	})
	return out, nil
}

// instanceEntriesOnly drops the candidates their release positively shows are not
// instance entries: a class, grammar, relation, scheme or concept, a section header or
// the root, as the release's reader parses it. The chunk ids come from a subject
// root's fields.instance_chunk, which is not runtime-only, and a key may sit in any
// entry's fields; neither may redact what the release shows to be something else.
//
// Every other candidate is kept and redacted, so a doubt never spares the subject's
// text: an entry the reader does not place (under an uncopied proposal, past a partial
// read) and every candidate of a release the reader cannot load (an incomplete copy,
// cut off before it completed and not yet removed by the next release). No stored
// column names an entry's kind (the chunk type is not it), so the parse is the record.
func (d *Document) instanceEntriesOnly(ctx context.Context, key sqlmem.ScopeKey, candidates []releaseMatch) ([]releaseMatch, error) {
	rels := map[int64]*loadedRelease{}
	var out []releaseMatch
	for _, m := range candidates {
		rel, ok := rels[m.seq]
		if !ok {
			var err error
			rel, err = d.loadRelease(ctx, key, m.seq)
			if errors.Is(err, connector.ErrOntologyReleaseNotFound) {
				rel = nil // incomplete: nothing is shown, so nothing is excluded
			} else if err != nil {
				return nil, err
			}
			rels[m.seq] = rel
		}
		if rel != nil && rel.notAnInstance(m.chunkID) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// inForceOf lists the instance entries of the ontology in force among chunks.
func (d *Document) inForceOf(ctx context.Context, chunks map[string]bool) []string {
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		return nil
	}
	var out []string
	for _, in := range read.Instances {
		if chunks[in.ChunkID] {
			out = append(out, in.ChunkID)
		}
	}
	return out
}

// summaryNeedles are the names a plan summary may carry of the subjects, by the rule the
// entries are matched by: the subjects' keys, and the chunk ids and titles of their
// instance entries as the releases copied them, or as the ontology in force holds them.
// A chunk id a subject records but no release or the ontology in force shows to be an
// instance is not one: it may name anything. Longest first, so a name inside another is
// not cut out of it first.
func summaryNeedles(keys map[string]bool, matches []releaseMatch, inForce []string) []string {
	set := map[string]bool{}
	for k := range keys {
		set[k] = true
	}
	for _, m := range matches {
		set[m.chunkID], set[m.title] = true, true
	}
	for _, id := range inForce {
		set[id] = true
	}
	delete(set, "")
	delete(set, releaseErasedTitle)
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// scrubbedSummary is one release's plan_summary with the subjects' names erased.
type scrubbedSummary struct {
	seq     int64
	summary string
}

// summariesNaming scrubs needles out of every release's plan_summary and returns the
// ones that changed, unwritten.
func (d *Document) summariesNaming(ctx context.Context, key sqlmem.ScopeKey, needles []string) ([]scrubbedSummary, error) {
	if len(needles) == 0 {
		return nil, nil
	}
	res, err := d.query(ctx, key, `SELECT seq, plan_summary FROM ontology_releases WHERE plan_summary IS NOT NULL ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var out []scrubbedSummary
	for _, r := range res.Rows {
		if len(r) < 2 {
			continue
		}
		if s, changed := scrubSummary(asStr(r[1]), needles); changed {
			out = append(out, scrubbedSummary{seq: asInt64Val(r[0]), summary: s})
		}
	}
	return out, nil
}

// scrubSummary replaces each needle with the erased marker. A JSON summary is rewritten
// value by value (strings, and object keys, which can be names too), so it stays JSON and
// its numbers keep their text; anything else is rewritten as text.
func scrubSummary(s string, needles []string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		out := scrubText(s, needles)
		return out, out != s
	}
	changed := false
	v = scrubValue(v, needles, &changed)
	if !changed {
		return s, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		out := scrubText(s, needles)
		return out, out != s
	}
	return string(b), true
}

func scrubValue(v any, needles []string, changed *bool) any {
	switch t := v.(type) {
	case string:
		out := scrubText(t, needles)
		if out != t {
			*changed = true
		}
		return out
	case []any:
		for i := range t {
			t[i] = scrubValue(t[i], needles, changed)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			nk := scrubText(k, needles)
			if nk != k {
				*changed = true
			}
			out[nk] = scrubValue(val, needles, changed)
		}
		return out
	}
	return v
}

// scrubText replaces the needles in one pass (a later needle never matches inside the
// marker an earlier one wrote), the longest at a position first.
func scrubText(s string, needles []string) string {
	pairs := make([]string, 0, 2*len(needles))
	for _, n := range needles {
		pairs = append(pairs, n, releaseErasedTitle)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// redactRelease redacts the entries ms of release seq, and its export_md, in one
// transaction.
func (d *Document) redactRelease(ctx context.Context, key sqlmem.ScopeKey, seq int64, ms []releaseMatch) error {
	res, err := d.query(ctx, key, `SELECT coalesce(export_md, '') FROM ontology_releases WHERE seq = ?`, seq)
	if err != nil {
		return err
	}
	var md string
	if len(res.Rows) == 1 && len(res.Rows[0]) == 1 {
		md = asStr(res.Rows[0][0])
	}
	for _, m := range ms {
		b, err := d.query(ctx, key, `SELECT body FROM ontology_release_bodies WHERE sha = ?`, m.sha)
		if err != nil {
			return err
		}
		var body string
		if len(b.Rows) == 1 && len(b.Rows[0]) == 1 {
			body = asStr(b.Rows[0][0])
		}
		md = redactReleaseMarkdown(md, m.title, body)
	}
	return d.withSqlTxn(ctx, key, func(txnID string) error {
		for _, m := range ms {
			if err := d.execTxn(ctx, txnID, `UPDATE ontology_release_entries SET title = ?, body_sha = ?
				WHERE release_seq = ? AND chunk_id = ?`, releaseErasedTitle, releaseErasedSHA, seq, m.chunkID); err != nil {
				return err
			}
		}
		return d.execTxn(ctx, txnID, `UPDATE ontology_releases SET export_md = ?, redacted = 1 WHERE seq = ?`, md, seq)
	})
}

// redactReleaseMarkdown replaces an entry's section in a release's export_md — its
// heading at any level and its body, as copyOntologyForRelease wrote them — with an
// erased heading at the same level.
func redactReleaseMarkdown(md, title, body string) string {
	var text string
	if strings.TrimSpace(body) != "" {
		text = "\n" + strings.TrimRight(body, "\n") + "\n"
	}
	for level := 6; level >= 1; level-- {
		hashes := strings.Repeat("#", level)
		seg := hashes + " " + headingReplacer.Replace(title) + "\n" + text + "\n"
		erased := hashes + " " + releaseErasedTitle + "\n\n"
		if strings.HasPrefix(md, seg) {
			md = erased + md[len(seg):]
		}
		md = strings.ReplaceAll(md, "\n"+seg, "\n"+erased)
	}
	return md
}
