package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// Snapshot admin endpoints — v0.8.17 Pause/Resume/Snapshot (PR 2).
// Wire shape:
//
//   POST   /v1/_snapshots                    — capture a new snapshot
//     body: {"label?": "...", "include_history?": false, "include_history_since?": "RFC3339"}
//     → 201 {"id": "snap_...", "byte_size": N, "created_at": "..."}
//     → 413 if exceeds LOOMCYCLE_SNAPSHOT_MAX_BYTES (or operator-supplied cap)
//
//   GET    /v1/_snapshots?label_contains=&limit=200
//     → 200 {"entries": [...metadata only, no JSON payload...]}
//
//   GET    /v1/_snapshots/{id}                — full row including JSON
//     → 200 {"id": ..., "json_content": {...}}
//     → 404 *ErrNotFound
//
//   DELETE /v1/_snapshots/{id}
//     → 204 idempotent (true OR false from store.SnapshotDelete both
//       map to 204 — operators scripting cleanup never see 404 on a
//       missing row, only "row no longer exists")
//
// Auth: bearer-token middleware applied at mux registration time.
// No agent surface — these are operator-only.

// snapshotCreateRequest is the body of POST /v1/_snapshots.
type snapshotCreateRequest struct {
	Label               string `json:"label,omitempty"`
	IncludeHistory      bool   `json:"include_history,omitempty"`
	IncludeHistorySince string `json:"include_history_since,omitempty"` // RFC3339; optional even when IncludeHistory=true
	MaxBytes            int64  `json:"max_bytes,omitempty"`             // override; 0 = use DefaultMaxBytes
}

// snapshotCreateResponse is the 201 response — metadata only; the
// caller fetches the full JSON via GET /v1/_snapshots/{id}.
type snapshotCreateResponse struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Label         string `json:"label,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	ByteSize      int64  `json:"byte_size"`
	// Warnings names what the capture carried that the operator should act on
	// — a header value that looks like a literal credential — by location,
	// never by value. The findings also travel in the envelope.
	Warnings []string `json:"warnings,omitempty"`
}

// snapshotListResponse wraps the metadata listing.
type snapshotListResponse struct {
	Entries []snapshotListEntryResponse `json:"entries"`
}

type snapshotListEntryResponse struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Label         string `json:"label,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	ByteSize      int64  `json:"byte_size"`
}

