// Package audit is the RFC L OSS append-only audit sink. Every
// OperatorTokenDef mutation (create/rotate/retire) writes one JSONL line
// recording WHO did WHAT to WHICH principal — never any token plaintext
// or hash (the Event struct deliberately has no field for them). The OSS
// sink here is a per-replica local file; operators wire their own pipeline
// (logrotate/fluentd/Loki). The tamper-evident, store-backed stream that
// every replica appends to is internal/auditchain (RFC FZ Part E); this
// file sink is its optional mirror.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Event is one audit record. Times are stamped by the sink (callers
// never pass a clock). NO token plaintext or hash — by construction.
type Event struct {
	TS               time.Time `json:"ts"`
	ActorTenant      string    `json:"actor_tenant,omitempty"`
	ActorSubject     string    `json:"actor_subject,omitempty"`
	ActorTokenSuffix string    `json:"actor_token_suffix,omitempty"`
	Action           string    `json:"action"` // create | rotate | retire
	TargetDefID      string    `json:"target_def_id,omitempty"`
	TargetTenant     string    `json:"target_tenant,omitempty"`
	TargetSubject    string    `json:"target_subject,omitempty"`
	TargetName       string    `json:"target_name,omitempty"`
	ScopesBefore     []string  `json:"scopes_before,omitempty"`
	ScopesAfter      []string  `json:"scopes_after,omitempty"`
	SourceAddr       string    `json:"source_addr,omitempty"`

	// --- RFC EY access control (Action = "acl_denied" | "acl_unavailable" |
	// "acl_shadow_diverged") --- the resource travels in TargetName as
	// `<type>:<name>`; these add the verb, the engine's outcome and the
	// rules that decided (`<acl>#<index>:<effect>`). Never a bearer.
	AclAction  string   `json:"acl_action,omitempty"`
	AclOutcome string   `json:"acl_outcome,omitempty"`
	AclRules   []string `json:"acl_rules,omitempty"`
	// AclRunID is the run a divergence was found in: set for what shadow
	// mode lets a run keep at its start (a tool, a volume's write, a
	// keyspace, the skills) or lets it spawn, empty for a request's gate.
	AclRunID string `json:"acl_run_id,omitempty"`

	// --- RFC AA SQL Memory (Action = "sql_query" | "sql_exec") ---
	// All omitempty so a non-SQL audit record is byte-identical to before.
	// SqlStatement is the REDACTED statement; it is omitted entirely when the
	// operator runs audit_mode=metadata (statements never recorded).
	SqlOp         string `json:"sql_op,omitempty"`        // "sql_query" | "sql_exec"
	SqlScope      string `json:"sql_scope,omitempty"`     // "agent" | "user" | "run"
	SqlScopeID    string `json:"sql_scope_id,omitempty"`  // resolved scope id (agent name / user id / run id)
	SqlStatement  string `json:"sql_statement,omitempty"` // redacted; omitted in metadata mode
	SqlRows       int64  `json:"sql_rows,omitempty"`      // rows returned (query) or affected (exec)
	SqlDurationMs int64  `json:"sql_duration_ms,omitempty"`
	SqlError      string `json:"sql_error,omitempty"`

	// --- RFC CF §7 subject erasure (Action = "erase_intent" | "erase_result") ---
	//
	// TWO RECORDS, and the pair is the point. `erase_intent` is written BEFORE any
	// deletion and its failure REFUSES the erasure; `erase_result` is written after and
	// carries what actually went. A crash between them still leaves evidence that an
	// erasure was attempted, by whom, against which subject — which a single
	// after-the-fact record cannot promise for an operation nothing can undo.
	//
	// The subject and tenant travel in TargetSubject / TargetTenant above; these add
	// only what is specific to an erasure.
	EraseDryRun bool `json:"erase_dry_run,omitempty"`
	// ErasePlanes is plane → rows removed. Recorded rather than a total because
	// "which planes were touched" is the question an auditor actually has.
	ErasePlanes map[string]int `json:"erase_planes,omitempty"`
	// EraseRetained is plane → why it was kept. An erasure record listing only its
	// deletions would read as complete when it is not.
	EraseRetained map[string]string `json:"erase_retained,omitempty"`
	EraseErrors   []string          `json:"erase_errors,omitempty"`

	// --- Run-tree delete (Action = "run_tree_delete") ---
	//
	// One record per call, written after the delete: ids and row counts, never
	// content. The tenant of the deleted tree travels in TargetTenant.
	DeleteRunID     string `json:"delete_run_id,omitempty"`
	DeleteSessionID string `json:"delete_session_id,omitempty"`
	// DeleteCounts is table → rows removed.
	DeleteCounts map[string]int `json:"delete_counts,omitempty"`
	// DeleteRefused is the error code of a call that deleted nothing because
	// it was refused (a live run in the tree, a tree over the size cap).
	DeleteRefused string `json:"delete_refused,omitempty"`

	// --- No-trace override (Action = "retention_override_set" |
	// "retention_override_clear") ---
	//
	// An operator switching storage back on for an agent whose runs store
	// none. The agent travels in TargetName, its tenant in TargetTenant.
	// RetentionUntil is when the override stops applying (set only).
	RetentionUntil string `json:"retention_until,omitempty"`

	// --- Tenant telemetry export (Action = "tenant_telemetry_set" |
	// "tenant_telemetry_delete") ---
	//
	// An operator pointing a tenant's traces at a collector, or stopping it.
	// The tenant travels in TargetTenant, the collector endpoint (no
	// credentials, no query: the policy refuses both) in TargetName on a set.
	// TelemetryContentCapture is the set's content capture, "on" or "off":
	// who turned on sending the tenant's prompts and answers out, and when.
	TelemetryContentCapture string `json:"telemetry_content_capture,omitempty"`

	// --- Document export (Action = "document_exported") ---
	//
	// A person downloaded a Document as a file off-run (RFC GR §4.4): a bulk
	// read, so it is recorded. The document travels in TargetName as
	// `document:<id>`, its tenant in TargetTenant, the person in
	// ActorSubject. Never content.
	ExportFormat string `json:"export_format,omitempty"`
	ExportScope  string `json:"export_scope,omitempty"`
	ExportBytes  int64  `json:"export_bytes,omitempty"`

	// --- Run inbox put (Action = "run_inbox_put") ---
	//
	// A run's owner (or an admin) put a file into the run's inbox (RFC HE
	// F1). The caller travels in the Actor fields, the run's tenant in
	// TargetTenant. Never the file's name, which is often personal data, and
	// never its content.
	InboxRunID     string `json:"inbox_run_id,omitempty"`
	InboxBytes     int64  `json:"inbox_bytes,omitempty"`
	InboxMediaType string `json:"inbox_media_type,omitempty"`
	InboxSHA256    string `json:"inbox_sha256,omitempty"`

	// --- Crypto (Action = "crypto_op" | "crypto_key_generate" |
	// "crypto_key_create" | "crypto_key_rotate" | "crypto_key_min_version" |
	// "crypto_key_delete_version" | "crypto_key_delete" | "crypto_key_export" |
	// "crypto_key_import") ---
	//
	// Who used or changed which key, and how much passed through: never data,
	// never key material. The key's name travels in TargetName, its tenant in
	// TargetTenant, and the acting user in ActorSubject.
	CryptoOp      string `json:"crypto_op,omitempty"`     // encrypt, decrypt, sign, ...
	CryptoSource  string `json:"crypto_source,omitempty"` // key | cred | run
	CryptoScope   string `json:"crypto_scope,omitempty"`  // agent | user | tenant | run
	CryptoKeyType string `json:"crypto_key_type,omitempty"`
	CryptoVersion int    `json:"crypto_version,omitempty"`
	CryptoBytesIn int    `json:"crypto_bytes_in,omitempty"`
	CryptoOutcome string `json:"crypto_outcome,omitempty"` // ok | refused | failed
	CryptoRunID   string `json:"crypto_run_id,omitempty"`
	CryptoAgent   string `json:"crypto_agent,omitempty"`

	// --- OAuth credential (RFC GB; Action = "oauth_create" |
	// "oauth_connect_start" | "oauth_connected" | "oauth_connect_failed" |
	// "oauth_needs_reauth" | "oauth_reregistered" | "oauth_disconnect") ---
	//
	// Who connected or disconnected which OAuth credential, and what came of
	// a connect or a refresh: never a token, a device or user code, or a
	// client secret (no field holds one). The credential's name travels in
	// TargetName, its tenant in TargetTenant, the user it belongs to (user
	// scope) in TargetSubject, and the caller in the Actor fields.
	OAuthScope   string `json:"oauth_scope,omitempty"`   // tenant | user | agent
	OAuthIssuer  string `json:"oauth_issuer,omitempty"`  // the authorization server
	OAuthOutcome string `json:"oauth_outcome,omitempty"` // ok | refused | failed
	// OAuthReason is why a connect failed or a grant was lost: a fixed
	// phrase or the authorization server's error code.
	OAuthReason string `json:"oauth_reason,omitempty"`

	// --- Git network operation (Action = "git_clone" | "git_fetch" |
	// "git_pull" | "git_push" | "git_ls_remote") ---
	//
	// Which run reached which repository, with which credential, and what it
	// changed: the credential's reference ("$cred:<name>"), never its value.
	GitHost       string   `json:"git_host,omitempty"`
	GitRepository string   `json:"git_repository,omitempty"` // host + path
	GitRunID      string   `json:"git_run_id,omitempty"`
	GitAgent      string   `json:"git_agent,omitempty"`
	GitCredential string   `json:"git_credential,omitempty"`
	GitRefs       []string `json:"git_refs,omitempty"`    // refs updated
	GitOutcome    string   `json:"git_outcome,omitempty"` // ok | a failure class

	// --- Sign-in (Action = "auth_login" | "auth_login_failed" | "auth_logout" |
	// "auth_session_expired" | "auth_session_revoked" | "auth_downgrade_refused") ---
	//
	// Who signed in through which gate, from where, and why a session ended:
	// never a credential, a session id or anything derived from one. The person
	// travels in ActorTenant / ActorSubject, the client in SourceAddr.
	AuthGate   string   `json:"auth_gate,omitempty"`
	AuthAMR    []string `json:"auth_amr,omitempty"`
	AuthReason string   `json:"auth_reason,omitempty"`
	// AuthClient is the app (RFC FL OAuth client) a sign-in, token or
	// ClientDef change is about; AuthRedirectURIs a ClientDef's redirect URIs.
	AuthClient       string   `json:"auth_client,omitempty"`
	AuthRedirectURIs []string `json:"auth_redirect_uris,omitempty"`

	// --- Password gate management (Action = "auth_invite_created" |
	// "auth_invite_revoked" | "auth_invite_redeemed" | "auth_reset_issued" |
	// "auth_password_set" | "auth_credential_removed" | "auth_login_renamed") ---
	//
	// The gate travels in AuthGate above; this adds the invite or reset row
	// id. Never a code, a link, a password or a TOTP secret. The user travels
	// in TargetTenant / TargetSubject, the caller in the Actor fields.
	AuthRecordID string `json:"auth_record_id,omitempty"`
	// AuthRecovery marks an auth:manage action on a user above the
	// provisioning floor that the gate's manager_may_recover_owners allowed
	// (a reset, or attaching a first password); AuthTargetAccessMode is that
	// user's access mode. Both are what an auditor of a website's credential
	// looks for.
	// An "auth_manage_refused" names the refused operation in AuthReason.
	AuthRecovery bool `json:"auth_recovery,omitempty"`
	// AuthGroupsAdded / AuthGroupsRemoved are a gate's group sync (RFC FA
	// §3.7b): the group names a login added the user to or removed it from.
	AuthGroupsAdded      []string `json:"auth_groups_added,omitempty"`
	AuthGroupsRemoved    []string `json:"auth_groups_removed,omitempty"`
	AuthTargetAccessMode string   `json:"auth_target_access_mode,omitempty"`

	// --- Statement review (Action = "statement_ruling" |
	// "statement_ruling_revert" | "statement_policy_ruling") ---
	//
	// statement_policy_ruling is the review policy applied over the queue:
	// RulingAction "policy", RulingCounts carrying the ontology version and
	// matched / skipped / ruled / failed / deferred / marked.
	//
	// One record per bulk ruling and per revert: who ruled, on what selection,
	// with what reason and outcome. The statements themselves carry
	// ruled_by bulk:<RulingID>. The tenant travels in TargetTenant.
	RulingID     string `json:"ruling_id,omitempty"`
	RulingAction string `json:"ruling_action,omitempty"` // accept | reject | policy
	RulingScope  string `json:"ruling_scope,omitempty"`  // tenant | user
	// RulingScopeID is whose user scope (scope user): the subject whose
	// documents were ruled on, which an operator may name.
	RulingScopeID string `json:"ruling_scope_id,omitempty"`
	// RulingFilter is the queue filter a ruling selected by; RulingIDs the
	// statement ids it named instead.
	RulingFilter json.RawMessage `json:"ruling_filter,omitempty"`
	RulingIDs    []string        `json:"ruling_ids,omitempty"`
	RulingReason string          `json:"ruling_reason,omitempty"`
	// RulingCounts is matched / skipped / ruled / failed for a ruling, and
	// reverted / changed / skipped for a revert.
	RulingCounts map[string]int `json:"ruling_counts,omitempty"`

	// --- Document move (Action = "document_move") ---
	//
	// One record per move_documents call that wrote: which documents went
	// from which scope to which, and the counts. Ids only, never content. The
	// caller travels in the Actor fields, the tenant in TargetTenant.
	MoveFromScope   string         `json:"move_from_scope,omitempty"`
	MoveFromScopeID string         `json:"move_from_scope_id,omitempty"`
	MoveToScope     string         `json:"move_to_scope,omitempty"`
	MoveToScopeID   string         `json:"move_to_scope_id,omitempty"`
	MoveDocumentIDs []string       `json:"move_document_ids,omitempty"`
	MoveCounts      map[string]int `json:"move_counts,omitempty"`

	// --- Ontology change applied (Action = "ontology_change_applied") ---
	//
	// An operator accepted a change or retire proposal (RFC GQ P4.2): the
	// change was applied to the entry it names and the proposal deleted, so
	// this record and the entry's revision history are what remains of it.
	// The entry's name travels in TargetName, its tenant in TargetTenant, the
	// operator in the Actor fields. Ids, names and counts only.
	OntologyProposalID string         `json:"ontology_proposal_id,omitempty"`
	OntologyEntryID    string         `json:"ontology_entry_id,omitempty"`
	OntologyOp         string         `json:"ontology_op,omitempty"`      // edit | retire
	OntologyApplied    []string       `json:"ontology_applied,omitempty"` // retire, rename, move, edit
	OntologyPlanRisk   string         `json:"ontology_plan_risk,omitempty"`
	OntologyPlanCounts map[string]int `json:"ontology_plan_counts,omitempty"`

	// --- Directory lookup (Action = "identity_lookup") ---
	//
	// A run, or a session on the MCP server, reading what the directory holds
	// about ANOTHER person or about a group: who asked, about whom, through
	// which op, and what came of it. The caller is told "not found" for a
	// refusal and for a miss alike; this record tells them apart. Never any
	// of the data returned. The person asked about travels in TargetSubject,
	// a group in TargetName, the tenant in TargetTenant.
	IdentityOp      string `json:"identity_op,omitempty"`
	IdentityOutcome string `json:"identity_outcome,omitempty"` // ok | refused | not_found
	IdentityRunID   string `json:"identity_run_id,omitempty"`
	IdentityAgent   string `json:"identity_agent,omitempty"`

	// --- Workspace sharing (RFC GW P8; Action = "workspace_share" |
	// "workspace_unshare" | "workspace_share_use") ---
	//
	// A share or an unshare an owner made, and every tool call a peer made
	// through a share: the workspace, both runs and the access. The
	// workspace's name travels in TargetName as `workspace:<name>`, its
	// tenant in TargetTenant, the caller (the owner, or for a use the peer)
	// in the Actor fields. Ids and names only, never a file's content.
	WorkspaceID           string `json:"workspace_id,omitempty"`
	WorkspaceOwnerRunID   string `json:"workspace_owner_run_id,omitempty"`
	WorkspaceOwnerAgentID string `json:"workspace_owner_agent_id,omitempty"`
	WorkspacePeerRunID    string `json:"workspace_peer_run_id,omitempty"`
	WorkspacePeerAgentID  string `json:"workspace_peer_agent_id,omitempty"`
	WorkspaceAccess       string `json:"workspace_access,omitempty"` // read | read_write
	// WorkspaceOp is a use's operation and tool, e.g. "read via File".
	WorkspaceOp string `json:"workspace_op,omitempty"`

	// --- Workspace templates (RFC GW P6; Action = "workspace_template_save"
	// | "workspace_template_delete") ---
	//
	// A template saved from a checkpoint, or deleted: its name in TargetName
	// (`workspace_template:<name>`), its tenant in TargetTenant, the run in
	// WorkspaceOwnerRunID/AgentID, for a save the workspace it was made from
	// in WorkspaceID. Never a file's content.
	WorkspaceTemplateID     string `json:"workspace_template_id,omitempty"`
	WorkspaceTemplateScope  string `json:"workspace_template_scope,omitempty"`
	WorkspaceTemplateDigest string `json:"workspace_template_digest,omitempty"`
	WorkspaceTemplateBytes  int64  `json:"workspace_template_bytes,omitempty"`

	// --- Workspace egress (RFC GW P4; Action = "workspace_egress") ---
	//
	// One request a workspace's session sent out through a credentialed or
	// tenant-written route (RFC GW §6.3): the route, the method, the path
	// under the route without its query, the status (the upstream's, or the
	// refusal's), the body bytes that passed both ways, and a refusal's
	// reason. The workspace travels in WorkspaceID, its owner run in
	// WorkspaceOwnerRunID, its tenant in TargetTenant. Never a query, a
	// header, a body or a credential.
	EgressRoute   string `json:"egress_route,omitempty"`
	EgressMethod  string `json:"egress_method,omitempty"`
	EgressPath    string `json:"egress_path,omitempty"`
	EgressStatus  int    `json:"egress_status,omitempty"`
	EgressBytes   int64  `json:"egress_bytes,omitempty"`
	EgressRefused string `json:"egress_refused,omitempty"`

	// --- Dropped observations (Action = "audit_events_dropped") ---
	//
	// The store-backed stream queues observation events (refusals, denials,
	// lookups); when its queue is full it drops them rather than block the
	// caller, and writes this summary — action → events dropped — to the
	// operator chain once it can.
	DroppedCounts map[string]int `json:"dropped_counts,omitempty"`
	// DroppedSources is who sent the most of the dropped events — the
	// client address, else tenant/subject — with their counts, at most a
	// few, so a gap in the chain can be traced to its cause.
	DroppedSources map[string]int `json:"dropped_sources,omitempty"`

	// --- Chain checkpoint (Action = "audit_chain_checkpoint") ---
	//
	// The operator chain's record of a tenant chain's signed head (the tenant
	// travels in TargetTenant): its position, its hash and the head's MAC.
	// Verification refuses a tenant chain whose head is behind its newest
	// checkpoint, or whose row at that position has another hash, so a
	// rollback to an earlier signed head is caught.
	CheckpointSeq     int64  `json:"checkpoint_seq,omitempty"`
	CheckpointHash    string `json:"checkpoint_hash,omitempty"`
	CheckpointHeadMAC string `json:"checkpoint_head_mac,omitempty"`

	// --- Write-ahead status (state changes recorded before they commit) ---
	//
	// ChangeStatus is "intent" on a row written BEFORE the change it records
	// is made, "failed" on the best-effort row that follows when the change
	// then did not happen, and "partial" when it stopped part-way after
	// applying some of it (ChangeCounts says what). An intent row with no
	// later row is a change that was made, or one interrupted by a crash;
	// empty on rows written after the fact.
	ChangeStatus string `json:"change_status,omitempty"`
	// ChangeCounts is, on a "partial" row, what was applied before the
	// change stopped: kind → rows (a snapshot restore's users, lists, …).
	ChangeCounts map[string]int `json:"change_counts,omitempty"`
	// UserStatus is the status a user change sets (active | disabled), on
	// the user lifecycle records.
	UserStatus string `json:"user_status,omitempty"`
}

