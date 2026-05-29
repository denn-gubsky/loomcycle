package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A2AServerCardDef is the v1.x RFC G built-in tool that lets agents
// author, fork, retire, and inspect A2A server-card definitions at
// runtime. Yaml `a2a_server_cards.<name>:` entries remain the
// operator-blessed root; this tool produces the DERIVED layer of
// orchestrator-authored forks.
//
// Five operations dispatched off the `op` field — mirrors ScheduleDef
// exactly minus the schedule-only add_hook/remove_hook ops (a server
// card has no on_complete list):
//
//	create  — declare a brand-new card name with a v1 definition.
//	          Refused if `name` matches a static cfg.A2AServerCards
//	          entry (use `fork` to derive a new version).
//	fork    — make a new version from an existing parent (by parent_def_id,
//	          or by-name from the active pointer / yaml template).
//	get     — fetch one row by def_id.
//	list    — list versions for a name (version DESC).
//	retire  — flip the retired flag. Lineage stays visible.
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity). The model NEVER supplies these.
type A2AServerCardDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Cfg is the loaded operator config. Used to resolve the
	// operator-blessed root (cfg.A2AServerCards[name]) for the
	// static-name-replace refusal and the bootstrap-from-yaml path.
	Cfg *config.Config

	// MaxDefinitionBytes caps the serialised definition JSON. 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field. 0 = no cap.
	MaxDescriptionBytes int
}

const a2aServerCardDefDescription = `Author, fork, retire, and inspect A2A server-card definitions at runtime. ` +
	`Static a2a_server_cards.<name>: yaml entries remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of orchestrator-authored forks. ` +
	`Operations: create, fork, get, list, retire.`

