package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RestoreOptions controls the Restore() behaviour.
type RestoreOptions struct {
	// IncludeHistory toggles restoring the optional
	// interaction_history section. False (default) skips it even
	// when present in the envelope — operators often want
	// running-state restore only (paused runs + their transcripts +
	// Memory + Channels + AgentDefs + Evaluations) without
	// pollution from archived sessions.
	IncludeHistory bool

	// ForceProbe, when non-nil, is called by Restore after section
	// writes complete. The pause-resume-snapshot RFC says the
	// resolver matrix is excluded from snapshots — restore triggers
	// an immediate probe so the matrix is populated before the
	// operator calls Resume. Pass nil to skip.
	ForceProbe func(ctx context.Context)

	// SqlMem restores the RFC AA Phase-3e SQL Memory facet when non-nil. nil
	// (SQL Memory disabled on the restoring host) skips the section with a
	// warning. The call sites pass the runtime's *sqlmem.Manager.
	SqlMem SqlMemSnapshotter

	// ReplicaID is the restoring replica's id, stamped on each restored
	// queued run: this replica queues it again and holds its wait, and if it
	// restarts before the run starts, its boot restarts only the queued rows
	// stamped with its own id. "" outside a cluster, where the store keeps no
	// replica per run.
	ReplicaID string

	// Now is the target's clock, which decides whether carried month-to-date
	// usage is still in the current budget window. nil = time.Now; tests set it.
	Now func() time.Time

	// Validators re-run a section's authoring validation over each restored
	// body, keyed by section name (migrations.Section*). The snapshot package
	// cannot import the tools that own those rules, so the call sites inject
	// them. A row a validator refuses is skipped with a warning, and an active
	// pointer at it is refused.
	//
	// A missing validator differs by section. The sections added with this
	// mechanism (webhook, A2A, memory-backend, document-source and volume
	// defs, and dirents) are never restored unvalidated: with no validator
	// their rows are skipped. The def sections snapshots carried before it
	// (agent, skill, team, hook, MCP server and channel defs) restore as they
	// always have, with one section-level warning, so a caller that wires no
	// validators keeps getting its definitions back. A channel_defs validator
	// is given the ChannelDefEntry as JSON: a channel row has no single body.
	Validators map[string]func(body json.RawMessage) error

	// CredentialExists and EnvSet back the missing-credential scan: after the
	// definitions land, each $cred:/$ghapp: reference and env-var name a
	// restored definition names is checked here, and a miss is a warning
	// naming the definition and the reference. Both answer yes or no and
	// never return a value. CredentialExists takes the scope a run of that
	// definition resolves credentials in. nil skips that half of the scan
	// with one warning.
	CredentialExists func(ctx context.Context, tenantID, agentName, userID, name string) bool
	EnvSet           func(name string) bool
	// CredentialToCompare backs the scan for a credential only ever compared
	// (a webhook's stored verification secret): does tenantID hold a
	// tenant-level credential name, whatever its allowed_urls. Unlike
	// CredentialExists it must not refuse a URL-restricted credential, which
	// is how such a secret is meant to be stored. nil skips these with the
	// same warning.
	CredentialToCompare func(ctx context.Context, tenantID, name string) bool
	// OAuthCredentialState backs the scan for an $oauth:<name> reference:
	// the state of the OAuth credential the name resolves to for that scope
	// (connected, pending_connect, needs_reauth, revoked), and whether one is
	// held at all. Like the others it never returns a value. nil skips these
	// with the same warning.
	OAuthCredentialState func(ctx context.Context, tenantID, agentName, userID, name string) (state string, found bool)

	// VolumeRoot is this host's dynamic volume root (the static volume
	// marked dynamic_root: true). Every restored dynamic volume's path is
	// derived under it and its directory created there; a snapshot never
	// supplies one. "" skips the volume_defs section with a warning.
	VolumeRoot string
	// StaticVolumeNames are this host's static yaml volumes. A restored
	// dynamic volume of the same name is skipped with a warning: static is
	// ground truth and would shadow it at resolution.
	StaticVolumeNames []string

	// DocumentExists reports whether a document a restored Path name points
	// at is on this host. It takes the dirent's own coordinates (tenant,
	// scope, scope_id — the dirent plane's, which SQL Memory keys
	// differently) and must not provision a scope to answer. nil — SQL Memory
	// is not enabled here, so no document can be — skips every document name
	// with a warning; the other kinds are checked against the store.
	DocumentExists func(ctx context.Context, tenantID, scope, scopeID, documentID string) (bool, error)
}

