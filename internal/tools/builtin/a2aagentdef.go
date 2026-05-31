package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A2AAgentDef is the v1.x RFC G built-in tool that lets agents author,
// fork, retire, and inspect REMOTE A2A peer definitions at runtime.
// Yaml `a2a_agents.<name>:` entries remain the operator-blessed root;
// this tool produces the DERIVED layer of orchestrator-authored forks.
//
// Five operations dispatched off the `op` field — mirrors ScheduleDef
// exactly minus the schedule-only add_hook/remove_hook ops:
//
//	create  — declare a brand-new peer name with a v1 definition.
//	          Refused if `name` matches a static cfg.A2AAgents entry.
//	fork    — make a new version from an existing parent.
//	get     — fetch one row by def_id.
//	list    — list versions for a name (version DESC).
//	retire  — flip the retired flag. Lineage stays visible.
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity). The model NEVER supplies these.
type A2AAgentDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Cfg is the loaded operator config. Used to resolve the
	// operator-blessed root (cfg.A2AAgents[name]) for the
	// static-name-replace refusal and the bootstrap-from-yaml path.
	Cfg *config.Config

	// MaxDefinitionBytes caps the serialised definition JSON. 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field. 0 = no cap.
	MaxDescriptionBytes int
}

const a2aAgentDefDescription = `Author, fork, retire, and inspect remote A2A peer definitions at runtime. ` +
	`Static a2a_agents.<name>: yaml entries remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of orchestrator-authored forks. ` +
	`Operations: create, fork, get, list, retire.`

