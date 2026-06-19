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