// ChangeStatus values.
const (
	ChangeIntent  = "intent"
	ChangeFailed  = "failed"
	ChangePartial = "partial"
)

// ChangeKind is how a change recorded ahead (ChangeIntent) behaves when its
// record cannot be written.
type ChangeKind int

const (
	// ChangeGrants: the change grants or widens access, or may (an
	// access-control write can drop a deny). It is refused when its record
	// cannot be written.
	ChangeGrants ChangeKind = iota
	// ChangeReduces: the change only takes access away. It goes ahead when
	// its record cannot be written, so an operator can still revoke a
	// compromised token or deprovision a user while the audit store is down;
	// the record is retried after the change and the miss is counted
	// (loomcycle_audit_unrecorded_total) and logged at error.
	ChangeReduces
)

// changeKinds classifies every action recorded ahead. An access-control
// write is never classed as reducing: a list rewrite, a retired list or
// binding, a deleted group or a removed membership can each remove a deny,
// so none is known to take access away.
// A function, not a package variable, so nothing can change the table.
func changeKinds() map[string]ChangeKind {
	return map[string]ChangeKind{
		// Tokens.
		"create": ChangeGrants,
		"rotate": ChangeGrants,
		"retire": ChangeReduces,
		// Users.
		"user_created":               ChangeGrants,
		"auth_user_approved":         ChangeGrants,
		"auth_login_renamed":         ChangeGrants,
		"user_updated":               ChangeGrants, // re-enable, access mode, display name
		"user_disabled":              ChangeReduces,
		"user_deleted":               ChangeReduces,
		"auth_scim_user_created":     ChangeGrants,
		"auth_scim_user_reactivated": ChangeGrants,
		"auth_scim_user_updated":     ChangeGrants,
		"auth_scim_user_deactivated": ChangeReduces,
		// A snapshot restore (users, groups, bindings and lists wholesale).
		"snapshot_restored": ChangeGrants,
		// Access control.
		"acl_written":          ChangeGrants,
		"acl_retired":          ChangeGrants,
		"binding_written":      ChangeGrants,
		"binding_retired":      ChangeGrants,
		"group_written":        ChangeGrants,
		"group_deleted":        ChangeGrants,
		"group_member_added":   ChangeGrants,
		"group_member_removed": ChangeGrants,
	}
}

