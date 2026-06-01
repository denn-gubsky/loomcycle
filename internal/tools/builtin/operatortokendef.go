package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// OperatorTokenDef is the RFC L built-in that mints, rotates, retires,
// and inspects the bearer tokens that replace the single
// LOOMCYCLE_AUTH_TOKEN shared secret. Each token binds an AUTHORITATIVE
// PRINCIPAL (tenant_id + subject + allowed_scopes) that the auth
// middleware (PR2) stamps into ctx.
//
// This is an OPERATOR-ADMIN capability — it is reached only via the
// admin transports (CLI / POST /v1/_operatortokendef / gRPC / MCP), all
// of which set OperatorTokenDefPolicy{Admin:true} after clearing the
// substrate:admin scope check. Agents do NOT get this tool in their
// allowed_tools, and the default policy is deny.
//
// NOT a versioned/forkable substrate Def: a secret has no "content hash
// to fork", so there is no fork op, no version, no active pointer —
// rotation (not promotion) is the lifecycle.
type OperatorTokenDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Pepper is prepended to the token before SHA-256 (auth.HashToken).
	// Sourced from the env-allowlisted LOOMCYCLE_OPERATOR_TOKEN_PEPPER.
	// May be empty in single-binary dev.
	Pepper string

	// Audit records create/rotate/retire. Never nil at runtime (main.go
	// wires audit.NopSink when no path is configured).
	Audit audit.Sink

	// RotationGraceSeconds is the default window during which a rotated
	// token still authenticates. 0 → 24h.
	RotationGraceSeconds int

	// MaxScopes caps the allowed_scopes list length (defense-in-depth).
	// 0 → 64.
	MaxScopes int
}

var operatorTokenNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

const operatorTokenDefDescription = `Mint, rotate, retire, and inspect operator auth tokens (RFC L). ` +
	`Each token binds a principal {tenant_id, subject, allowed_scopes}. ` +
	`OPERATOR-ADMIN only. Operations: create, rotate, retire, get, list. ` +
	`The token plaintext is shown ONCE on create/rotate and never again.`

const operatorTokenDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["create","rotate","retire","get","list"], "description": "Operation to perform."},
    "name":       {"type": "string", "description": "Token name (required for create/list; create/rotate/retire accept name or def_id)."},
    "def_id":     {"type": "string", "description": "Existing def_id (for get; alternative target for rotate/retire)."},
    "tenant_id":  {"type": "string", "description": "Authoritative tenant (required for create)."},
    "subject":    {"type": "string", "description": "Authoritative subject (optional for create; defaults to tok:<name>)."},
    "scopes":     {"type": "array", "items": {"type": "string"}, "description": "Allowed scopes from the closed catalog (create; default [substrate:admin])."},
    "grace_seconds": {"type": "integer", "description": "Rotation grace window override (rotate)."}
  },
  "required": ["op"]
}`

type operatorTokenDefInput struct {
	Op           string   `json:"op"`
	Name         string   `json:"name,omitempty"`
	DefID        string   `json:"def_id,omitempty"`
	TenantID     string   `json:"tenant_id,omitempty"`
	Subject      string   `json:"subject,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	GraceSeconds *int     `json:"grace_seconds,omitempty"`
}

// Name implements tools.Tool.
func (s *OperatorTokenDef) Name() string { return "OperatorTokenDef" }

// Description implements tools.Tool.
func (s *OperatorTokenDef) Description() string { return operatorTokenDefDescription }

// InputSchema implements tools.Tool.
func (s *OperatorTokenDef) InputSchema() json.RawMessage {
	return json.RawMessage(operatorTokenDefInputSchema)
}

// Execute implements tools.Tool.
func (s *OperatorTokenDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("OperatorTokenDef tool: not configured (no Store backend)"), nil
	}
	if !tools.OperatorTokenDefPolicy(ctx).Admin {
		// Default-deny: only the admin transports grant this.
		return errResult("OperatorTokenDef tool: requires operator-admin (substrate:admin)"), nil
	}
	var in operatorTokenDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	switch in.Op {
	case "create":
		return s.execCreate(ctx, in)
	case "rotate":
		return s.execRotate(ctx, in)
	case "retire":
		return s.execRetire(ctx, in)
	case "get":
		return s.execGet(ctx, in)
	case "list":
		return s.execList(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, rotate, retire, get, list)", in.Op)), nil
	}
}