// RestoreResult is the operator-facing summary of a Restore() call.
// Warnings carry every non-fatal anomaly the restore encountered:
// synthesized sessions, dropped expired rows, skipped sections, etc.
// Operators read warnings to decide whether the restore is "clean
// enough" to call Resume.
//
// MCPServerDefsActivated counts the MCP server registrations the caller's
// post-restore refresh made live: absent from, or different in, the
// in-process registry before the refresh ran.
//
// PausedRunsResumed counts the paused runs the caller re-dispatched as live
// loops after the refresh. It can be below PausedRunsRestored: a run whose
// agent no longer resolves is flagged failed and named in Warnings instead.
//
// UsageCarryRestored counts the (tenant, user) usage carries whose stored
// value grew; a re-restore of the same snapshot grows none.
//
// DefsDisabledForCredentials counts the trigger defs — schedules and
// webhooks — restored disabled because their literal credentials were
// stripped from the snapshot; the warnings name each one and its keys.
//
// ActivePointersRefused counts the active pointers not written because the
// def they name is missing here or is not that tenant's def of that name —
// what a promote refuses; the warnings name each pointer.
//
// AgentDefsRefused, SkillDefsRefused, TeamDefsRefused, HookDefsRefused,
// MCPServerDefsRefused and ChannelDefsRefused count the rows of those sections
// not written because the validator the call site wired refused them — what
// an author on this host could not have created; the warnings name each row
// and the reason.
//
// TeamChannelRowsSkipped counts the channel messages and cursors of a team's
// own channels not written because that team is not defined in their tenant
// here (team_channels.go); the warnings name each team.
//
// VolumeDirsCreated counts the dynamic volume directories this restore
// created under the host's dynamic root; a restored volume whose directory
// was already there is in VolumeDefsRestored but not here.
type RestoreResult struct {
	UsersRestored                  int      `json:"users_restored"`
	TokenLimitsRestored            int      `json:"token_limits_restored"`
	OAuthCredentialsRestored       int      `json:"oauth_credentials_restored,omitempty"`
	ACLListsRestored               int      `json:"acl_lists_restored"`
	ACLGroupsRestored              int      `json:"acl_groups_restored"`
	ACLMembersRestored             int      `json:"acl_members_restored"`
	ACLBindingsRestored            int      `json:"acl_bindings_restored"`
	UsageCarryRestored             int      `json:"usage_carry_restored"`
	AgentDefsRestored              int      `json:"agent_defs_restored"`
	AgentDefActiveRestored         int      `json:"agent_def_active_restored"`
	SkillDefsRestored              int      `json:"skill_defs_restored"`
	SkillDefActiveRestored         int      `json:"skill_def_active_restored"`
	TeamDefsRestored               int      `json:"team_defs_restored"`
	TeamDefActiveRestored          int      `json:"team_def_active_restored"`
	HookDefsRestored               int      `json:"hook_defs_restored"`
	HookDefActiveRestored          int      `json:"hook_def_active_restored"`
	MCPServerDefsRestored          int      `json:"mcp_server_defs_restored"`
	MCPServerDefActiveRestored     int      `json:"mcp_server_def_active_restored"`
	VolumeDefsRestored             int      `json:"volume_defs_restored"`
	VolumeDirsCreated              int      `json:"volume_dirs_created"`
	MemoryBackendDefsRestored      int      `json:"memory_backend_defs_restored"`
	MemoryBackendDefActiveRestored int      `json:"memory_backend_def_active_restored"`
	DocSourceDefsRestored          int      `json:"document_source_defs_restored"`
	DocSourceDefActiveRestored     int      `json:"document_source_def_active_restored"`
	A2AAgentDefsRestored           int      `json:"a2a_agent_defs_restored"`
	A2AAgentDefActiveRestored      int      `json:"a2a_agent_def_active_restored"`
	A2AServerCardDefsRestored      int      `json:"a2a_server_card_defs_restored"`
	A2AServerCardDefActiveRestored int      `json:"a2a_server_card_def_active_restored"`
	MemoryRestored                 int      `json:"memory_restored"`
	MemoryPendingRestored          int      `json:"memory_pending_restored"`
	ChannelDefsRestored            int      `json:"channel_defs_restored"`
	ChannelMessagesRestored        int      `json:"channel_messages_restored"`
	ChannelCursorsRestored         int      `json:"channel_cursors_restored"`
	TeamChannelRowsSkipped         int      `json:"team_channel_rows_skipped"`
	WebhookDefsRestored            int      `json:"webhook_defs_restored"`
	WebhookDefActiveRestored       int      `json:"webhook_def_active_restored"`
	ScheduleDefsRestored           int      `json:"schedule_defs_restored"`
	ScheduleDefActiveRestored      int      `json:"schedule_def_active_restored"`
	ScheduleRunStateRestored       int      `json:"schedule_run_state_restored"`
	DefsDisabledForCredentials     int      `json:"defs_disabled_for_credentials"`
	ActivePointersRefused          int      `json:"active_pointers_refused"`
	AgentDefsRefused               int      `json:"agent_defs_refused"`
	SkillDefsRefused               int      `json:"skill_defs_refused"`
	TeamDefsRefused                int      `json:"team_defs_refused"`
	HookDefsRefused                int      `json:"hook_defs_refused"`
	MCPServerDefsRefused           int      `json:"mcp_server_defs_refused"`
	ChannelDefsRefused             int      `json:"channel_defs_refused"`
	EvaluationsRestored            int      `json:"evaluations_restored"`
	PausedRunsRestored             int      `json:"paused_runs_restored"`
	QueuedRunsRestored             int      `json:"queued_runs_restored"`
	QueuedRunsRequeued             int      `json:"queued_runs_requeued"` // set by the caller's post-restore restart, not by Restore
	SynthesizedSessions            int      `json:"synthesized_sessions"`
	TranscriptEventsRestored       int      `json:"transcript_events_restored"`
	InteractionHistoryRestored     int      `json:"interaction_history_restored"`
	SqlMemScopesRestored           int      `json:"sqlmem_scopes_restored"`
	DirentsRestored                int      `json:"dirents_restored"`
	MCPServerDefsActivated         int      `json:"mcp_server_defs_activated"` // set by the caller's post-restore refresh, not by Restore
	PausedRunsResumed              int      `json:"paused_runs_resumed"`       // set by the caller's post-restore resume, not by Restore
	PausedRunsAlreadyLive          int      `json:"paused_runs_already_live"`  // likewise: paused rows a live loop still owns, left untouched
	Warnings                       []string `json:"warnings,omitempty"`

	// Refresh is what the caller's post-restore refresh pushes into the
	// in-process caches. Never serialized.
	Refresh RestoreRefresh `json:"-"`
}

