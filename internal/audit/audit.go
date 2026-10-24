// Package audit is the RFC L OSS append-only audit sink. Every
// OperatorTokenDef mutation (create/rotate/retire) writes one JSONL line
// recording WHO did WHAT to WHICH principal — never any token plaintext
// or hash (the Event struct deliberately has no field for them). The OSS
// sink is a per-replica local file; operators wire their own pipeline
// (logrotate/fluentd/Loki). Tamper-evidence + unified queryable audit
// are enterprise features (RFC M).
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
	AuthRecovery         bool   `json:"auth_recovery,omitempty"`
	AuthTargetAccessMode string `json:"auth_target_access_mode,omitempty"`
}

// Sink records audit events. Record must be safe for concurrent use and
// must never return an error path that blocks the caller's primary
// operation — audit is best-effort observability, not a transaction
// participant; callers log a Record error and proceed.
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
