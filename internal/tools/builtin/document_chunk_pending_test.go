package builtin

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The chunk write must carry the SERVER's answer about who wrote a fact
// (RFC CV, P2f prerequisite).
//
// originForEntityWrite cannot tell a consolidation pass from any other agent — it
// returns "agent_explicit" for every run — so a machine-distilled fact whose home
// is a chunk would claim to have been written by hand. That is survivable only
// while the k/v twin carries the real answer, and the collapse removes the twin.
//
// from_pending is the same mechanism Memory set uses, for the same reason: origin
// names the writer, so a caller may hand over an id but never the value.

// TestUpsertChunk_FromPendingStampsTheServersProvenance.
func TestUpsertChunk_FromPendingStampsTheServersProvenance(t *testing.T) {
	d, ctx, st := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	sk := sidecarScope(t, d, ctx)
	tenant := direntTenant(ctx)

	// A drained pending row, as a consolidation pass would leave it.
	// The pending row's own origin is the PRODUCER (agent_explicit | compaction),
	// never "consolidator" — that is the writer's identity and is stamped
	// separately. This asserts the producer wins, which is the whole point of
	// from_pending: a server-recorded producer beats an ambient guess.
	const pendingID = "p-1"
	if err := st.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{
		ID: pendingID, TenantID: tenant, Scope: store.MemoryScopeUser, ScopeID: sk.ScopeID,
		Payload:         []byte(`{"text":"Denn prefers Go."}`),
		Origin:          store.PendingOriginCompaction,
		SourceSessionID: "sess-a", SourceRunID: "run-a",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/denn-prefers-go","title":"Denn prefers Go",
		"body":"Denn prefers Go for backend services.","type":"fact","subject":"Denn",
		"from_pending":"`+pendingID+`"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id := asStr(out["id"])

	// The BODY row: the discriminator a collapsed fact is classified by.
	prov, err := st.MemoryProvenanceGet(ctx, tenant, store.MemoryScopeUser, sk.ScopeID, "doc.chunk:"+id)
	if err != nil {
		t.Fatalf("body provenance: %v", err)
	}
	if prov.Origin != store.PendingOriginCompaction {
		t.Errorf("body row origin = %q, want %q — a server-recorded producer must beat the "+
			"ambient attribution, exactly as it does on the k/v write",
			prov.Origin, store.PendingOriginCompaction)
	}

	// The SIDECAR: the two halves of one record must agree, or a fact's provenance
	// depends on which half a reader consults.
	res, err := d.SqlMem.Query(ctx, sk,
		d.SqlMem.Rebind(`SELECT coalesce(origin,''), coalesce(session_id,''), coalesce(run_id,'')
		                   FROM chunk_memory_meta WHERE chunk_id = ?`), []any{id})
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("sidecar read: %v", err)
	}
	gotOrigin, gotSession, gotRun := asStr(res.Rows[0][0]), asStr(res.Rows[0][1]), asStr(res.Rows[0][2])
	if gotOrigin != store.PendingOriginCompaction {
		t.Errorf("sidecar origin = %q, want %q — it must agree with the body row, or a fact's "+
			"provenance depends on which half a reader consults", gotOrigin, store.PendingOriginCompaction)
	}
	// session_id had NO writer in this plane at all before this: the run id is on
	// the context and the session id is not, so every chunk-homed fact was missing
	// the chat half of its source reference.
	if gotSession != "sess-a" {
		t.Errorf("sidecar session_id = %q, want sess-a — the chat half of the source reference", gotSession)
	}
	if gotRun != "run-a" {
		t.Errorf("sidecar run_id = %q, want run-a (the run the fact was distilled FROM, not the "+
			"run doing the writing)", gotRun)
	}
}