// Counts is every counter on the result, keyed by name: the JSON key with its
// "_restored" suffix trimmed, so "agent_defs_restored" is "agent_defs". This
// is the extensible `restored` map every transport sends. It is derived from
// the struct rather than listed, because a hand-kept list is how five def
// counters went unreported on every transport: a counter added here reaches
// the wire with no transport change.
func (r RestoreResult) Counts() map[string]int {
	rv := reflect.ValueOf(r)
	rt := rv.Type()
	out := make(map[string]int, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Int {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[strings.TrimSuffix(name, "_restored")] = int(rv.Field(i).Int())
	}
	return out
}

// Restore reads a canonical JSON envelope (Export's output shape),
// migrates each section per the migrations registry, and writes
// rows back into the store via the SnapshotRestore* methods. The
// restore order matches the section FK dependency graph (stage 2 runs it
// from restoreSectionOrder, restore_sections.go):
//
//	users, token_limits (first: before any definition or resumed run)
//	acl               groups → members → lists → bindings (before any run)
//	agent_defs        → agent_def_active (FK: name → agent_defs.def_id)
//	volume_defs       (path derived under this host's dynamic root)
//	memory_backend_defs, document_source_defs → their active pointers
//	a2a_agent_defs, a2a_server_card_defs → their active pointers
//	memory            → memory_pending (undrained, unleased: consolidated here)
//	webhook_defs, schedule_defs → their active pointers (after channel_defs)
//	(missing-credential scan over the restored triggers and cards)
//	(sessions synth)  → paused_runs       (FK: session_id → sessions.id)
//	                  → transcript events (FK: run_id    → runs.id)
//	channel_defs      → channels.messages, .cursors (a team's own channels'
//	                   rows only for a team defined here: team_defs are above)
//	evaluations       (no FKs)
//	interaction_history (optional)
//	sqlmem            (document structure; bodies came with memory)
//	dirents           (LAST: a name points at a document, memory entry or
//	                   volume restored above, and is skipped when its target
//	                   is not here)
//
// Idempotent: a second Restore of the same envelope writes nothing. Every
// row is keyed so a re-insert is a no-op and the live row stands. For a
// definition the key is its def_id alone, so a DIFFERENT definition on a
// (tenant, name, version) the target already holds is a conflict: the
// section reports it as a warning and the live row stands. A usage carry
// keeps the larger of the stored and the incoming month-to-date total.
//
// Critical correctness detail (the architect's catch in PR 3
// planning): the snapshot's paused_runs section references
// session_id values, but sessions are NOT a captured section.
// Restore synthesizes a minimal session row per paused run with
// ID = "snap_sess_" + run.ID — deterministic so a re-run produces
// the same synthetic ID. RestoreResult.SynthesizedSessions counts
// these for operator visibility.
//
// Returns *migrations.ErrSnapshotVersionTooNew when any section's
// version is newer than the reader's CurrentVersion. Returns
// *migrations.ErrUnknownSectionVersion for corrupted / unsupported
// snapshots. Both errors carry the section + version strings for
// operator-actionable messaging, and both are returned before anything is
// written. Any other error can leave the sections before it written; the
// result it returns alongside still carries their Refresh and Warnings.
func Restore(ctx context.Context, s store.Store, raw []byte, opts RestoreOptions) (out RestoreResult, err error) {
	// Every warning, on every return path, leaves without a URL credential:
	// the ones that quote a refused body (a validator's error repeats the
	// value it refused) and any a future section adds.
	defer func() { out.Warnings = redactWarnings(out.Warnings) }()
	if s == nil {
		return RestoreResult{}, fmt.Errorf("snapshot restore: nil store")
	}
	if len(raw) == 0 {
		return RestoreResult{}, fmt.Errorf("snapshot restore: empty input")
	}

	// Stage 1: deserialise the outer envelope to discover sections
	// and their declared versions. We deserialise via a deferred
	// map[string]json.RawMessage rather than directly into Envelope
	// so each section's raw bytes can pass through Migrate() before
	// being typed-decoded — that's the migration mechanism's hook.
	var outer struct {
		SchemaVersion int             `json:"schema_version"`
		CreatedAt     time.Time       `json:"created_at"`
		Sections      json.RawMessage `json:"sections"`
		Checksum      string          `json:"checksum,omitempty"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return RestoreResult{}, fmt.Errorf("snapshot restore: parse envelope: %w", err)
	}
	if outer.SchemaVersion > SchemaVersion {
		return RestoreResult{}, fmt.Errorf("snapshot restore: envelope schema_version %d is newer than reader %d", outer.SchemaVersion, SchemaVersion)
	}

	// exp7 I4: verify the integrity digest BEFORE any decode/insert, when
	// present. outer.Sections holds the raw "sections" bytes exactly as they
	// appear in the document (json.RawMessage), so hashing them reproduces
	// Export's digest. A snapshot without a checksum (captured before I4)
	// skips verification and restores unchanged — additive, not a gate.
	if outer.Checksum != "" {
		if got := sectionChecksum(outer.Sections); got != outer.Checksum {
			return RestoreResult{}, fmt.Errorf("snapshot restore: checksum mismatch (want %s, got %s) — snapshot is truncated or tampered", outer.Checksum, got)
		}
	}

	// Decode the section map from the raw sections bytes for the per-section
	// migration + insert logic below.
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(outer.Sections, &sections); err != nil {
		return RestoreResult{}, fmt.Errorf("snapshot restore: parse sections: %w", err)
	}

	result := RestoreResult{}

	// A section this reader does not know was written by a newer loomcycle.
	// Its rows are not restored; say so, rather than let the operator find out
	// when something the source had is missing here. Sorted for a stable
	// warning order.
	var unknown []string
	for name := range sections {
		if !migrations.KnownSection(name) {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"section %s present but not understood by this reader; upgrade loomcycle to restore it", name))
	}

	// Stage 1.5: every section's version, before anything is written, so a
	// version this reader cannot read aborts with nothing written.
	if err := checkSectionVersions(sections, opts); err != nil {
		return result, err
	}

	// Stage 2: per-section migration + decode + insert, in the order of
	// restoreSectionOrder (restore_sections.go). Order matters for FK and
	// naming reasons; each entry there says what it must come after, and
	// TestRestoreSectionOrder_SatisfiesDependencies pins it.
	rs := &restoreRun{s: s, opts: opts, sections: sections, result: &result, scan: &credScan{}}
	for _, sec := range restoreSectionOrder() {
		if err := sec.restore(ctx, rs); err != nil {
			return result, err
		}
	}

	// Stage 3: trigger an immediate resolver probe so the matrix is
	// populated before the operator calls Resume. Done last so any
	// errors above are returned before the resolver work.
	if opts.ForceProbe != nil {
		opts.ForceProbe(ctx)
	}

	return result, nil
}

// checkSectionVersions runs every section this restore will decode through its
// migration, discarding the output, so a section this reader cannot read
// refuses the whole restore before its first write. Without it a too-new
// later section failed after users, token_limits and usage_carry were
// already written, and a retry of the fixed envelope then found them present
// and reported nothing to refresh, leaving the restored budgets unenforced
// until a restart.
//
// The skips mirror the decode sites in Restore: a section it never decodes (an
// unknown one, history not asked for, SQL Memory not wired here) is not
// checked, so a section it skips cannot refuse a restore it never refused.
// Sorted, so the section named in the error is stable.
func checkSectionVersions(sections map[string]json.RawMessage, opts RestoreOptions) error {
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch {
		case !migrations.KnownSection(name):
			continue
		case name == migrations.SectionInteractionHistory && !opts.IncludeHistory:
			continue
		case name == migrations.SectionSqlMem && opts.SqlMem == nil:
			continue
		}
		if _, err := migrateSection(name, sections[name]); err != nil {
			return err
		}
	}
	return nil
}

// migrateSection peeks at a section's version and runs its raw bytes through
// the per-section migration registry. Returns the migration error verbatim
// (operator gets the version + section + suggested action).
func migrateSection(section string, raw json.RawMessage) (json.RawMessage, error) {
	// Peek at the section's version. The section wrapper has
	// {"version": "1.0", ...} as its outer shape.
	var versioned struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &versioned); err != nil {
		return nil, fmt.Errorf("snapshot section %s: parse version: %w", section, err)
	}
	if versioned.Version == "" {
		// Tolerant default — pre-versioned sections (corrupted /
		// hand-written) get current-version semantics. Future-strict
		// readers may flip this to refuse.
		versioned.Version = migrations.CurrentVersion
	}
	return migrations.Migrate(section, versioned.Version, raw)
}

// decodeWithMigration runs the raw section bytes through the
// per-section migration registry then JSON-decodes into the typed
// destination.
func decodeWithMigration(section string, raw json.RawMessage, dst any) error {
	migrated, err := migrateSection(section, raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(migrated, dst); err != nil {
		return fmt.Errorf("snapshot section %s: decode migrated bytes: %w", section, err)
	}
	return nil
}

// restoreQueuedRun inserts one captured queued run as queued, with the
// request it was queued with and its events so far, and names it for the
// caller to queue again (Refresh.QueuedRunIDs). A run already on the target
// is left as it is: it has its own waiter there, or has moved on. The row is
// stamped with replicaID, the replica that queues it again.
func restoreQueuedRun(ctx context.Context, s store.Store, e QueuedRunEntry, replicaID string, result *RestoreResult) {
	sessionID := e.SessionID
	if sessionID == "" {
		sessionID = "snap_sess_" + e.RunID
	}
	sessInserted, err := s.SnapshotRestoreSession(ctx, store.Session{
		ID: sessionID, TenantID: e.TenantID, Agent: e.Agent, CreatedAt: e.StartedAt, UserID: e.UserID,
	})
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: session %s: %v", e.RunID, sessionID, err))
		return
	}
	if sessInserted && e.SessionID == "" {
		result.SynthesizedSessions++
	}
	row := store.Run{
		ID: e.RunID, SessionID: sessionID, TenantID: e.TenantID, Status: store.RunQueued,
		StartedAt: e.StartedAt, AgentID: e.AgentID, UserID: e.UserID, UserTier: e.UserTier,
		Interactive: e.Interactive, OperatorKeyRestricted: e.OperatorKeyRestricted, Isolated: e.Isolated,
		CredentialsNoUserScope: e.CredentialsNoUserScope,
		Retention:              e.Retention, ParentContext: e.ParentContext,
		IdempotencyKey: e.IdempotencyKey, DeliveryAltKey: e.DeliveryAltKey,
		ReplicaID: replicaID,
	}
	// As for a paused run: a key another run here already holds is dropped,
	// and the run still comes back.
	if keys := nonEmpty(e.IdempotencyKey, e.DeliveryAltKey); len(keys) > 0 {
		held, found, err := s.RunByDeliveryKeys(ctx, keys)
		switch {
		case err != nil:
			result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: check its dedup keys: %v — restored without them", e.RunID, err))
			row.IdempotencyKey, row.DeliveryAltKey = "", ""
		case found && held.ID != e.RunID:
			result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: run %s on this target already holds its dedup key — restored without it", e.RunID, held.ID))
			row.IdempotencyKey, row.DeliveryAltKey = "", ""
		}
	}
	inserted, err := s.SnapshotRestoreRun(ctx, row)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: %v", e.RunID, err))
		return
	}
	if !inserted {
		return
	}
	result.QueuedRunsRestored++
	// The envelope is input, not what Capture is known to write: a hand-made
	// or edited one is checked here as Capture would have shaped it.
	if input, ok := restorableQueuedInput(e); ok {
		if err := s.SetQueuedInput(ctx, e.RunID, input); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: its request was not written (%v); it will be failed when queued again", e.RunID, err))
		}
	} else if len(e.QueuedInput) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: its request was not restored (a no-trace run stores none, or it is not a JSON object); it will be failed when queued again", e.RunID))
	}
	if e.SecretsDropped {
		result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: its per-run secrets are not carried by a snapshot; it runs without them", e.RunID))
	}
	if len(e.TranscriptEvents) > 0 {
		evs := make([]store.Event, 0, len(e.TranscriptEvents))
		for _, te := range e.TranscriptEvents {
			evs = append(evs, store.Event{Timestamp: time.Unix(0, te.TsNs), Type: te.Type, Payload: te.Payload})
		}
		written, err := s.SnapshotRestoreRunEvents(ctx, e.RunID, sessionID, evs)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("queued_run %s: its %d captured events were not written: %v", e.RunID, len(evs), err))
		}
		result.TranscriptEventsRestored += written
	}
	result.Refresh.QueuedRunIDs = append(result.Refresh.QueuedRunIDs, e.RunID)
}

// restorableQueuedInput is the request to store for a restored queued run, or
// false when none may be: a no-trace run stores no prompt anywhere, and bytes
// that are not a JSON object are not a request. Sealed secrets are removed
// whatever the envelope says — a snapshot never brings one in, even sealed.
func restorableQueuedInput(e QueuedRunEntry) (json.RawMessage, bool) {
	if len(e.QueuedInput) == 0 || e.Retention == retentionmode.None {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(e.QueuedInput, &obj); err != nil {
		return nil, false
	}
	input, _ := StripQueuedSecrets(e.QueuedInput)
	return input, len(input) > 0
}

// nonEmpty returns the non-empty strings of vals.
func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