const a2aAgentDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Remote-peer name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, or bootstraps from a yaml template)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of the remote-peer definition for create/fork. Server-set fields are silently ignored if supplied.",
      "additionalProperties": true
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":       {"type": "boolean", "description": "create + fork both default true (new versions replace old). Pass false to leave the existing active pointer in place."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."}
  },
  "required": ["op"]
}`

type a2aAgentDefInput struct {
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
func (s *A2AAgentDef) Name() string { return "A2AAgentDef" }

// Description implements tools.Tool.
func (s *A2AAgentDef) Description() string { return a2aAgentDefDescription }

// InputSchema implements tools.Tool.
func (s *A2AAgentDef) InputSchema() json.RawMessage {
	return json.RawMessage(a2aAgentDefInputSchema)
}

// Execute implements tools.Tool.
func (s *A2AAgentDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("A2AAgentDef tool: not configured (no Store backend)"), nil
	}
	if s.Cfg == nil {
		return errResult("A2AAgentDef tool: not configured (no Config — operator-blessed root unavailable)"), nil
	}
	var in a2aAgentDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.A2AAgentDefPolicy(ctx)

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

func (s *A2AAgentDef) execCreate(ctx context.Context, policy tools.A2AAgentDefPolicyValue, in a2aAgentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if _, ok := s.Cfg.A2AAgents[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static cfg.A2AAgents entry — use `fork` to derive a new version", in.Name)), nil
	}

	def, err := s.buildDefinition(in.Name, "", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := validateA2AAgentDef(def); err != nil {
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
	row := store.A2AAgentDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
	}
	created, err := s.Store.A2AAgentDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.A2AAgentDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(a2aAgentRowResponse(created, promote))
}

// ---- fork ----

func (s *A2AAgentDef) execFork(ctx context.Context, policy tools.A2AAgentDefPolicyValue, in a2aAgentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// Resolve the parent. Three paths (mirror ScheduleDef):
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + active pointer exists → use it
	//   3. neither → name must have a yaml template; bootstrap v1
	parentDefID := in.ParentDefID
	var parent store.A2AAgentDefRow
	if parentDefID != "" {
		row, err := s.Store.A2AAgentDefGet(ctx, parentDefID)
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
		row, err := s.Store.A2AAgentDefGetActive(ctx, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No active pointer → must bootstrap from yaml.
			static, ok := s.Cfg.A2AAgents[in.Name]
			if !ok {
				return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version nor a static cfg.A2AAgents entry", in.Name)), nil
			}
			bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
			if berr != nil {
				// Concurrent first-fork may have already bootstrapped v1;
				// re-read active pointer before propagating.
				if row2, gerr := s.Store.A2AAgentDefGetActive(ctx, in.Name); gerr == nil {
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
	if err := validateA2AAgentDef(def); err != nil {
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
	row := store.A2AAgentDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
	}
	created, err := s.Store.A2AAgentDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.A2AAgentDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(a2aAgentRowResponse(created, promote))
}

// ---- get / list ----

func (s *A2AAgentDef) execGet(ctx context.Context, policy tools.A2AAgentDefPolicyValue, in a2aAgentDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.A2AAgentDefGet(ctx, in.DefID)
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
	return okJSON(a2aAgentRowResponse(row, false))
}

func (s *A2AAgentDef) execList(ctx context.Context, policy tools.A2AAgentDefPolicyValue, in a2aAgentDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	rows, err := s.Store.A2AAgentDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, a2aAgentRowResponseMap(r))
	}
	return okJSON(map[string]any{"name": in.Name, "versions": out})
}

// ---- retire ----

func (s *A2AAgentDef) execRetire(ctx context.Context, policy tools.A2AAgentDefPolicyValue, in a2aAgentDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := s.Store.A2AAgentDefGet(ctx, in.DefID)
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
	if err := s.Store.A2AAgentDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

// ---- helpers ----

func (s *A2AAgentDef) checkScopeForName(policy tools.A2AAgentDefPolicyValue, name string) error {
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("A2AAgentDef tool: agent has no a2a_agent_def_scopes (default-deny); add `a2a_agent_def_scopes: [...]` to the agent yaml")
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
	return fmt.Errorf("A2AAgentDef tool: name %q not in this agent's a2a_agent_def_scopes (%v)", name, policy.Scopes)
}

func (s *A2AAgentDef) buildDefinition(name, parentJSON string, overlay json.RawMessage) (mergedA2AAgentDef, error) {
	base := mergedA2AAgentDef{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return mergedA2AAgentDef{}, fmt.Errorf("parse parent definition: %w", err)
		}
	} else if static, ok := s.Cfg.A2AAgents[name]; ok {
		// Create-with-static-name is REFUSED in execCreate; this branch
		// handles fork's bootstrap-from-static when no parent JSON yet
		// but a static entry exists.
		base = staticToMergedA2AAgentDef(static)
	}

	if len(overlay) > 0 {
		var ov mergedA2AAgentDef
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mergedA2AAgentDef{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	return base, nil
}

func (s *A2AAgentDef) bootstrapStatic(ctx context.Context, name string, static config.A2AAgent) (store.A2AAgentDefRow, error) {
	def := staticToMergedA2AAgentDef(static)
	defJSON, err := json.Marshal(def)
	if err != nil {
		return store.A2AAgentDefRow{}, fmt.Errorf("marshal: %w", err)
	}
	ident := tools.RunIdentity(ctx)
	row := store.A2AAgentDefRow{
		DefID:                  mintDefID(),
		Name:                   name,
		Definition:             defJSON,
		Description:            "bootstrapped from static cfg.A2AAgents",
		CreatedByAgentID:       ident.AgentID,
		BootstrappedFromStatic: true,
	}
	created, err := s.Store.A2AAgentDefCreate(ctx, row)
	if err != nil {
		return store.A2AAgentDefRow{}, err
	}
	if err := s.Store.A2AAgentDefSetActive(ctx, name, created.DefID, ident.AgentID); err != nil {
		// Bootstrap succeeded but couldn't promote — return the row;
		// the next fork iteration finds it via the active-pointer retry.
		return created, fmt.Errorf("promote bootstrap: %w", err)
	}
	return created, nil
}

// a2aBindings is the closed set of allowed direct-endpoint bindings.
var a2aBindings = map[string]bool{
	"jsonrpc": true,
	"grpc":    true,
	"rest":    true,
}

// a2aCredentialRefRe matches the credential-key charset for
// bearer_credential_ref ([a-zA-Z0-9_-]{1,64}).
var a2aCredentialRefRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// validateA2AAgentDef enforces the runtime-supplied overlay shape.
// EXACTLY ONE of agent_card_url OR (endpoint+binding) must be set.
func validateA2AAgentDef(def mergedA2AAgentDef) error {
	hasCardURL := def.AgentCardURL != ""
	hasDirect := def.Endpoint != "" || def.Binding != ""
	if hasCardURL && hasDirect {
		return fmt.Errorf("set exactly one of agent_card_url OR (endpoint+binding), not both")
	}
	if !hasCardURL && !hasDirect {
		return fmt.Errorf("set exactly one of agent_card_url OR (endpoint+binding); neither was provided")
	}
	if hasDirect {
		if def.Endpoint == "" {
			return fmt.Errorf("endpoint required when binding is set")
		}
		if def.Binding == "" {
			return fmt.Errorf("binding required when endpoint is set")
		}
	}
	if def.Binding != "" && !a2aBindings[def.Binding] {
		return fmt.Errorf("unknown binding %q (must be one of: jsonrpc, grpc, rest)", def.Binding)
	}
	// Reachability targets can be model-authored (a fork overlay carries
	// agent_card_url / endpoint), so reject non-HTTP schemes and hostless
	// URLs upfront. This is defense-in-depth: the HTTP fetch + jsonrpc/rest
	// transports also dial through the SSRF-blocking client in
	// internal/tools/a2a, which refuses private/loopback/metadata targets
	// at connect time. (The gRPC binding dials via grpc-go, outside that
	// client — so a grpc endpoint is validated for shape here but its
	// dial is NOT private-IP-blocked; gRPC peers are operator-configured in
	// practice and a callable fork must already be in allowed_tools.)
	if hasCardURL {
		if err := requireHTTPURL("agent_card_url", def.AgentCardURL); err != nil {
			return err
		}
	}
	if def.Endpoint != "" && (def.Binding == "rest" || def.Binding == "jsonrpc") {
		if err := requireHTTPURL("endpoint", def.Endpoint); err != nil {
			return err
		}
	}
	if def.Auth.Scheme != "" && !a2aSecuritySchemeKinds[def.Auth.Scheme] {
		return fmt.Errorf("auth.scheme %q invalid (must be one of: http, apiKey, oauth2, mtls)", def.Auth.Scheme)
	}
	if def.Auth.BearerCredentialRef != "" && !a2aCredentialRefRe.MatchString(def.Auth.BearerCredentialRef) {
		return fmt.Errorf("auth.bearer_credential_ref %q invalid (must match [a-zA-Z0-9_-]{1,64})", def.Auth.BearerCredentialRef)
	}
	return nil
}

// requireHTTPURL rejects a peer reachability URL that is not an absolute
// http(s) URL with a host — closing junk and non-HTTP schemes (file://,
// gopher://, …) a model-authored Def might carry before they reach the SDK.
func requireHTTPURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid URL: %v", field, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must be an http or https URL (got scheme %q)", field, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q has no host", field, raw)
	}
	return nil
}

// ---- response shape ----

func a2aAgentRowResponse(row store.A2AAgentDefRow, promoted bool) map[string]any {
	m := a2aAgentRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func a2aAgentRowResponseMap(row store.A2AAgentDefRow) map[string]any {
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

// ---- mergedA2AAgentDef: the JSON-tagged persistence shape ----
//
// Same conceptual fields as config.A2AAgent but with JSON tags
// (snake_case) for the substrate-write path. The
// lookup.SubstrateA2AAgentDef adapter mirrors this exactly for
// read-side round-trip; a drift test pins parity.
type mergedA2AAgentDef struct {
	AgentCardURL     string                   `json:"agent_card_url,omitempty"`
	Endpoint         string                   `json:"endpoint,omitempty"`
	Binding          string                   `json:"binding,omitempty"`
	Auth             mergedA2AAgentAuth       `json:"auth,omitempty"`
	ExpectedSkills   []mergedA2AExpectedSkill `json:"expected_skills,omitempty"`
	VerifySignedCard bool                     `json:"verify_signed_card,omitempty"`
}

// mergedA2AAgentAuth mirrors config.A2AAgentAuth.
type mergedA2AAgentAuth struct {
	Scheme              string `json:"scheme,omitempty"`
	BearerCredentialRef string `json:"bearer_credential_ref,omitempty"`
}

// mergedA2AExpectedSkill mirrors config.A2AExpectedSkill.
type mergedA2AExpectedSkill struct {
	ID       string `json:"id,omitempty"`
	Required bool   `json:"required,omitempty"`
}

func (d *mergedA2AAgentDef) applyOverlay(ov mergedA2AAgentDef) {
	// agent_card_url and endpoint+binding are mutually exclusive (the
	// validator enforces this). When an overlay supplies EXACTLY ONE
	// reachability mode, clear the OTHER so a fork can flip a peer from
	// one mode to the other without leaving a stale field that would
	// trip the both-set validation refusal. When an overlay supplies
	// BOTH (a caller mistake), we deliberately do NOT clear either —
	// the merged def then carries both and the validator rejects it
	// loudly, rather than silently dropping one mode.
	overlayHasCardURL := ov.AgentCardURL != ""
	overlayHasDirect := ov.Endpoint != "" || ov.Binding != ""
	if overlayHasCardURL {
		d.AgentCardURL = ov.AgentCardURL
		if !overlayHasDirect {
			d.Endpoint = ""
			d.Binding = ""
		}
	}
	if overlayHasDirect {
		if ov.Endpoint != "" {
			d.Endpoint = ov.Endpoint
		}
		if ov.Binding != "" {
			d.Binding = ov.Binding
		}
		if !overlayHasCardURL {
			d.AgentCardURL = ""
		}
	}
	if ov.Auth.Scheme != "" {
		d.Auth.Scheme = ov.Auth.Scheme
	}
	if ov.Auth.BearerCredentialRef != "" {
		d.Auth.BearerCredentialRef = ov.Auth.BearerCredentialRef
	}
	if ov.ExpectedSkills != nil {
		d.ExpectedSkills = ov.ExpectedSkills
	}
	// VerifySignedCard is a plain bool: an overlay can only flip it true.
	// The model toggles it off by restating the full def via a parent
	// fork rather than relying on overlay zero-value semantics, matching
	// the server-card capabilities-block posture.
	if ov.VerifySignedCard {
		d.VerifySignedCard = true
	}
}

func staticToMergedA2AAgentDef(a config.A2AAgent) mergedA2AAgentDef {
	out := mergedA2AAgentDef{
		AgentCardURL: a.AgentCardURL,
		Endpoint:     a.Endpoint,
		Binding:      a.Binding,
		Auth: mergedA2AAgentAuth{
			Scheme:              a.Auth.Scheme,
			BearerCredentialRef: a.Auth.BearerCredentialRef,
		},
		VerifySignedCard: a.VerifySignedCard,
	}
	if len(a.ExpectedSkills) > 0 {
		out.ExpectedSkills = make([]mergedA2AExpectedSkill, len(a.ExpectedSkills))
		for i, sk := range a.ExpectedSkills {
			out.ExpectedSkills[i] = mergedA2AExpectedSkill{ID: sk.ID, Required: sk.Required}
		}
	}
	return out
}
