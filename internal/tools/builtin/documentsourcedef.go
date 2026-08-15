package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// DocumentSourceDef is the RFC CE built-in tool that lets agents author,
// fork, retire, and inspect named DOCUMENT SOURCE definitions at runtime.
// Yaml `document_sources.<name>:` entries remain the operator-blessed root;
// this tool produces the DERIVED layer of orchestrator-authored forks.
//
// Faithful mirror of MemoryBackendDef. Five operations dispatched off the
// `op` field:
//
//	create  — declare a brand-new source name with a v1 definition.
//	          Refused if `name` matches a static cfg.DocumentSources entry.
//	fork    — make a new version from an existing parent.
//	get     — fetch one row by def_id.
//	list    — list versions for a name (version DESC).
//	retire  — flip the retired flag. Lineage stays visible.
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity). The model NEVER supplies these.
type DocumentSourceDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Cfg is the loaded operator config. Used to resolve the
	// operator-blessed root (cfg.DocumentSources[name]) for the
	// static-name-replace refusal and the bootstrap-from-yaml path.
	Cfg *config.Config

	// MaxDefinitionBytes caps the serialised definition JSON. 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field. 0 = no cap.
	MaxDescriptionBytes int
}

const documentSourceDefDescription = `Author, fork, retire, and inspect named document source definitions at runtime. ` +
	`Static document_sources.<name>: yaml entries remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of orchestrator-authored forks. ` +
	`Operations: create, fork, get, list, retire.`

const documentSourceDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Source name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, or bootstraps from a yaml template)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of the document source definition for create/fork. Server-set fields are silently ignored if supplied.",
      "additionalProperties": true
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":       {"type": "boolean", "description": "create + fork both default true (new versions replace old). Pass false to leave the existing active pointer in place."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."}
  },
  "required": ["op"]
}`

type documentSourceDefInput struct {
	Op          string          `json:"op"`
	Name        string          `json:"name,omitempty"`
	DefID       string          `json:"def_id,omitempty"`
	ParentDefID string          `json:"parent_def_id,omitempty"`
	Overlay     json.RawMessage `json:"overlay,omitempty"`
	Description string          `json:"description,omitempty"`
	Promote     *bool           `json:"promote,omitempty"`
	Retired     *bool           `json:"retired,omitempty"`
}

// Name implements tools.Tool.
func (s *DocumentSourceDef) Name() string { return "DocumentSourceDef" }

// Description implements tools.Tool.
func (s *DocumentSourceDef) Description() string { return documentSourceDefDescription }

// InputSchema implements tools.Tool.
func (s *DocumentSourceDef) InputSchema() json.RawMessage {
	return json.RawMessage(documentSourceDefInputSchema)
}

// Execute implements tools.Tool.
func (s *DocumentSourceDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("DocumentSourceDef tool: not configured (no Store backend)"), nil
	}
	if s.Cfg == nil {
		return errResult("DocumentSourceDef tool: not configured (no Config — operator-blessed root unavailable)"), nil
	}
	var in documentSourceDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.DocumentSourceDefPolicy(ctx)

	switch in.Op {
	case "create":
		return s.execCreate(ctx, policy, in)
	case "fork":
		return s.execFork(ctx, policy, in)
	case "get":
		return s.execGet(ctx, policy, in)
	case "list":
		return s.execList(ctx, policy, in)
	case "retire":
		return s.execRetire(ctx, policy, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, fork, get, list, retire)", in.Op)), nil
	}
}

// ---- create ----

func (s *DocumentSourceDef) execCreate(ctx context.Context, policy tools.DocumentSourceDefPolicyValue, in documentSourceDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if _, ok := s.Cfg.DocumentSources[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static cfg.DocumentSources entry — use `fork` to derive a new version", in.Name)), nil
	}

	def, err := s.buildDefinition(in.Name, "", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := validateDocumentSourceDef(def); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("create: marshal: %s", err)), nil
	}
	if s.MaxDefinitionBytes > 0 && len(defJSON) > s.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("create: definition (%d bytes) exceeds max %d", len(defJSON), s.MaxDefinitionBytes)), nil
	}
	if s.MaxDescriptionBytes > 0 && len(in.Description) > s.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("create: description (%d bytes) exceeds max %d", len(in.Description), s.MaxDescriptionBytes)), nil
	}

	// RFC N: stamp the def under the caller's authoritative tenant (run
	// identity, never tool input). "" = the shared/operator/legacy tenant.
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID
	row := store.DocumentSourceDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		TenantID:         tenantID,
	}
	created, err := s.Store.DocumentSourceDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.DocumentSourceDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(documentSourceRowResponse(created, promote))
}

// ---- fork ----

func (s *DocumentSourceDef) execFork(ctx context.Context, policy tools.DocumentSourceDefPolicyValue, in documentSourceDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// RFC N: fork resolves the parent within the caller's own tenant first
	// (then the shared "" base), but the new version is ALWAYS stamped under
	// the caller's own tenant (authoritative run identity, never tool input).
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID

	// Resolve the parent. Paths (mirror MemoryBackendDef):
	//   1. parent_def_id supplied → pin (refuse another tenant's private def)
	//   2. parent_def_id empty + own-tenant active pointer → use it
	//   3. else shared "" active pointer (for non-"" tenants) → use it
	//   4. neither → name must have a yaml template; bootstrap v1
	parentDefID := in.ParentDefID
	var parent store.DocumentSourceDefRow
	if parentDefID != "" {
		row, err := s.Store.DocumentSourceDefGet(ctx, parentDefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: parent_def_id %q not found", parentDefID)), nil
			}
			return errResult(fmt.Sprintf("fork: %s", err)), nil
		}
		if row.Name != in.Name {
			return errResult(fmt.Sprintf("fork: parent_def_id %q has name %q, refusing to fork under name %q", parentDefID, row.Name, in.Name)), nil
		}
		// A def_id is a global handle. Allow forking the SHARED ("") base or
		// the caller's OWN tenant's def; refuse forking ANOTHER tenant's
		// private def (would copy its body across the boundary) unless the
		// caller is a substrate:admin (crosses tenants by design).
		if row.TenantID != "" && row.TenantID != tenantID && !defCallerIsAdmin(ctx) {
			return errResult(fmt.Sprintf("fork: parent_def_id %q belongs to another tenant, refusing", parentDefID)), nil
		}
		parent = row
	} else {
		row, err := s.Store.DocumentSourceDefGetActive(ctx, tenantID, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No own-tenant active pointer. Fall back to the SHARED ("") base
			// (same precedence lookup.DocumentSource walks); the fork still
			// lands under the caller's tenant. Skip when tenantID is already "".
			if tenantID != "" {
				if shared, serr := s.Store.DocumentSourceDefGetActive(ctx, "", in.Name); serr == nil {
					parent = shared
					parentDefID = shared.DefID
				} else if !errors.As(serr, &nf) {
					return errResult(fmt.Sprintf("fork: %s", serr)), nil
				}
			}
			// Still no parent → bootstrap from yaml, else refuse.
			if parentDefID == "" {
				static, ok := s.Cfg.DocumentSources[in.Name]
				if !ok {
					return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version (own tenant or shared \"\") nor a static cfg.DocumentSources entry", in.Name)), nil
				}
				bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
				if berr != nil {
					// Concurrent first-fork may have already bootstrapped v1;
					// re-read own-tenant active pointer before propagating.
					if row2, gerr := s.Store.DocumentSourceDefGetActive(ctx, tenantID, in.Name); gerr == nil {
						parent = row2
						parentDefID = row2.DefID
					} else {
						return errResult(fmt.Sprintf("fork: bootstrap static: %s", berr)), nil
					}
				} else {
					parent = bootstrap
					parentDefID = bootstrap.DefID
				}
			}
		}
	}

	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}

	def, err := s.buildDefinition(in.Name, string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := validateDocumentSourceDef(def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	defJSON, err := json.Marshal(def)
	if err != nil {
		return errResult(fmt.Sprintf("fork: marshal: %s", err)), nil
	}
	if s.MaxDefinitionBytes > 0 && len(defJSON) > s.MaxDefinitionBytes {
		return errResult(fmt.Sprintf("fork: definition (%d bytes) exceeds max %d", len(defJSON), s.MaxDefinitionBytes)), nil
	}
	if s.MaxDescriptionBytes > 0 && len(in.Description) > s.MaxDescriptionBytes {
		return errResult(fmt.Sprintf("fork: description (%d bytes) exceeds max %d", len(in.Description), s.MaxDescriptionBytes)), nil
	}

	row := store.DocumentSourceDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		TenantID:         tenantID,
	}
	created, err := s.Store.DocumentSourceDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.DocumentSourceDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(documentSourceRowResponse(created, promote))
}

// ---- get / list ----

func (s *DocumentSourceDef) execGet(ctx context.Context, policy tools.DocumentSourceDefPolicyValue, in documentSourceDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.DocumentSourceDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	// RFC N: def_id is a global handle but a def is owned by exactly one
	// tenant. Refuse cross-tenant reads with the SAME opaque not-found a
	// missing def returns — never leak existence/body of another tenant's row.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
	}
	if err := s.checkScopeForName(policy, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	return okJSON(documentSourceRowResponse(row, false))
}

func (s *DocumentSourceDef) execList(ctx context.Context, policy tools.DocumentSourceDefPolicyValue, in documentSourceDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	rows, err := s.Store.DocumentSourceDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	// RFC N: ListByName returns rows across ALL tenants for a name (names
	// are per-tenant now). Filter to the caller's own tenant so a tenant
	// lists only its own versions; a substrate:admin sees all.
	tenantID := tools.RunIdentity(ctx).TenantID
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if !defCallerIsAdmin(ctx) && r.TenantID != tenantID {
			continue
		}
		out = append(out, documentSourceRowResponseMap(r))
	}
	return okJSON(map[string]any{"name": in.Name, "versions": out})
}

// ---- retire ----

func (s *DocumentSourceDef) execRetire(ctx context.Context, policy tools.DocumentSourceDefPolicyValue, in documentSourceDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := s.Store.DocumentSourceDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	// RFC N: refuse cross-tenant retire (global by-def_id mutation). Opaque
	// not-found — don't leak existence of another tenant's def.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
	}
	if err := s.checkScopeForName(policy, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if err := s.Store.DocumentSourceDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

// ---- helpers ----

func (s *DocumentSourceDef) checkScopeForName(policy tools.DocumentSourceDefPolicyValue, name string) error {
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("DocumentSourceDef tool: no def-scope granted in this caller's context (default-deny)")
	}
	for _, sc := range policy.Scopes {
		switch sc {
		case "any":
			return nil
		case "self":
			if name == policy.SelfName {
				return nil
			}
		case "descendants":
			// Same KNOWN GAP as MemoryBackendDef's "descendants" — accept on
			// presence; tighten when RunIdentity gains the parent lineage
			// walk surface.
			return nil
		default:
			if strings.HasPrefix(sc, "named:") {
				if strings.TrimPrefix(sc, "named:") == name {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("DocumentSourceDef tool: name %q not in the caller's granted def-scope (%v)", name, policy.Scopes)
}

func (s *DocumentSourceDef) buildDefinition(name, parentJSON string, overlay json.RawMessage) (mergedDocumentSourceDef, error) {
	base := mergedDocumentSourceDef{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return mergedDocumentSourceDef{}, fmt.Errorf("parse parent definition: %w", err)
		}
	} else if static, ok := s.Cfg.DocumentSources[name]; ok {
		// Create-with-static-name is REFUSED in execCreate; this branch
		// handles fork's bootstrap-from-static when no parent JSON yet
		// but a static entry exists.
		base = staticToMergedDocumentSourceDef(static)
	}

	if len(overlay) > 0 {
		var ov mergedDocumentSourceDef
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mergedDocumentSourceDef{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	// The definition's name is the registry key (the `name` arg), not an
	// overlay-controlled field: a DocumentSourceDef is addressed by name, so
	// the stored shape MUST equal the key callers select it by. Stamping it
	// here (after the overlay) both removes the need to duplicate `name` into
	// every overlay and prevents an overlay from declaring a name that
	// diverges from the row it is stored under. Mirrors MemoryBackendDef's
	// name-stamp posture.
	base.Name = name
	return base, nil
}

func (s *DocumentSourceDef) bootstrapStatic(ctx context.Context, name string, static config.DocumentSource) (store.DocumentSourceDefRow, error) {
	def := staticToMergedDocumentSourceDef(static)
	def.Name = name
	defJSON, err := json.Marshal(def)
	if err != nil {
		return store.DocumentSourceDefRow{}, fmt.Errorf("marshal: %w", err)
	}
	ident := tools.RunIdentity(ctx)
	row := store.DocumentSourceDefRow{
		DefID:                  mintDefID(),
		Name:                   name,
		Definition:             defJSON,
		Description:            "bootstrapped from static cfg.DocumentSources",
		CreatedByAgentID:       ident.AgentID,
		BootstrappedFromStatic: true,
		// RFC N: the bootstrapped lineage root lives in the forking caller's
		// tenant (static cfg is the shared base; the fork that triggers
		// bootstrap is per-tenant). "" = shared.
		TenantID: ident.TenantID,
	}
	created, err := s.Store.DocumentSourceDefCreate(ctx, row)
	if err != nil {
		return store.DocumentSourceDefRow{}, err
	}
	if err := s.Store.DocumentSourceDefSetActive(ctx, ident.TenantID, name, created.DefID, ident.AgentID); err != nil {
		// Bootstrap succeeded but couldn't promote — return the row;
		// the next fork iteration finds it via the active-pointer retry.
		return created, fmt.Errorf("promote bootstrap: %w", err)
	}
	return created, nil
}

// validateDocumentSourceDef enforces the runtime-supplied overlay shape,
// mirroring the static `document_sources.<name>:` validation in
// config.Validate. A document source is dialed (base_url) and its
// api_key_env is SENT to that peer, so both are load-bearing at the door.
//
// Runs on create AND fork.
func validateDocumentSourceDef(def mergedDocumentSourceDef) error {
	// base_url is required and dialed. Unlike MemoryBackend's optional
	// inprocess base_url, a document source has no meaning without a peer.
	if def.Config.BaseURL == "" {
		return fmt.Errorf("config.base_url is required")
	}
	if err := requireHTTPURL("config.base_url", def.Config.BaseURL); err != nil {
		return err
	}
	// api_key_env is resolved and SENT to the def-supplied base_url, so an
	// unvalidated name is a one-request exfiltration vector: paired with a
	// base_url the author controls, `api_key_env: LOOMCYCLE_AUTH_TOKEN` would
	// ship the operator bearer to a peer. The persisted shape must never hold
	// an infra secret or a non-allowlisted var. Only checked when set.
	if def.Config.APIKeyEnv != "" && !config.EnvNameCredentialSafe(def.Config.APIKeyEnv) {
		return fmt.Errorf("config.api_key_env %q is not an allowed credential env var "+
			"(must be LOOMCYCLE_-prefixed or a known third-party key, and must not be one "+
			"of loomcycle's own infrastructure secrets)", def.Config.APIKeyEnv)
	}
	// tenancy_strategy.kind ∈ {"", "key_per_tenant"}. shared_key_with_prefix
	// is NOT supported (the document proxy has no key-prefix semantics).
	switch def.TenancyStrategy.Kind {
	case "":
		// no-op
	case "key_per_tenant":
		if def.TenancyStrategy.EnvPattern != "" && !strings.Contains(def.TenancyStrategy.EnvPattern, "{tenant_id}") {
			return fmt.Errorf("tenancy_strategy.env_pattern %q must contain {tenant_id}", def.TenancyStrategy.EnvPattern)
		}
	default:
		return fmt.Errorf("tenancy_strategy.kind %q must be \"\" or key_per_tenant", def.TenancyStrategy.Kind)
	}
	return nil
}

// ---- response shape ----

func documentSourceRowResponse(row store.DocumentSourceDefRow, promoted bool) map[string]any {
	m := documentSourceRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func documentSourceRowResponseMap(row store.DocumentSourceDefRow) map[string]any {
	return map[string]any{
		"def_id":                   row.DefID,
		"name":                     row.Name,
		"version":                  row.Version,
		"parent_def_id":            row.ParentDefID,
		"description":              row.Description,
		"created_at":               row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"created_by_agent_id":      row.CreatedByAgentID,
		"retired":                  row.Retired,
		"bootstrapped_from_static": row.BootstrappedFromStatic,
		"definition":               row.Definition,
	}
}

// ---- mergedDocumentSourceDef: the JSON-tagged persistence shape ----
//
// EXACT field set of config.DocumentSource plus a name field (stamped from
// the registry key like MemoryBackendDef; config.DocumentSource itself has no
// name field). The lookup.SubstrateDocumentSourceDef adapter mirrors this
// exactly for read-side round-trip; a drift test pins parity
// (TestMergedDocumentSourceDef_DriftDetection_VsLookupSubstrate).
type mergedDocumentSourceDef struct {
	Name            string                      `json:"name,omitempty"`
	Config          mergedDocumentSourceConfig  `json:"config,omitempty"`
	TenancyStrategy mergedDocumentSourceTenancy `json:"tenancy_strategy,omitempty"`
}

// mergedDocumentSourceConfig mirrors config.DocumentSourceConfig.
type mergedDocumentSourceConfig struct {
	BaseURL    string `json:"base_url,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
}