// snapshotGetResponse carries the full row including the JSON
// payload. JSONContent is emitted as a raw nested object — the
// envelope is already valid JSON, so re-marshalling would be wasted.
type snapshotGetResponse struct {
	ID            string          `json:"id"`
	CreatedAt     string          `json:"created_at"`
	Label         string          `json:"label,omitempty"`
	SchemaVersion int             `json:"schema_version"`
	ByteSize      int64           `json:"byte_size"`
	JSONContent   json.RawMessage `json:"json_content"`
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req snapshotCreateRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		// Empty body is fine (all fields optional); only error on
		// malformed JSON.
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON object (or empty)")
		return
	}
	opts := snapshot.CaptureOptions{
		Label:          req.Label,
		MaxBytes:       req.MaxBytes,
		IncludeHistory: req.IncludeHistory,
		Channels:       channelConfigForSnapshot(s.cfg()),
	}
	if req.IncludeHistorySince != "" {
		ts, err := time.Parse(time.RFC3339, req.IncludeHistorySince)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_since", "include_history_since must be RFC3339")
			return
		}
		opts.IncludeHistorySince = ts
	}
	// Guard the typed-nil interface trap: only set SqlMem when the concrete
	// manager is non-nil (RFC AA Phase 3e; SQL Memory disabled ⇒ section absent).
	if s.sqlMem != nil {
		opts.SqlMem = s.sqlMem
		opts.SqlMemMaxScopeBytes = s.cfg().Storage.SqlMemSnapshotMaxScopeBytes // 3f.2 per-scope cap
	}
	captured, err := snapshot.CaptureReport(r.Context(), s.store, opts)
	if err != nil {
		var tooLarge *snapshot.ErrSnapshotTooLarge
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "snapshot_too_large", tooLarge.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "capture_failed", err.Error())
		return
	}
	row := captured.Row
	if err := s.store.SnapshotCreate(r.Context(), *row); err != nil {
		// Defensive: SnapshotCreate's id collision is rare (8 hex
		// bytes + ms timestamp) but possible under scripted bulk
		// captures. Surface as 409 so the caller can retry.
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			writeJSONError(w, http.StatusConflict, "snapshot_id_conflict", conflict.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "persist_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(snapshotCreateResponse{
		ID:            row.ID,
		CreatedAt:     row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Label:         row.Label,
		SchemaVersion: row.SchemaVersion,
		ByteSize:      row.ByteSize,
		Warnings:      captured.Warnings,
	})
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	labelContains := r.URL.Query().Get("label_contains")
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be a non-negative integer")
			return
		}
		// limit=0 keeps the default (200). The codebase convention is
		// "0 = use default", not "no limit" — an operator scripting
		// curl ?limit=0 expecting empty results would otherwise get
		// the entire table. True unlimited isn't a wire feature; bump
		// the limit param when more rows are needed.
		if n > 0 {
			limit = n
		}
	}
	rows, err := s.store.SnapshotList(r.Context(), labelContains, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	entries := make([]snapshotListEntryResponse, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, snapshotListEntryResponse{
			ID:            r.ID,
			CreatedAt:     r.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Label:         r.Label,
			SchemaVersion: r.SchemaVersion,
			ByteSize:      r.ByteSize,
		})
	}
	writeJSON(w, http.StatusOK, snapshotListResponse{Entries: entries})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, err := s.store.SnapshotGet(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "snapshot_not_found", "no snapshot with id "+id)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshotGetResponse{
		ID:            row.ID,
		CreatedAt:     row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Label:         row.Label,
		SchemaVersion: row.SchemaVersion,
		ByteSize:      row.ByteSize,
		JSONContent:   row.JSONContent,
	})
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Idempotent: both (true) and (false) map to 204. Operators
	// scripting cleanup never see 404 on a missing row.
	if _, err := s.store.SnapshotDelete(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// snapshotRestoreRequest is the body of POST /v1/_snapshots/{id}/restore.
type snapshotRestoreRequest struct {
	// IncludeHistory toggles restoring the optional
	// interaction_history section. Default false (the running-state
	// restore most operators want).
	IncludeHistory bool `json:"include_history,omitempty"`
	// JSON, when set, overrides the {id} path — restore from the
	// supplied envelope rather than looking up by id. Used by the
	// CLI's `loomcycle snapshot restore <file.json>` flow which
	// posts the envelope directly without persisting first.
	JSON json.RawMessage `json:"json,omitempty"`
}

// snapshotRestoreResponse mirrors snapshot.RestoreResult for the wire.
type snapshotRestoreResponse struct {
	AgentDefsRestored       int `json:"agent_defs_restored"`
	AgentDefActiveRestored  int `json:"agent_def_active_restored"`
	MemoryRestored          int `json:"memory_restored"`
	ChannelMessagesRestored int `json:"channel_messages_restored"`
	ChannelCursorsRestored  int `json:"channel_cursors_restored"`
	EvaluationsRestored     int `json:"evaluations_restored"`
	PausedRunsRestored      int `json:"paused_runs_restored"`
	// PausedRunsResumed is how many of the restored paused runs were
	// re-dispatched as live loops (F42 / RFC X Phase 2). May be < restored
	// when a run's agent no longer resolves or it isn't auto-resumable; those
	// are flagged failed and surfaced in Warnings.
	PausedRunsResumed          int      `json:"paused_runs_resumed"`
	SynthesizedSessions        int      `json:"synthesized_sessions"`
	TranscriptEventsRestored   int      `json:"transcript_events_restored"`
	InteractionHistoryRestored int      `json:"interaction_history_restored"`
	Warnings                   []string `json:"warnings,omitempty"`

	// Restored is every restore counter keyed by name
	// (snapshot.RestoreResult.Counts), the extensible form. The typed counters
	// above stay frozen at the subset they always carried; a new section adds
	// a key here and touches no transport.
	Restored map[string]int `json:"restored"`
}

func (s *Server) handleRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req snapshotRestoreRequest
	// The body may carry an inline snapshot envelope (req.json) up to the
	// snapshot ceiling, so the cap is the envelope ceiling + envelope-field
	// headroom — not the 1 MiB control-body cap used elsewhere.
	r.Body = http.MaxBytesReader(w, r.Body, snapshot.DefaultMaxBytes+(1<<20))
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON object (or empty)")
		return
	}

	// Resolve the envelope bytes — either the supplied JSON or
	// fetch by id.
	var rawBytes []byte
	if len(req.JSON) > 0 {
		rawBytes = req.JSON
	} else {
		row, err := s.store.SnapshotGet(r.Context(), id)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				writeJSONError(w, http.StatusNotFound, "snapshot_not_found", "no snapshot with id "+id)
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "get_failed", err.Error())
			return
		}
		rawBytes = row.JSONContent
	}

	// Restore — passes ForceProbe so the resolver matrix is
	// refreshed before this returns. Operators can call Resume
	// immediately after a successful restore without waiting for
	// the periodic probe.
	result, err := snapshot.Restore(r.Context(), s.store, rawBytes, s.snapshotRestoreOptions(r.Context(), req.IncludeHistory))
	if err != nil {
		// Migration / version errors map to 422 (semantically valid
		// JSON, semantically invalid state). The error message
		// carries the section + version strings so operators see
		// what to do.
		var tooNew *migrations.ErrSnapshotVersionTooNew
		var unknown *migrations.ErrUnknownSectionVersion
		switch {
		case errors.As(err, &tooNew):
			writeJSONError(w, http.StatusUnprocessableEntity, "snapshot_version_too_new", err.Error())
			return
		case errors.As(err, &unknown):
			writeJSONError(w, http.StatusUnprocessableEntity, "snapshot_version_unknown", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "restore_failed", err.Error())
		return
	}

	s.finishRestore(r.Context(), &result)

	writeJSON(w, http.StatusOK, snapshotRestoreResponse{
		Restored:                   result.Counts(),
		AgentDefsRestored:          result.AgentDefsRestored,
		AgentDefActiveRestored:     result.AgentDefActiveRestored,
		MemoryRestored:             result.MemoryRestored,
		ChannelMessagesRestored:    result.ChannelMessagesRestored,
		ChannelCursorsRestored:     result.ChannelCursorsRestored,
		EvaluationsRestored:        result.EvaluationsRestored,
		PausedRunsRestored:         result.PausedRunsRestored,
		PausedRunsResumed:          result.PausedRunsResumed,
		SynthesizedSessions:        result.SynthesizedSessions,
		TranscriptEventsRestored:   result.TranscriptEventsRestored,
		InteractionHistoryRestored: result.InteractionHistoryRestored,
		Warnings:                   result.Warnings,
	})
}