// ChangeKindOf is action's kind, and whether it is classified at all. An
// unclassified action is to be treated as granting (refused unrecorded).
func ChangeKindOf(action string) (ChangeKind, bool) {
	k, ok := changeKinds()[action]
	return k, ok
}

// ChangeActions lists the classified actions, for the census tests.
func ChangeActions() []string {
	kinds := changeKinds()
	out := make([]string, 0, len(kinds))
	for a := range kinds {
		out = append(out, a)
	}
	return out
}

// Sink records audit events. Record must be safe for concurrent use. Most
// callers log a Record error and proceed; token, access-control and user
// lifecycle changes record an intent before they are made, and one that
// grants access is refused when it cannot be written (ChangeKindOf).
type Sink interface {
	Record(ev Event) error
}

// NopSink discards events. Used when no audit path is configured.
type NopSink struct{}

func (NopSink) Record(Event) error { return nil }

// FileSink appends JSONL to a path. One process-wide mutex serialises
// writes so concurrent token ops don't interleave partial lines.
type FileSink struct {
	mu   sync.Mutex
	path string
	now  func() time.Time // injectable for tests; defaults to time.Now
}

// NewFileSink opens (creating if needed) the audit file for append. It
// does not hold the file open — each Record reopens in O_APPEND so an
// external logrotate that renames the file is picked up on the next
// write without a SIGHUP.
func NewFileSink(path string) (*FileSink, error) {
	if path == "" {
		return nil, fmt.Errorf("audit: empty path")
	}
	// Probe writability up front so a misconfigured path fails at boot,
	// not on the first token op.
	// #nosec G304 -- the audit log path is operator config
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %q: %w", path, err)
	}
	_ = f.Close()
	return &FileSink{path: path, now: time.Now}, nil
}

func (s *FileSink) Record(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = s.now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open %q: %w", s.path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}