// mergedDocumentSourceTenancy mirrors config.DocumentSourceTenancy.
type mergedDocumentSourceTenancy struct {
	Kind       string `json:"kind,omitempty"`
	EnvPattern string `json:"env_pattern,omitempty"`
}

func (d *mergedDocumentSourceDef) applyOverlay(ov mergedDocumentSourceDef) {
	if ov.Config.BaseURL != "" {
		d.Config.BaseURL = ov.Config.BaseURL
	}
	if ov.Config.APIVersion != "" {
		d.Config.APIVersion = ov.Config.APIVersion
	}
	if ov.Config.APIKeyEnv != "" {
		d.Config.APIKeyEnv = ov.Config.APIKeyEnv
	}
	if ov.TenancyStrategy.Kind != "" {
		d.TenancyStrategy.Kind = ov.TenancyStrategy.Kind
	}
	if ov.TenancyStrategy.EnvPattern != "" {
		d.TenancyStrategy.EnvPattern = ov.TenancyStrategy.EnvPattern
	}
}

func staticToMergedDocumentSourceDef(m config.DocumentSource) mergedDocumentSourceDef {
	return mergedDocumentSourceDef{
		Config: mergedDocumentSourceConfig{
			BaseURL:    m.Config.BaseURL,
			APIVersion: m.Config.APIVersion,
			APIKeyEnv:  m.Config.APIKeyEnv,
		},
		TenancyStrategy: mergedDocumentSourceTenancy{
			Kind:       m.TenancyStrategy.Kind,
			EnvPattern: m.TenancyStrategy.EnvPattern,
		},
	}
}