// snapshotRestoreOptions is the RestoreOptions every restore call site passes
// (the HTTP handler and the connector behind gRPC and MCP), so no transport
// can restore with less validation than another.
//
//   - Validators: the authoring validators of every section that has one —
//     the newer sections, which are never restored unvalidated, and the def
//     sections snapshots carried first (agent, skill, team, hook, MCP server
//     and channel defs). A row a validator refuses is skipped with a
//     warning — restoring never widens what an author could create here.
//     The MCP server and hook validators judge against THIS host: its
//     allowlists and stdio opt-in, and its code-hook runner.
//   - CredentialExists: the metadata-only keyability probe (never decrypts),
//     so the missing-credential scan names a $cred: reference this host
//     cannot resolve. nil when no credential store is wired: the scan says
//     it did not check.
//   - EnvSet: whether an env var a restored definition names is set here.
//     Only a yes or no leaves the process environment.
//   - VolumeRoot / StaticVolumeNames: this host's dynamic volume root, under
//     which every restored dynamic volume's path is derived, and its static
//     volumes, whose names a restored one may not take.
//   - DocumentExists: whether a document a restored Path name points at is on
//     this host, asked without provisioning a SQL Memory scope. nil when SQL
//     Memory is off here: no document can be, so no document name restores.
//
// ctx is the restore's own: the channel validator resolves HookDef references
// through the store under it.
func (s *Server) snapshotRestoreOptions(ctx context.Context, includeHistory bool) snapshot.RestoreOptions {
	var compileCode func(string) error
	if s.codeHooks != nil {
		compileCode = s.codeHooks.Compile
	}
	opts := snapshot.RestoreOptions{
		IncludeHistory: includeHistory,
		Validators: map[string]func(json.RawMessage) error{
			// The def sections snapshots carried first. Agent, skill and team
			// bodies are checked for what they carry themselves — the inline
			// hook webhooks above all; hook and channel defs for their
			// headers, urls and references.
			migrations.SectionAgentDefs:         builtin.ValidateAgentDefBody,
			migrations.SectionSkillDefs:         builtin.ValidateSkillDefBody,
			migrations.SectionTeamDefs:          builtin.ValidateTeamDefBody,
			migrations.SectionHookDefs:          builtin.HookDefBodyValidator(compileCode),
			migrations.SectionChannelDefs:       s.restoredChannelDefValidator(ctx),
			migrations.SectionWebhookDefs:       builtin.ValidateWebhookDefBody,
			migrations.SectionA2AAgentDefs:      builtin.ValidateA2AAgentDefBody,
			migrations.SectionA2AServerCardDefs: builtin.ValidateA2AServerCardDefBody,
			// base_url + api_key_env is an exfiltration pair: the key named
			// is sent to the URL, so both go through the authoring checks.
			migrations.SectionMemoryBackendDefs: builtin.ValidateMemoryBackendDefBody,
			migrations.SectionDocSourceDefs:     builtin.ValidateDocumentSourceDefBody,
			// A dynamic volume is re-checked as create checks it; its path
			// is derived under VolumeRoot below, never read from the file.
			migrations.SectionVolumeDefs: builtin.ValidateVolumeDefBody,
			// A Path name is re-checked as the tools write one: canonical
			// path, known scope and kind, a ref of that kind's shape.
			migrations.SectionDirents: builtin.ValidateDirentEntry,
		},
		CredentialExists: s.credKeyable,
		EnvSet:           func(name string) bool { return os.Getenv(name) != "" },
	}
	// The dynamic root and static volume names the VolumeDef tool resolves
	// against: a restored volume lands where a create here would put it, and
	// never under a name a static volume already holds.
	if cfg := s.cfg(); cfg != nil {
		// An MCP server def is dialed at its url, so it restores only when an
		// author here could register it: this host's allowlists, its stdio
		// opt-in. With no config there is nothing to judge against; the section
		// then restores unvalidated and says so.
		opts.Validators[migrations.SectionMCPServerDefs] = builtin.MCPServerDefBodyValidator(cfg)
		if root, ok := builtin.DynamicVolumeRoot(cfg); ok {
			opts.VolumeRoot = root
		}
		for name := range cfg.Volumes {
			opts.StaticVolumeNames = append(opts.StaticVolumeNames, name)
		}
	}
	if s.resolver != nil {
		opts.ForceProbe = s.resolver.ForceProbe
	}
	if s.sqlMem != nil {
		opts.SqlMem = s.sqlMem // RFC AA Phase 3e
		opts.DocumentExists = builtin.SnapshotDocumentExists(s.sqlMem)
	}
	return opts
}

