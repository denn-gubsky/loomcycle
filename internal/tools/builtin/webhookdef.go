package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// WebhookDef is the v1.x RFC H (Input Webhooks) built-in tool that lets
// agents author, fork, retire, and inspect INBOUND webhook definitions
// at runtime. Yaml `webhooks.<name>:` entries remain the operator-blessed
// root; this tool produces the DERIVED layer of orchestrator-authored
// forks.
//
// RFC H WH-2 / mirrors A2AAgentDef exactly minus the schedule-only
// add_hook/remove_hook ops. Five operations dispatched off the `op`
// field:
//
//	create  — declare a brand-new webhook name with a v1 definition.
//	          Refused if `name` matches a static cfg.Webhooks entry.
//	fork    — make a new version from an existing parent.
//	get     — fetch one row by def_id.
//	list    — list versions for a name (version DESC).
//	retire  — flip the retired flag. Lineage stays visible.
//
// Server-stamped fields: created_at, created_by_agent_id (from
// tools.RunIdentity). The model NEVER supplies these.
type WebhookDef struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Cfg is the loaded operator config. Used to resolve the
	// operator-blessed root (cfg.Webhooks[name]) for the
	// static-name-replace refusal and the bootstrap-from-yaml path.
	Cfg *config.Config

	// MaxDefinitionBytes caps the serialised definition JSON. 0 = no cap.
	MaxDefinitionBytes int

	// MaxDescriptionBytes caps the description field. 0 = no cap.
	MaxDescriptionBytes int
}

const webhookDefDescription = `Author, fork, retire, and inspect inbound webhook definitions at runtime. ` +
	`Static webhooks.<name>: yaml entries remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of orchestrator-authored forks. ` +
	`Operations: create, fork, get, list, retire.`

const webhookDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Webhook name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, or bootstraps from a yaml template)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of the webhook definition for create/fork. Server-set fields are silently ignored if supplied.",
      "additionalProperties": true
    },
    "description":   {"type": "string", "description": "Free-text rationale for create/fork."},
    "promote":       {"type": "boolean", "description": "create + fork both default true (new versions replace old). Pass false to leave the existing active pointer in place."},
    "retired":       {"type": "boolean", "description": "Required for retire — set true to retire, false to un-retire."}
  },
  "required": ["op"]
}`

type webhookDefInput struct {
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
func (s *WebhookDef) Name() string { return "WebhookDef" }

// Description implements tools.Tool.
func (s *WebhookDef) Description() string { return webhookDefDescription }

// InputSchema implements tools.Tool.
func (s *WebhookDef) InputSchema() json.RawMessage {
	return json.RawMessage(webhookDefInputSchema)
}

// Execute implements tools.Tool.
func (s *WebhookDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if s.Store == nil {
		return errResult("WebhookDef tool: not configured (no Store backend)"), nil
	}
	if s.Cfg == nil {
		return errResult("WebhookDef tool: not configured (no Config — operator-blessed root unavailable)"), nil
	}
	var in webhookDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.WebhookDefPolicy(ctx)

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

func (s *WebhookDef) execCreate(ctx context.Context, policy tools.WebhookDefPolicyValue, in webhookDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("create: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	if _, ok := s.Cfg.Webhooks[in.Name]; ok {
		return errResult(fmt.Sprintf("create: name %q matches a static cfg.Webhooks entry — use `fork` to derive a new version", in.Name)), nil
	}

	def, err := s.buildDefinition(in.Name, "", in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := validateWebhookDef(def); err != nil {
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
	row := store.WebhookDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
	}
	created, err := s.Store.WebhookDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.WebhookDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	return okJSON(webhookRowResponse(created, promote))
}

// ---- fork ----

func (s *WebhookDef) execFork(ctx context.Context, policy tools.WebhookDefPolicyValue, in webhookDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// Resolve the parent. Three paths (mirror A2AAgentDef):
	//   1. parent_def_id supplied → pin
	//   2. parent_def_id empty + active pointer exists → use it
	//   3. neither → name must have a yaml template; bootstrap v1
	parentDefID := in.ParentDefID
	var parent store.WebhookDefRow
	if parentDefID != "" {
		row, err := s.Store.WebhookDefGet(ctx, parentDefID)
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
		row, err := s.Store.WebhookDefGetActive(ctx, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No active pointer → must bootstrap from yaml.
			static, ok := s.Cfg.Webhooks[in.Name]
			if !ok {
				return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version nor a static cfg.Webhooks entry", in.Name)), nil
			}
			bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
			if berr != nil {
				// Concurrent first-fork may have already bootstrapped v1;
				// re-read active pointer before propagating.
				if row2, gerr := s.Store.WebhookDefGetActive(ctx, in.Name); gerr == nil {
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
	if err := validateWebhookDef(def); err != nil {
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
	row := store.WebhookDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
	}
	created, err := s.Store.WebhookDefCreate(ctx, row)
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	promote := true
	if in.Promote != nil {
		promote = *in.Promote
	}
	if promote {
		if err := s.Store.WebhookDefSetActive(ctx, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	return okJSON(webhookRowResponse(created, promote))
}

// ---- get / list ----

func (s *WebhookDef) execGet(ctx context.Context, policy tools.WebhookDefPolicyValue, in webhookDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("get: missing required field: def_id"), nil
	}
	row, err := s.Store.WebhookDefGet(ctx, in.DefID)
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
	return okJSON(webhookRowResponse(row, false))
}

func (s *WebhookDef) execList(ctx context.Context, policy tools.WebhookDefPolicyValue, in webhookDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("list: missing required field: name"), nil
	}
	if err := s.checkScopeForName(policy, in.Name); err != nil {
		return errResult(err.Error()), nil
	}
	rows, err := s.Store.WebhookDefListByName(ctx, in.Name)
	if err != nil {
		return errResult(fmt.Sprintf("list: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, webhookRowResponseMap(r))
	}
	return okJSON(map[string]any{"name": in.Name, "versions": out})
}

// ---- retire ----

func (s *WebhookDef) execRetire(ctx context.Context, policy tools.WebhookDefPolicyValue, in webhookDefInput) (tools.Result, error) {
	if in.DefID == "" {
		return errResult("retire: missing required field: def_id"), nil
	}
	if in.Retired == nil {
		return errResult("retire: missing required field: retired (true|false)"), nil
	}
	row, err := s.Store.WebhookDefGet(ctx, in.DefID)
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
	if err := s.Store.WebhookDefSetRetired(ctx, in.DefID, *in.Retired); err != nil {
		return errResult(fmt.Sprintf("retire: %s", err)), nil
	}
	return okJSON(map[string]any{"def_id": in.DefID, "retired": *in.Retired})
}

// ---- helpers ----

func (s *WebhookDef) checkScopeForName(policy tools.WebhookDefPolicyValue, name string) error {
	if len(policy.Scopes) == 0 {
		return fmt.Errorf("WebhookDef tool: caller has no WebhookDef scope (default-deny) — WebhookDef is operator-admin-only; reach it via the bearer-authed admin endpoint, the LoomCycle MCP meta-tool, or the gRPC substrate path")
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
	return fmt.Errorf("WebhookDef tool: name %q not in the caller's WebhookDef scopes (%v)", name, policy.Scopes)
}

func (s *WebhookDef) buildDefinition(name, parentJSON string, overlay json.RawMessage) (mergedWebhookDef, error) {
	base := mergedWebhookDef{}
	if parentJSON != "" {
		if err := json.Unmarshal([]byte(parentJSON), &base); err != nil {
			return mergedWebhookDef{}, fmt.Errorf("parse parent definition: %w", err)
		}
	} else if static, ok := s.Cfg.Webhooks[name]; ok {
		// Create-with-static-name is REFUSED in execCreate; this branch
		// handles fork's bootstrap-from-static when no parent JSON yet
		// but a static entry exists.
		base = staticToMergedWebhookDef(static)
	}

	if len(overlay) > 0 {
		var ov mergedWebhookDef
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mergedWebhookDef{}, fmt.Errorf("parse overlay: %w", err)
		}
		base.applyOverlay(ov)
	}
	// Unlike A2AServerCardDef, config.Webhook carries no Name field — the
	// webhook's addressable name is the registry key (the `name` arg)
	// only, exactly like A2AAgentDef. So no name stamp here.
	return base, nil
}

func (s *WebhookDef) bootstrapStatic(ctx context.Context, name string, static config.Webhook) (store.WebhookDefRow, error) {
	def := staticToMergedWebhookDef(static)
	defJSON, err := json.Marshal(def)
	if err != nil {
		return store.WebhookDefRow{}, fmt.Errorf("marshal: %w", err)
	}
	ident := tools.RunIdentity(ctx)
	row := store.WebhookDefRow{
		DefID:                  mintDefID(),
		Name:                   name,
		Definition:             defJSON,
		Description:            "bootstrapped from static cfg.Webhooks",
		CreatedByAgentID:       ident.AgentID,
		BootstrappedFromStatic: true,
	}
	created, err := s.Store.WebhookDefCreate(ctx, row)
	if err != nil {
		return store.WebhookDefRow{}, err
	}
	if err := s.Store.WebhookDefSetActive(ctx, name, created.DefID, ident.AgentID); err != nil {
		// Bootstrap succeeded but couldn't promote — return the row;
		// the next fork iteration finds it via the active-pointer retry.
		return created, fmt.Errorf("promote bootstrap: %w", err)
	}
	return created, nil
}

// BootstrapStaticWebhooks materializes static yaml `webhooks:` into the
// webhook_defs substrate at boot — parity with agents/skills/schedules
// (F24), so a static webhook is listable/forkable via the WebhookDef tool
// without first being forked, and shows up in the Library like the other Defs.
// Idempotent + fork-respecting: a name that already has an active version (a
// prior bootstrap or a runtime fork) is left untouched. Returns the count of
// webhooks newly seeded. A minimal tool instance (Store + Cfg) suffices.
func (s *WebhookDef) BootstrapStaticWebhooks(ctx context.Context) (int, error) {
	if s.Store == nil || s.Cfg == nil {
		return 0, nil
	}
	names := make([]string, 0, len(s.Cfg.Webhooks))
	for name := range s.Cfg.Webhooks {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order for logs + tests
	seeded := 0
	for _, name := range names {
		_, err := s.Store.WebhookDefGetActive(ctx, name)
		if err == nil {
			continue // already has an active version — leave it
		}
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return seeded, fmt.Errorf("bootstrap %q: get active: %w", name, err)
		}
		if _, berr := s.bootstrapStatic(ctx, name, s.Cfg.Webhooks[name]); berr != nil {
			return seeded, fmt.Errorf("bootstrap %q: %w", name, berr)
		}
		seeded++
	}
	return seeded, nil
}

// validateWebhookDef enforces the runtime-supplied overlay shape.
// STRUCTURAL validation only — the env-allowlist RESOLVABILITY check for
// signing_secret_env / bearer_token_env / user_credentials_from_env is
// deferred to the WH-4 receiver/admin (this validator only checks the
// env-var-NAME charset, not whether the var is actually set + allowed).
//
// RFC H WH-2 / mirrors A2AServerCardDef's structure-only posture.
func validateWebhookDef(def mergedWebhookDef) error {
	// delivery ∈ {"", "spawn", "channel"} ("" treated as spawn).
	switch def.Delivery {
	case "", "spawn":
		if def.Agent == "" {
			return fmt.Errorf("delivery=spawn requires agent (non-empty)")
		}
		if def.Channel != "" {
			return fmt.Errorf("delivery=spawn forbids channel (set agent, not channel)")
		}
	case "channel":
		if def.Channel == "" {
			return fmt.Errorf("delivery=channel requires channel (non-empty)")
		}
		if def.Agent != "" {
			return fmt.Errorf("delivery=channel forbids agent (set channel, not agent)")
		}
		// RFC H Decision 11: channel mode forbids credentials. A channel
		// publish is not an autonomous run, so it has no run identity to
		// carry per-user MCP-tool bearers — silently accepting them would
		// be a credential leak into the message bus.
		if len(def.UserCredentialsFromEnv) > 0 {
			return fmt.Errorf("delivery=channel forbids user_credentials_from_env (RFC H Decision 11: channel mode has no run identity to carry credentials)")
		}
		for k := range def.PayloadMapping {
			if strings.HasPrefix(k, "user_credentials.") {
				return fmt.Errorf("delivery=channel forbids payload_mapping key %q (RFC H Decision 11: channel mode cannot map user credentials)", k)
			}
		}
	default:
		return fmt.Errorf("unknown delivery %q (must be one of: spawn, channel)", def.Delivery)
	}

	// auth.kind ∈ {"", "hmac", "bearer"} ("" treated as hmac).
	switch def.Auth.Kind {
	case "", "hmac":
		if def.Auth.SigningSecretEnv == "" {
			return fmt.Errorf("auth.kind=hmac requires auth.signing_secret_env")
		}
		if !envVarNameRe.MatchString(def.Auth.SigningSecretEnv) {
			return fmt.Errorf("auth.signing_secret_env %q is not a valid env-var name (must match [A-Z][A-Z0-9_]*)", def.Auth.SigningSecretEnv)
		}
		// SHA-256 is the only digest the receiver implements (verifyHMAC
		// hardcodes sha256.New). The Algorithm field is carried through
		// every Def site, so an unsupported value would be silently dropped
		// and the sender's valid signatures rejected with an opaque 401 —
		// the never-silently-degrade contract (RFC H Decision 9) demands a
		// loud refusal here instead. "" defaults to sha256.
		if a := strings.ToLower(strings.TrimSpace(def.Auth.Algorithm)); a != "" && a != "sha256" {
			return fmt.Errorf("auth.algorithm %q unsupported (only sha256 is implemented)", def.Auth.Algorithm)
		}
	case "bearer":
		if def.Auth.BearerTokenEnv == "" {
			return fmt.Errorf("auth.kind=bearer requires auth.bearer_token_env")
		}
		if !envVarNameRe.MatchString(def.Auth.BearerTokenEnv) {
			return fmt.Errorf("auth.bearer_token_env %q is not a valid env-var name (must match [A-Z][A-Z0-9_]*)", def.Auth.BearerTokenEnv)
		}
	case "none":
		// Trusted-network escape hatch: the receiver performs NO verification.
		// It is honored only when the operator sets
		// LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED=1; otherwise every delivery
		// 503s "unauthenticated_mode_disabled". Forbid declaring a secret that
		// would never be consumed — silent dead config is worse than a loud
		// refusal (RFC H Decision 9, never-silently-degrade).
		if def.Auth.SigningSecretEnv != "" || def.Auth.BearerTokenEnv != "" {
			return fmt.Errorf("auth.kind=none forbids signing_secret_env / bearer_token_env (no verification is performed)")
		}
	default:
		return fmt.Errorf("unknown auth.kind %q (must be one of: hmac, bearer, none)", def.Auth.Kind)
	}

	// Every value in user_credentials_from_env must be a valid env-var
	// name (WH-4 checks resolvability against the allowlist).
	for k, v := range def.UserCredentialsFromEnv {
		if !envVarNameRe.MatchString(v) {
			return fmt.Errorf("user_credentials_from_env[%q] = %q is not a valid env-var name (must match [A-Z][A-Z0-9_]*)", k, v)
		}
	}

	// Every payload_mapping value must be non-empty (JSONPath strings;
	// JSONPath SYNTAX validation is deferred to WH-4's payload.go).
	for k, v := range def.PayloadMapping {
		if v == "" {
			return fmt.Errorf("payload_mapping[%q] is empty (expected a JSONPath string)", k)
		}
	}

	if def.BodySizeLimitBytes < 0 {
		return fmt.Errorf("body_size_limit_bytes must be >= 0")
	}
	if def.RateLimit.RequestsPerMinute < 0 {
		return fmt.Errorf("rate_limit.requests_per_minute must be >= 0")
	}
	if def.RateLimit.Burst < 0 {
		return fmt.Errorf("rate_limit.burst must be >= 0")
	}
	if def.SyncResponse.Enabled {
		if def.SyncResponse.TimeoutMs <= 0 || def.SyncResponse.TimeoutMs > 60000 {
			return fmt.Errorf("sync_response.timeout_ms must be in (0, 60000] when sync_response.enabled")
		}
	}
	return nil
}

// ---- response shape ----

func webhookRowResponse(row store.WebhookDefRow, promoted bool) map[string]any {
	m := webhookRowResponseMap(row)
	m["promoted"] = promoted
	return m
}

func webhookRowResponseMap(row store.WebhookDefRow) map[string]any {
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

// ---- mergedWebhookDef: the JSON-tagged persistence shape ----
//
// EXACT field set of config.Webhook (WH-1) with the same snake_case JSON
// tags for the substrate-write path. The lookup.SubstrateWebhookDef
// adapter mirrors this exactly for read-side round-trip; a drift test
// pins parity (TestMergedWebhookDef_DriftDetection_VsLookupSubstrate).
//
// on_complete reuses config.ScheduledRunHook — the same hook shape
// ScheduleDef uses — rather than a parallel type, per RFC H WH-2.
type mergedWebhookDef struct {
	Enabled                bool                      `json:"enabled,omitempty"`
	Delivery               string                    `json:"delivery,omitempty"`
	Agent                  string                    `json:"agent,omitempty"`
	Channel                string                    `json:"channel,omitempty"`
	Auth                   mergedWebhookAuth         `json:"auth,omitempty"`
	RateLimit              mergedWebhookRateLimit    `json:"rate_limit,omitempty"`
	BodySizeLimitBytes     int                       `json:"body_size_limit_bytes,omitempty"`
	UserCredentialsFromEnv map[string]string         `json:"user_credentials_from_env,omitempty"`
	UserCredentials        map[string]string         `json:"user_credentials,omitempty"` // RFC F fork-time explicit values (ScheduleDef parity)
	Metadata               map[string]any            `json:"metadata,omitempty"`         // non-secret, trusted agent metadata
	TenantID               string                    `json:"tenant_id,omitempty"`        // tenant the spawned run executes as (RFC N follow-up); def-authored, never from payload
	PayloadMapping         map[string]string         `json:"payload_mapping,omitempty"`
	SyncResponse           mergedWebhookSyncResp     `json:"sync_response,omitempty"`
	OnComplete             []config.ScheduledRunHook `json:"on_complete,omitempty"`
}

// mergedWebhookAuth mirrors config.WebhookAuth.
type mergedWebhookAuth struct {
	Kind             string `json:"kind,omitempty"`
	Algorithm        string `json:"algorithm,omitempty"`
	Header           string `json:"header,omitempty"`
	SigningSecretEnv string `json:"signing_secret_env,omitempty"`
	DeliveryIDHeader string `json:"delivery_id_header,omitempty"`
	BearerTokenEnv   string `json:"bearer_token_env,omitempty"`
}

// mergedWebhookRateLimit mirrors config.WebhookRateLimit.
type mergedWebhookRateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
	Burst             int `json:"burst,omitempty"`
}

// mergedWebhookSyncResp mirrors config.WebhookSyncResponse.
type mergedWebhookSyncResp struct {
	Enabled   bool `json:"enabled,omitempty"`
	TimeoutMs int  `json:"timeout_ms,omitempty"`
}

func (d *mergedWebhookDef) applyOverlay(ov mergedWebhookDef) {
	// Enabled is a plain bool: an overlay can only flip it true. The model
	// disables a webhook by restating the full def via a parent fork
	// rather than relying on overlay zero-value semantics — matching the
	// A2AAgentDef VerifySignedCard / server-card capabilities posture.
	if ov.Enabled {
		d.Enabled = true
	}
	if ov.Delivery != "" {
		d.Delivery = ov.Delivery
	}
	// delivery=spawn and delivery=channel use disjoint target fields
	// (agent vs channel; the validator rejects both-set). When an overlay
	// supplies EXACTLY ONE target, clear the OTHER so a fork can flip a
	// webhook from one delivery mode to the other without leaving a stale
	// field that would trip the validation refusal. When an overlay
	// supplies BOTH (a caller mistake), we deliberately do NOT clear
	// either — the merged def then carries both and the validator rejects
	// it loudly, rather than silently dropping one target. Mirrors
	// A2AAgentDef's both-set reachability posture.
	overlayHasAgent := ov.Agent != ""
	overlayHasChannel := ov.Channel != ""
	if overlayHasAgent {
		d.Agent = ov.Agent
		if !overlayHasChannel {
			d.Channel = ""
		}
	}
	if overlayHasChannel {
		d.Channel = ov.Channel
		if !overlayHasAgent {
			d.Agent = ""
		}
	}
	if ov.Auth.Kind != "" {
		d.Auth.Kind = ov.Auth.Kind
	}
	if ov.Auth.Algorithm != "" {
		d.Auth.Algorithm = ov.Auth.Algorithm
	}
	if ov.Auth.Header != "" {
		d.Auth.Header = ov.Auth.Header
	}
	if ov.Auth.SigningSecretEnv != "" {
		d.Auth.SigningSecretEnv = ov.Auth.SigningSecretEnv
	}
	if ov.Auth.DeliveryIDHeader != "" {
		d.Auth.DeliveryIDHeader = ov.Auth.DeliveryIDHeader
	}
	if ov.Auth.BearerTokenEnv != "" {
		d.Auth.BearerTokenEnv = ov.Auth.BearerTokenEnv
	}
	if ov.RateLimit.RequestsPerMinute != 0 {
		d.RateLimit.RequestsPerMinute = ov.RateLimit.RequestsPerMinute
	}
	if ov.RateLimit.Burst != 0 {
		d.RateLimit.Burst = ov.RateLimit.Burst
	}
	if ov.BodySizeLimitBytes != 0 {
		d.BodySizeLimitBytes = ov.BodySizeLimitBytes
	}
	if ov.UserCredentialsFromEnv != nil {
		d.UserCredentialsFromEnv = ov.UserCredentialsFromEnv
	}
	if ov.UserCredentials != nil {
		d.UserCredentials = ov.UserCredentials
	}
	if ov.Metadata != nil {
		d.Metadata = ov.Metadata
	}
	if ov.TenantID != "" {
		d.TenantID = ov.TenantID
	}
	if ov.PayloadMapping != nil {
		d.PayloadMapping = ov.PayloadMapping
	}
	// sync_response is a small struct; an overlay that touches it replaces
	// the block wholesale (no partial-merge use case — the model either
	// omits the key to keep the parent's value, or restates the pair).
	if ov.SyncResponse != (mergedWebhookSyncResp{}) {
		d.SyncResponse = ov.SyncResponse
	}
	if ov.OnComplete != nil {
		d.OnComplete = ov.OnComplete
	}
}

func staticToMergedWebhookDef(w config.Webhook) mergedWebhookDef {
	out := mergedWebhookDef{
		Enabled:  w.Enabled,
		Delivery: w.Delivery,
		Agent:    w.Agent,
		Channel:  w.Channel,
		Auth: mergedWebhookAuth{
			Kind:             w.Auth.Kind,
			Algorithm:        w.Auth.Algorithm,
			Header:           w.Auth.Header,
			SigningSecretEnv: w.Auth.SigningSecretEnv,
			DeliveryIDHeader: w.Auth.DeliveryIDHeader,
			BearerTokenEnv:   w.Auth.BearerTokenEnv,
		},
		RateLimit: mergedWebhookRateLimit{
			RequestsPerMinute: w.RateLimit.RequestsPerMinute,
			Burst:             w.RateLimit.Burst,
		},
		BodySizeLimitBytes:     w.BodySizeLimitBytes,
		UserCredentialsFromEnv: w.UserCredentialsFromEnv,
		UserCredentials:        w.UserCredentials,
		Metadata:               w.Metadata,
		TenantID:               w.TenantID,
		PayloadMapping:         w.PayloadMapping,
		SyncResponse: mergedWebhookSyncResp{
			Enabled:   w.SyncResponse.Enabled,
			TimeoutMs: w.SyncResponse.TimeoutMs,
		},
		// OnComplete is config.ScheduledRunHook on BOTH sides (the merged
		// shape reuses ScheduleDef's hook type), so the slice is copied by
		// reference — no field-by-field projection needed.
		OnComplete: w.OnComplete,
	}
	return out
}