const a2aServerCardDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Server-card name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, or bootstraps from a yaml template)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of the server-card definition for create/fork. Server-set fields are silently ignored if supplied.",
      "additionalProperties": true
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":       {"type": "boolean", "description": "create + fork both default true (new versions replace old). Pass false to leave the existing active pointer in place."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."}
  },
  "required": ["op"]
}`

type a2aServerCardDefInput struct {
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
func (s *A2AServerCardDef) Name() string { return "A2AServerCardDef" }

// Description implements tools.Tool.
func (s *A2AServerCardDef) Description() string { return a2aServerCardDefDescription }

// InputSchema implements tools.Tool.
func (s *A2AServerCardDef) InputSchema() json.RawMessage {
	return json.RawMessage(a2aServerCardDefInputSchema)
}

// Execute implements tools.Tool.
func (s *A2AServerCardDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("A2AServerCardDef tool: not configured (no Store backend)"), nil
	}
	if s.Cfg == nil {
		return errResult("A2AServerCardDef tool: not configured (no Config — operator-blessed root unavailable)"), nil
	}
	var in a2aServerCardDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.A2AServerCardDefPolicy(ctx)

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

func (s *A2AServerCardDef) execCreate(ctx context.Context, policy tools.A2AServerCardDefPolicyValue, in a2aServerCardDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if _, ok := s.Cfg.A2AServerCards[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static cfg.A2AServerCards entry — use `fork` to derive a new version", in.Name)), nil
	}

	def, err := s.buildDefinition(in.Name, "", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := validateA2AServerCardDef(def); err != nil {
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

	ident := tools.RunIdentity(ctx)
	row := store.A2AServerCardDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
	}
	created, err := s.Store.A2AServerCardDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.A2AServerCardDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(a2aServerCardRowResponse(created, promote))
}

// ---- fork ----

func (s *A2AServerCardDef) execFork(ctx context.Context, policy tools.A2AServerCardDefPolicyValue, in a2aServerCardDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// Resolve the parent. Three paths (mirror ScheduleDef):
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + active pointer exists → use it
	//   3. neither → name must have a yaml template; bootstrap v1
	parentDefID := in.ParentDefID
	var parent store.A2AServerCardDefRow
	if parentDefID != "" {
		row, err := s.Store.A2AServerCardDefGet(ctx, parentDefID)
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
		parent = row
	} else {
		row, err := s.Store.A2AServerCardDefGetActive(ctx, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No active pointer → must bootstrap from yaml.
			static, ok := s.Cfg.A2AServerCards[in.Name]
			if !ok {
				return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version nor a static cfg.A2AServerCards entry", in.Name)), nil
			}
			bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
			if berr != nil {
				// Concurrent first-fork may have already bootstrapped v1;
				// re-read active pointer before propagating.
				if row2, gerr := s.Store.A2AServerCardDefGetActive(ctx, in.Name); gerr == nil {
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

	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}

	def, err := s.buildDefinition(in.Name, string(parent.Definition), in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := validateA2AServerCardDef(def); err != nil {
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

	ident := tools.RunIdentity(ctx)
	row := store.A2AServerCardDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
	}
	created, err := s.Store.A2AServerCardDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.A2AServerCardDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(a2aServerCardRowResponse(created, promote))
}

// ---- get / list ----

func (s *A2AServerCardDef) execGet(ctx context.Context, policy tools.A2AServerCardDefPolicyValue, in a2aServerCardDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.A2AServerCardDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("get: %s", err)), nil
	}
	if err := s.checkScopeForName(policy, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	return okJSON(a2aServerCardRowResponse(row, false))
}

func (s *A2AServerCardDef) execList(ctx context.Context, policy tools.A2AServerCardDefPolicyValue, in a2aServerCardDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	rows, err := s.Store.A2AServerCardDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, a2aServerCardRowResponseMap(r))
	}
	return okJSON(map[string]any{"name": in.Name, "versions": out})
}

// ---- retire ----

func (s *A2AServerCardDef) execRetire(ctx context.Context, policy tools.A2AServerCardDefPolicyValue, in a2aServerCardDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := s.Store.A2AServerCardDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	if err := s.checkScopeForName(policy, row.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if err := s.Store.A2AServerCardDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

// ---- helpers ----

func (s *A2AServerCardDef) checkScopeForName(policy tools.A2AServerCardDefPolicyValue, name string) error {
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("A2AServerCardDef tool: agent has no a2a_server_card_def_scopes (default-deny); add `a2a_server_card_def_scopes: [...]` to the agent yaml")
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
			// Same KNOWN GAP as ScheduleDef's "descendants" — accept on
			// presence; tighten when RunIdentity gains the parent
			// lineage walk surface.
			return nil
		default:
			if strings.HasPrefix(sc, "named:") {
				if strings.TrimPrefix(sc, "named:") == name {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("A2AServerCardDef tool: name %q not in this agent's a2a_server_card_def_scopes (%v)", name, policy.Scopes)
}

func (s *A2AServerCardDef) buildDefinition(name, parentJSON string, overlay json.RawMessage) (mergedA2AServerCardDef, error) {
	base := mergedA2AServerCardDef{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return mergedA2AServerCardDef{}, fmt.Errorf("parse parent definition: %w", err)
		}
	} else if static, ok := s.Cfg.A2AServerCards[name]; ok {
		// Create-with-static-name is REFUSED in execCreate; this branch
		// handles fork's bootstrap-from-static when no parent JSON yet
		// but a static entry exists.
		base = staticToMergedA2AServerCardDef(static)
	}

	if len(overlay) > 0 {
		var ov mergedA2AServerCardDef
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mergedA2AServerCardDef{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	return base, nil
}

func (s *A2AServerCardDef) bootstrapStatic(ctx context.Context, name string, static config.A2AServerCard) (store.A2AServerCardDefRow, error) {
	def := staticToMergedA2AServerCardDef(static)
	defJSON, err := json.Marshal(def)
	if err != nil {
		return store.A2AServerCardDefRow{}, fmt.Errorf("marshal: %w", err)
	}
	ident := tools.RunIdentity(ctx)
	row := store.A2AServerCardDefRow{
		DefID:                  mintDefID(),
		Name:                   name,
		Definition:             defJSON,
		Description:            "bootstrapped from static cfg.A2AServerCards",
		CreatedByAgentID:       ident.AgentID,
		BootstrappedFromStatic: true,
	}
	created, err := s.Store.A2AServerCardDefCreate(ctx, row)
	if err != nil {
		return store.A2AServerCardDefRow{}, err
	}
	if err := s.Store.A2AServerCardDefSetActive(ctx, name, created.DefID, ident.AgentID); err != nil {
		// Bootstrap succeeded but couldn't promote — return the row;
		// the next fork iteration finds it via the active-pointer retry.
		return created, fmt.Errorf("promote bootstrap: %w", err)
	}
	return created, nil
}

// envVarNameRe matches the env-var-name shape used for sign_with_key_env.
var envVarNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// a2aSecuritySchemeKinds is the closed set of allowed security-scheme
// kinds, shared with A2AAgentDef's auth.scheme validation.
var a2aSecuritySchemeKinds = map[string]bool{
	"http":   true,
	"apiKey": true,
	"oauth2": true,
	"mtls":   true,
}

// validateA2AServerCardDef enforces the runtime-supplied overlay shape.
// Validates STRUCTURE only — the env-allowlist check for
// sign_with_key_env is enforced at card-serving time (a later slice),
// not here.
func validateA2AServerCardDef(def mergedA2AServerCardDef) error {
	if def.Name == "" {
		return fmt.Errorf("name: required")
	}
	if len(def.ExposedAgents) == 0 {
		return fmt.Errorf("exposed_agents: required (must be non-empty)")
	}
	for i, e := range def.ExposedAgents {
		if e.AgentName == "" {
			return fmt.Errorf("exposed_agents[%d]: agent_name required", i)
		}
	}
	for i, sc := range def.SecuritySchemes {
		if !a2aSecuritySchemeKinds[sc.Kind] {
			return fmt.Errorf("security_schemes[%d]: unknown kind %q (must be one of: http, apiKey, oauth2, mtls)", i, sc.Kind)
		}
	}
	if def.SignWithKeyEnv != "" && !envVarNameRe.MatchString(def.SignWithKeyEnv) {
		return fmt.Errorf("sign_with_key_env %q is not a valid env-var name (must match [A-Z][A-Z0-9_]*)", def.SignWithKeyEnv)
	}
	return nil
}

// ---- response shape ----

func a2aServerCardRowResponse(row store.A2AServerCardDefRow, promoted bool) map[string]any {
	m := a2aServerCardRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func a2aServerCardRowResponseMap(row store.A2AServerCardDefRow) map[string]any {
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

// ---- mergedA2AServerCardDef: the JSON-tagged persistence shape ----
//
// Same conceptual fields as config.A2AServerCard but with JSON tags
// (snake_case) for the substrate-write path. The
// lookup.SubstrateA2AServerCardDef adapter mirrors this exactly for
// read-side round-trip; a drift test pins parity.
type mergedA2AServerCardDef struct {
	Name            string                    `json:"name,omitempty"`
	Description     string                    `json:"description,omitempty"`
	Provider        mergedA2AProvider         `json:"provider,omitempty"`
	Capabilities    mergedA2ACapabilities     `json:"capabilities,omitempty"`
	ExposedAgents   []mergedA2AExposedAgent   `json:"exposed_agents,omitempty"`
	SecuritySchemes []mergedA2ASecurityScheme `json:"security_schemes,omitempty"`
	SignWithKeyEnv  string                    `json:"sign_with_key_env,omitempty"`
}

// mergedA2AProvider mirrors config.A2AServerCardProvider.
type mergedA2AProvider struct {
	Organization string `json:"organization,omitempty"`
	URL          string `json:"url,omitempty"`
}

// mergedA2ACapabilities mirrors config.A2AServerCardCaps.
type mergedA2ACapabilities struct {
	Streaming         bool `json:"streaming,omitempty"`
	PushNotifications bool `json:"push_notifications,omitempty"`
	ExtendedAgentCard bool `json:"extended_agent_card,omitempty"`
}

// mergedA2AExposedAgent mirrors config.A2AExposedAgent.
type mergedA2AExposedAgent struct {
	AgentName   string   `json:"agent_name,omitempty"`
	SkillID     string   `json:"skill_id,omitempty"`
	SkillName   string   `json:"skill_name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	InputModes  []string `json:"input_modes,omitempty"`
	OutputModes []string `json:"output_modes,omitempty"`
}