// restoredChannelDefValidator is a runtime channel create's validation, for a
// snapshot restore to run over each channel it would write (it is given the
// snapshot.ChannelDefEntry as JSON): a name, scope and semantic a create
// accepts, no name a yaml channel here already holds, and hooks that validate
// and whose HookDef references resolve in the channel's tenant or the shared
// one — hook defs restore before channel defs, so a reference to one this
// restore brought back resolves, and one it refused does not. The admin-only
// check on a global channel is not repeated: a restore is admin-only already.
func (s *Server) restoredChannelDefValidator(ctx context.Context) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		var e snapshot.ChannelDefEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("does not decode as a channel definition: %w", err)
		}
		if cfg := s.cfg(); cfg != nil {
			if _, yaml := cfg.Channels[e.Name]; yaml {
				return fmt.Errorf("%w: %q", connector.ErrChannelYamlImmutable, e.Name)
			}
		}
		if !validChannelName(e.Name) {
			return fmt.Errorf("name must match [A-Za-z0-9_-]{1,128}")
		}
		switch e.Scope {
		case "global", "agent", "user", "tenant":
		default:
			return fmt.Errorf("scope must be one of global|tenant|user|agent, got %q", e.Scope)
		}
		switch e.Semantic {
		case "queue", "topic":
		default:
			return fmt.Errorf("semantic must be one of queue|topic, got %q", e.Semantic)
		}
		if e.DefaultTTL < 0 || e.MaxMessages < 0 {
			return fmt.Errorf("default_ttl and max_messages must be >= 0")
		}
		if store.NoChannelHooks(e.Hooks) {
			return nil
		}
		var h hooks.EventHooks
		if err := json.Unmarshal(e.Hooks, &h); err != nil {
			return fmt.Errorf("hooks do not decode: %w", err)
		}
		_, err := s.checkChannelHooks(ctx, e.Name, e.Publisher, e.TenantID, h)
		return err
	}
}