func (s *OperatorTokenDef) grace() time.Duration {
	if s.RotationGraceSeconds > 0 {
		return time.Duration(s.RotationGraceSeconds) * time.Second
	}
	return 24 * time.Hour
}

func (s *OperatorTokenDef) maxScopes() int {
	if s.MaxScopes > 0 {
		return s.MaxScopes
	}
	return 64
}

// ---- create ----

func (s *OperatorTokenDef) execCreate(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if !operatorTokenNameRe.MatchString(in.Name) {
		return errResult(fmt.Sprintf("create: name %q invalid (must match [a-zA-Z0-9_.-]{1,64})", in.Name)), nil
	}
	if in.TenantID == "" {
		return errResult("create: missing required field: tenant_id"), nil
	}
	if !operatorTokenNameRe.MatchString(in.TenantID) {
		return errResult(fmt.Sprintf("create: tenant_id %q invalid (must match [a-zA-Z0-9_.-]{1,64})", in.TenantID)), nil
	}
	subject := in.Subject
	if subject == "" {
		// Decision 4: default to a stable per-token id so attribution /
		// fairness / audit are still distinct even without an explicit
		// subject.
		subject = "tok:" + in.Name
	} else if !operatorTokenNameRe.MatchString(subject) {
		return errResult(fmt.Sprintf("create: subject %q invalid (must match [a-zA-Z0-9_.-]{1,64})", subject)), nil
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = []string{auth.ScopeAdmin} // preserve "single token, full power"
	}
	if len(scopes) > s.maxScopes() {
		return errResult(fmt.Sprintf("create: too many scopes (%d > %d)", len(scopes), s.maxScopes())), nil
	}
	if bad := auth.UnknownScopes(scopes); bad != nil {
		return errResult(fmt.Sprintf("create: unknown scope(s) %v — not in the closed catalog", bad)), nil
	}

	// A name has at most one current (non-retired) token; force `rotate`
	// for an existing live name so a create can't silently shadow it.
	if _, err := s.Store.OperatorTokenDefGetCurrentByName(ctx, in.Name); err == nil {
		return errResult(fmt.Sprintf("create: name %q already has a live token — use `rotate` to replace it", in.Name)), nil
	} else {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return errResult(fmt.Sprintf("create: %s", err)), nil
		}
	}

	plaintext, suffix, err := auth.MintToken()
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	ident := tools.RunIdentity(ctx)
	row := store.OperatorTokenDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		TenantID:         in.TenantID,
		Subject:          subject,
		TokenHash:        auth.HashToken(s.Pepper, plaintext),
		AllowedScopes:    scopes,
		CreatedByAgentID: ident.AgentID,
	}
	created, err := s.Store.OperatorTokenDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	s.audit(ctx, audit.Event{
		Action: "create", TargetDefID: created.DefID, TargetName: created.Name,
		TargetTenant: created.TenantID, TargetSubject: created.Subject, ScopesAfter: scopes,
	})
	return okJSON(operatorTokenCreateResponse(created, plaintext, suffix))
}

// ---- rotate ----

func (s *OperatorTokenDef) execRotate(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	prior, err := s.resolveTarget(ctx, "rotate", in)
	if err != nil {
		return errResult(err.Error()), nil
	}
	plaintext, suffix, mErr := auth.MintToken()
	if mErr != nil {
		return errResult(fmt.Sprintf("rotate: %s", mErr)), nil
	}
	ident := tools.RunIdentity(ctx)
	row := store.OperatorTokenDefRow{
		DefID:            mintDefID(),
		Name:             prior.Name,
		TenantID:         prior.TenantID,
		Subject:          prior.Subject,
		TokenHash:        auth.HashToken(s.Pepper, plaintext),
		AllowedScopes:    prior.AllowedScopes,
		CreatedByAgentID: ident.AgentID,
		RotatedFrom:      prior.DefID,
	}
	created, err := s.Store.OperatorTokenDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("rotate: %s", err)), nil
	}
	grace := s.grace()
	if in.GraceSeconds != nil && *in.GraceSeconds >= 0 {
		grace = time.Duration(*in.GraceSeconds) * time.Second
	}
	retireOldAt := time.Now().Add(grace).UTC()
	if err := s.Store.OperatorTokenDefSetRetiredAt(ctx, prior.DefID, retireOldAt); err != nil {
		return errResult(fmt.Sprintf("rotate: retire prior: %s", err)), nil
	}
	s.audit(ctx, audit.Event{
		Action: "rotate", TargetDefID: created.DefID, TargetName: created.Name,
		TargetTenant: created.TenantID, TargetSubject: created.Subject,
		ScopesBefore: prior.AllowedScopes, ScopesAfter: created.AllowedScopes,
	})
	resp := operatorTokenCreateResponse(created, plaintext, suffix)
	resp["rotated_from"] = prior.DefID
	resp["prior_retires_at"] = retireOldAt.Format(time.RFC3339Nano)
	return okJSON(resp)
}