// mergedA2ASecurityScheme mirrors config.A2ASecurityScheme.
type mergedA2ASecurityScheme struct {
	Kind   string `json:"kind,omitempty"`
	Scheme string `json:"scheme,omitempty"`
}

func (d *mergedA2AServerCardDef) applyOverlay(ov mergedA2AServerCardDef) {
	if ov.Name != "" {
		d.Name = ov.Name
	}
	if ov.Description != "" {
		d.Description = ov.Description
	}
	if ov.Provider.Organization != "" {
		d.Provider.Organization = ov.Provider.Organization
	}
	if ov.Provider.URL != "" {
		d.Provider.URL = ov.Provider.URL
	}
	// Capabilities is a bool triple; an overlay that touches the card's
	// capabilities replaces the block wholesale. There's no partial-merge
	// use case (the model either keeps the parent's caps by omitting the
	// key entirely, or restates the full set).
	if ov.Capabilities != (mergedA2ACapabilities{}) {
		d.Capabilities = ov.Capabilities
	}
	if ov.ExposedAgents != nil {
		d.ExposedAgents = ov.ExposedAgents
	}
	if ov.SecuritySchemes != nil {
		d.SecuritySchemes = ov.SecuritySchemes
	}
	if ov.SignWithKeyEnv != "" {
		d.SignWithKeyEnv = ov.SignWithKeyEnv
	}
}

func staticToMergedA2AServerCardDef(c config.A2AServerCard) mergedA2AServerCardDef {
	out := mergedA2AServerCardDef{
		Name:        c.Name,
		Description: c.Description,
		Provider: mergedA2AProvider{
			Organization: c.Provider.Organization,
			URL:          c.Provider.URL,
		},
		Capabilities: mergedA2ACapabilities{
			Streaming:         c.Capabilities.Streaming,
			PushNotifications: c.Capabilities.PushNotifications,
			ExtendedAgentCard: c.Capabilities.ExtendedAgentCard,
		},
		SignWithKeyEnv: c.SignWithKeyEnv,
	}
	if len(c.ExposedAgents) > 0 {
		out.ExposedAgents = make([]mergedA2AExposedAgent, len(c.ExposedAgents))
		for i, e := range c.ExposedAgents {
			out.ExposedAgents[i] = mergedA2AExposedAgent{
				AgentName:   e.AgentName,
				SkillID:     e.SkillID,
				SkillName:   e.SkillName,
				Description: e.Description,
				Tags:        e.Tags,
				InputModes:  e.InputModes,
				OutputModes: e.OutputModes,
			}
		}
	}
	if len(c.SecuritySchemes) > 0 {
		out.SecuritySchemes = make([]mergedA2ASecurityScheme, len(c.SecuritySchemes))
		for i, sc := range c.SecuritySchemes {
			out.SecuritySchemes[i] = mergedA2ASecurityScheme{Kind: sc.Kind, Scheme: sc.Scheme}
		}
	}
	return out
}