// finishRestore is what every restore call site owes once snapshot.Restore has
// written the rows: the caches over the restored tables first, so a run
// resumed next sees what the restore wrote, then the paused runs re-dispatched
// as live loops (F42 / RFC X Phase 2 — reconstructed from their transcripts),
// so a snapshotted mid-run genuinely continues on this instance.
// ResumePausedRuns takes every pause_state='paused' row in the store, the
// restored ones among them, as the boot-time resume does.
// The HTTP handler and the connector (gRPC, MCP) both call it, so neither the
// step nor its order can differ by transport.
//
// A run whose agent no longer resolves, or that is not auto-resumable, is
// flagged failed and named in the warnings; PausedRunsResumed counts the ones
// re-dispatched. A paused row that a live loop still owns — parked here by a
// runtime pause, or owned by another replica that is alive — is left exactly
// as it is and counted in PausedRunsAlreadyLive, neither resumed nor warned
// about.
// The resume gets a context that outlives the request, so a slow re-dispatch
// or a long-lived resumed loop does not block the response and is not killed
// when the caller hangs up.
//
// No advisory lock, as the restore endpoint never had one: the lock
// (coord.LockKeyResumePausedRuns) is for boot, where every replica would
// otherwise scan the same store at once. A restore runs on the one replica
// that received it; a run a live loop owns, on this replica or another, is
// left alone by the resume itself, before it writes anything.
func (s *Server) finishRestore(ctx context.Context, result *snapshot.RestoreResult) {
	s.postRestoreRefresh(ctx, result)
	r := s.resumePausedRunsReport(context.WithoutCancel(ctx))
	result.PausedRunsResumed = r.Resumed
	result.PausedRunsAlreadyLive = r.AlreadyLive
	result.Warnings = append(result.Warnings, r.Warnings...)
}

// postRestoreRefresh brings the in-process caches over restored tables in
// line with what Restore just wrote (RFC DP §4.11). Both restore call sites
// run it after Restore returns and before any paused run is resumed, so a
// resumed run sees the restored state rather than what the caches held.
//
// It refreshes only THIS replica. Another replica's caches catch up at its
// next boot — the limit every boot-filled cache here has.
func (s *Server) postRestoreRefresh(ctx context.Context, result *snapshot.RestoreResult) {
	// Restored MCP server defs go live without a restart.
	if s.mcpRegistryRefresh != nil {
		n, err := s.mcpRegistryRefresh(ctx)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"mcp_server_defs: reloading the MCP server registry failed, so restored definitions are not live until a restart: %v", err))
		}
		result.MCPServerDefsActivated = n
	}
	// Restored budgets and carried usage are enforced at once, not at the next
	// boot's Seed. Only what the restore wrote is pushed: a budget it left
	// alone is already cached, and a carry is added by how much it GREW, so
	// the counters stay ledger + stored carry across a re-restore. Both are
	// in-memory writes that cannot fail — no store re-read that could leave a
	// persisted budget unenforced.
	for _, row := range result.Refresh.TokenLimits {
		s.limits.PutLimit(row)
	}
	for _, c := range result.Refresh.UsageCarried {
		s.limits.AddCarried(c.Month, c.TenantID, c.UserID, c.Tokens)
	}
}

func (s *Server) handleExportSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, err := s.store.SnapshotGet(r.Context(), id)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "snapshot_not_found", "no snapshot with id "+id)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	// Serve the raw JSON content with a Content-Disposition header
	// so CLI consumers using `curl -O` save it under the snapshot's
	// id. The bytes are already canonical (Capture stored them via
	// json.Marshal); no re-marshal needed.
	//
	// Sanitize the id before splicing into the header value: snapshot
	// IDs from mintID are clean ("snap_<ms>_<hex>") but the id here is
	// a path param under operator control, and trusting it would
	// permit header injection / response-splitting via embedded
	// quotes, CR, or LF. Replace those with '_' rather than rejecting
	// so a misuse still gets the body (just with a sanitized filename).
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeSnapshotFilename(id)+`.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(row.JSONContent)
}

// sanitizeSnapshotFilename strips characters that would break the
// Content-Disposition header value (double quote, CR, LF) and replaces
// them with '_'. mintID-produced ids never trip this in practice; it's
// defense-in-depth against operator-supplied or future-format ids.
func sanitizeSnapshotFilename(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, id)
}

// channelConfigForSnapshot translates cfg.Channels (operator-yaml
// shape, map[string]config.Channel) into the snapshot envelope's
// []snapshot.ChannelConfigEntry. Stable ordering across captures
// would require sorting; today the map iteration order is Go's
// random-per-run order, but snapshot.Capture's deterministic-
// ordering tests focus on store-read sections (memory, evaluations)
// rather than the operator-config passthrough. If operators report
// nondeterminism here, sort by Name in a follow-up.
func channelConfigForSnapshot(cfg *config.Config) []snapshot.ChannelConfigEntry {
	if cfg == nil || len(cfg.Channels) == 0 {
		return nil
	}
	out := make([]snapshot.ChannelConfigEntry, 0, len(cfg.Channels))
	for name, ch := range cfg.Channels {
		out = append(out, snapshot.ChannelConfigEntry{
			Name:        name,
			Description: ch.Semantic,
			Scope:       ch.Scope,
			TTLSeconds:  ch.DefaultTTL,
			MaxMessages: ch.MaxMessages,
		})
	}
	return out
}