// TestUpsertChunk_AnUnknownPendingIdIsSilent: a miss must not cost a fact its
// write, matching Memory set. An id that is unknown or belongs to someone else
// simply leaves the ambient attribution in place.
func TestUpsertChunk_AnUnknownPendingIdIsSilent(t *testing.T) {
	d, ctx, st := documentFixture(t)
	doc := newEntityDoc(t, d, ctx)
	sk := sidecarScope(t, d, ctx)
	tenant := direntTenant(ctx)

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/x","title":"X","body":"A fact.","type":"fact","subject":"X",
		"from_pending":"no-such-id"}`)
	if r.IsError {
		t.Fatalf("an unknown pending id must not fail the write: %s", r.Text)
	}
	id := asStr(out["id"])
	prov, err := st.MemoryProvenanceGet(ctx, tenant, store.MemoryScopeUser, sk.ScopeID, "doc.chunk:"+id)
	if err != nil {
		t.Fatalf("body provenance: %v", err)
	}
	if prov.Origin == "" {
		t.Errorf("the write lost its ambient origin to an unknown pending id — a miss is silent, not destructive")
	}
}

// TestUpsertChunk_AConsolidationPassSaysSo is the half that actually mattered.
//
// The k/v write stamps `consolidator` when the run holds the Consolidation memory
// policy — a server-side grant, not a caller assertion. The chunk write checked
// only that a run existed, so the SAME distillation landed origin=consolidator on
// its k/v row and agent_explicit on its chunk. Harmless only while the k/v twin
// held the real answer; once the chunk is the fact's only home, every
// machine-distilled fact would claim to have been written by hand.
func TestUpsertChunk_AConsolidationPassSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name          string
		consolidation bool
		want          string
	}{
		{"a consolidation pass", true, "consolidator"},
		{"an ordinary agent", false, "agent_explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ctx, st := documentFixture(t)
			// A RUN is required, not just the grant: the operator planes hand out
			// Consolidation alongside wildcard memory scopes and carry no run id, so
			// keying on the grant alone would let any authenticated operator session
			// mint facts claiming a machine distilled them.
			ctx = tools.WithRunID(ctx, "run-1")
			ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{Consolidation: tc.consolidation})
			doc := newEntityDoc(t, d, ctx)
			sk := sidecarScope(t, d, ctx)

			out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
				"natural_key":"memory/fact/x","title":"X","body":"A distilled fact.",
				"type":"fact","subject":"X"}`)
			if r.IsError {
				t.Fatalf("upsert_chunk: %s", r.Text)
			}
			id := asStr(out["id"])
			prov, err := st.MemoryProvenanceGet(ctx, direntTenant(ctx), store.MemoryScopeUser, sk.ScopeID, "doc.chunk:"+id)
			if err != nil {
				t.Fatalf("body provenance: %v", err)
			}
			if prov.Origin != tc.want {
				t.Errorf("origin = %q, want %q — the chunk plane must answer 'who wrote this' the "+
					"same way the k/v plane does", prov.Origin, tc.want)
			}
		})
	}
}

// TestUpsertChunk_TheGrantAloneIsNotEnough: the Consolidation policy WITHOUT a run
// must not mint a consolidator origin. The operator planes carry the grant and no
// run id, so keying on the grant alone would hollow out the one thing the column is
// for — a trustworthy filter for facts a machine distilled from a transcript.
func TestUpsertChunk_TheGrantAloneIsNotEnough(t *testing.T) {
	d, ctx, st := documentFixture(t)
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{Consolidation: true}) // no run id
	doc := newEntityDoc(t, d, ctx)
	sk := sidecarScope(t, d, ctx)

	out, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+doc+`",
		"natural_key":"memory/fact/y","title":"Y","body":"A fact.","type":"fact","subject":"Y"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	prov, err := st.MemoryProvenanceGet(ctx, direntTenant(ctx), store.MemoryScopeUser, sk.ScopeID,
		"doc.chunk:"+asStr(out["id"]))
	if err != nil {
		t.Fatalf("body provenance: %v", err)
	}
	if prov.Origin == "consolidator" {
		t.Error("the grant alone minted a consolidator origin with no run — any authenticated " +
			"operator session could then claim a machine distilled its writes")
	}
}