// ---- retire ----

func (s *OperatorTokenDef) execRetire(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	target, err := s.resolveTarget(ctx, "retire", in)
	if err != nil {
		return errResult(err.Error()), nil
	}
	now := time.Now().UTC()
	if err := s.Store.OperatorTokenDefSetRetiredAt(ctx, target.DefID, now); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	s.audit(ctx, audit.Event{
		Action: "retire", TargetDefID: target.DefID, TargetName: target.Name,
		TargetTenant: target.TenantID, TargetSubject: target.Subject, ScopesBefore: target.AllowedScopes,
	})
	return okJSON(map[string]any{"def_id": target.DefID, "name": target.Name, "retired_at": now.Format(time.RFC3339Nano)})
}

// ---- get / list ----

func (s *OperatorTokenDef) execGet(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.OperatorTokenDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	return okJSON(operatorTokenRowResponse(row))
}

func (s *OperatorTokenDef) execList(ctx context.Context, in operatorTokenDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	rows, err := s.Store.OperatorTokenDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, operatorTokenRowResponse(r))
	}
	return okJSON(map[string]any{"name": in.Name, "tokens": out})
}

// ---- helpers ----

// resolveTarget finds the row a rotate/retire targets, by def_id or by
// name (the name's current non-retired token).
func (s *OperatorTokenDef) resolveTarget(ctx context.Context, op string, in operatorTokenDefInput) (store.OperatorTokenDefRow, error) {
	if in.DefID != "" {
		row, err := s.Store.OperatorTokenDefGet(ctx, in.DefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return store.OperatorTokenDefRow{}, fmt.Errorf("%s: def_id %q not found", op, in.DefID)
			}
			return store.OperatorTokenDefRow{}, fmt.Errorf("%s: %s", op, err)
		}
		return row, nil
	}
	if in.Name != "" {
		row, err := s.Store.OperatorTokenDefGetCurrentByName(ctx, in.Name)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return store.OperatorTokenDefRow{}, fmt.Errorf("%s: no live token for name %q", op, in.Name)
			}
			return store.OperatorTokenDefRow{}, fmt.Errorf("%s: %s", op, err)
		}
		return row, nil
	}
	return store.OperatorTokenDefRow{}, fmt.Errorf("%s: need name or def_id", op)
}

func (s *OperatorTokenDef) audit(ctx context.Context, ev audit.Event) {
	if s.Audit == nil {
		return
	}
	ident := tools.RunIdentity(ctx)
	ev.ActorSubject = ident.UserID
	// ActorTenant is wired in PR2 once RunIdentity carries an
	// authoritative TenantID (the identity-threading deliverable).
	_ = s.Audit.Record(ev) // best-effort; never blocks the op
}

// operatorTokenRowResponse renders a row WITHOUT any secret material
// (token plaintext is never stored; token_hash is json:"-").
func operatorTokenRowResponse(row store.OperatorTokenDefRow) map[string]any {
	m := map[string]any{
		"def_id":              row.DefID,
		"name":                row.Name,
		"tenant_id":           row.TenantID,
		"subject":             row.Subject,
		"allowed_scopes":      row.AllowedScopes,
		"created_at":          row.CreatedAt.UTC().Format(time.RFC3339Nano),
		"created_by_agent_id": row.CreatedByAgentID,
		"retired":             !row.RetiredAt.IsZero(),
	}
	if row.RotatedFrom != "" {
		m["rotated_from"] = row.RotatedFrom
	}
	if !row.RetiredAt.IsZero() {
		m["retired_at"] = row.RetiredAt.UTC().Format(time.RFC3339Nano)
	}
	return m
}

// operatorTokenCreateResponse adds the show-once plaintext + suffix to
// the row response. The ONLY place the plaintext ever crosses the wire.
func operatorTokenCreateResponse(row store.OperatorTokenDefRow, plaintext, suffix string) map[string]any {
	m := operatorTokenRowResponse(row)
	m["token"] = plaintext
	m["token_suffix"] = suffix
	m["warning"] = "store this token now — it is shown once and cannot be retrieved later"
	return m
}
