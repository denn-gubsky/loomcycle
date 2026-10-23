package runident

// ParentContext is the typed caller-tracking lineage attached to a run
// and propagated to all its sub-agents. The runtime stores and echoes
// these fields verbatim and never branches on their values — they are
// consumer-domain concepts (a deliberate, operator-requested exception
// to loomcycle's usual domain-agnostic posture).
//
// It lives in this leaf package, which imports nothing from the module, so
// Identity can carry it and runner, tools, connector, store and the wire
// surfaces can all reference one type without an import cycle.
// store.ParentContext is an alias for it; store owns its encoding
// (store.EncodeParentContext / DecodeParentContext).
//
// Unlike UserBearer/UserCredentials this is NOT a secret: safe to
// persist, log, and emit in events. All fields optional; an all-empty
// struct is treated as absent (nil) at wire entry.
//
// Two owners share this struct. The CALLER sets RootAgentRunID, FunctionKey
// and TierAtRun. The RUNTIME alone sets the Board*, Walk/Wave and State fields, on the
// runs a team walk spawns — consumers group runs by them, so a caller must not
// be able to claim them. Every ingress clears the runtime-owned fields through
// connector.StripRuntimeParentContext; a field added here must be classified
// there (TestStripRuntimeParentContext_ClassifiesEveryField fails until it is).
type ParentContext struct {
	// RootAgentRunID is the consumer's identifier for the user-
	// initiated run at the root of the spawn tree. Echoed on every
	// descendant so the consumer can attribute child costs to it.
	RootAgentRunID string `json:"root_agent_run_id,omitempty"`
	// FunctionKey is the consumer's logical-operation key for the root
	// request (e.g. its cost-aggregation bucket).
	FunctionKey string `json:"function_key,omitempty"`
	// TierAtRun is the consumer's tier marker captured at root-run time.
	// Distinct from UserTier (loomcycle's resolver policy) — this is the
	// consumer's own snapshot, carried verbatim.
	TierAtRun string `json:"tier_at_run,omitempty"`
	// RUNTIME-OWNED from here down: cleared from caller input at every ingress.
	//
	// Board{Scope,ChunkID,DocumentID} correlate a run to a workflow-board task
	// (a Document chunk) for loomboard (RFC BT P4). A board-bound `TeamDef
	// op=run` stamps these onto each handler run it spawns, so a client folding
	// the run-state stream can pin a live agent to its kanban card. Empty for
	// non-board runs. Not secret — safe to persist and emit like the rest.
	BoardScope      string `json:"board_scope,omitempty"`
	BoardChunkID    string `json:"board_chunk_id,omitempty"`
	BoardDocumentID string `json:"board_document_id,omitempty"`
	// Walk/Wave correlate a run to the TeamDef walk and the Starter WAVE that
	// spawned it. A wave is N runs dispatched together from one channel read;
	// these three are the join that recovers it.
	//
	// It is a JOIN, not a copy: the prompt, transcript, tokens, cost and error
	// of every run in a wave already exist: what was missing was the key that
	// groups them. A canvas reviewing a wave reads the runs; a Starter that
	// restarted mid-wave rebuilds the wave the same way, which is what lets the
	// deferred sink publish stay a deferral rather than a second copy of the
	// results. Empty for every non-wave run.
	WalkID string `json:"walk_id,omitempty"`
	WaveID string `json:"wave_id,omitempty"`
	// No omitempty: index 0 is a REAL position in a wave, and dropping it
	// makes the first run of every wave indistinguishable from a run with no
	// index at all. It is only ever written alongside WalkID/WaveID, so this
	// adds a field to an object that was already being stored rather than
	// creating new rows.
	WaveIndex int `json:"wave_index"`
	// State and StateVisit say which state of the walk ran this member, and
	// which time: every member a walk spawns carries them (agent, parallel,
	// consolidator and Starter alike), where the wave fields are a Starter's
	// alone. State is the state id.
	//
	// StateVisit is the walk's own count of state visits — 1 for the first
	// state it ran, +1 for every one after — so the members of ONE visit share
	// it and a revisit of the same state gets a new one. It is deliberately not
	// the per-state iteration count: an operator's `continue` on a cap resets
	// that to 1, and a reroute to 0, so two different visits could carry the
	// same number. 0 = not a walk member.
	State      string `json:"state,omitempty"`
	StateVisit int    `json:"state_visit,omitempty"`
}

// IsZero reports whether every field is empty (no meaningful tracking
// context). Wire entry points normalise a zero struct to nil so
// back-compat decode paths stay clean.
// EVERY field must be listed. A field added to ParentContext and not added
// here makes a context carrying ONLY that field encode as zero in
// store.EncodeParentContext — the column is written NULL and the value is
// dropped silently, on every run, with nothing to point at.
//
// That is not hypothetical: the wave correlation (WalkID/WaveID/WaveIndex)
// was added without being listed, so every run a Starter spawned stored a NULL
// parent_context and the join the design was built on never existed. It looked
// correct everywhere except in the database.
func (p *ParentContext) IsZero() bool {
	return p == nil || (p.RootAgentRunID == "" && p.FunctionKey == "" && p.TierAtRun == "" &&
		p.BoardScope == "" && p.BoardChunkID == "" && p.BoardDocumentID == "" &&
		p.WalkID == "" && p.WaveID == "" && p.WaveIndex == 0 &&
		p.State == "" && p.StateVisit == 0)
}

// Clone returns a deep copy (nil-safe) so a parent's ParentContext can
// be handed to a child run without aliasing.
func (p *ParentContext) Clone() *ParentContext {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}
