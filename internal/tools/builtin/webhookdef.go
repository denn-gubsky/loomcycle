package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"reflect"
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

	// Credentials answers whether the author's tenant holds the credential an
	// auth.secret names. nil: only an operator may name one.
	Credentials *credential.Engine
}

const webhookDefDescription = `Author, fork, retire, and inspect inbound webhook definitions at runtime. ` +
	`Static webhooks.<name>: yaml entries remain the operator's immutable ground truth; this tool ` +
	`produces the DERIVED layer of orchestrator-authored forks. ` +
	`Operations: create, fork, get, list, retire. ` +
	`The definition's tenant_id is the tenant its spawned runs execute in: it defaults to your own tenant, ` +
	`and only an admin may set it to another tenant (a fork that would inherit another tenant's is refused too). ` +
	`A definition carrying capture_disabled was restored from a snapshot without its literal user_credentials: ` +
	`it answers every delivery with 404 until a fork, or a create on the same name, re-supplies EVERY key listed ` +
	`there and sets enabled: true. A key counts as a non-blank user_credentials value, or a ` +
	`user_credentials_from_env variable that is allowlisted (LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST or ` +
	`LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST) and set (the receiver skips any other). While keys are missing, the ` +
	`create/fork result lists them in disabled_until_credentials_supplied, and unusable_credentials says why a ` +
	`key you named did not count. A fork merges user_credentials and user_credentials_from_env key by key with ` +
	`the parent's; an empty value removes a key.`

const webhookDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":            {"type": "string", "enum": ["create","fork","get","list","retire"], "description": "Operation to perform."},
    "name":          {"type": "string", "description": "Webhook name (required for create/fork/list)."},
    "def_id":        {"type": "string", "description": "Existing def_id (required for get/retire)."},
    "parent_def_id": {"type": "string", "description": "Fork parent (optional for fork — when absent, forks the active def of the name, or bootstraps from a yaml template)."},
    "overlay": {
      "type": "object",
      "description": "Mutable subset of the webhook definition for create/fork. delivery is spawn (default — start a run of agent), channel (publish the body to channel) or team (start a detached walk of the team named in team, as TeamDef op=run mode=detach does, with the raw request body as the walk's input). For team, vars maps a variable the team declares to a JSONPath into the body ($, .key and [N] only), e.g. {\"delivery\":\"team\",\"team\":\"pr-review\",\"vars\":{\"repo\":\"$.repository.full_name\",\"pr\":\"$.pull_request.number\"}}; a path the body lacks leaves that variable at the team's default. delivery=team forbids agent, channel, credentials, metadata, on_complete, sync_response and every payload_mapping target except user_id; team and vars are refused on the other deliveries. The team is looked up in the webhook's tenant when a delivery arrives: a missing or retired team, a variable it does not declare, or a projected value it refuses (too long, or containing {{ or }}) starts no walk and answers 400 invalid_run. An accepted delivery answers 202 with the walk's run_id. tenant_id (where spawned runs execute) may name only your own tenant unless you are an admin; omit it to use yours. user_tier pins the tier a spawned run is routed and billed under; a delivery never chooses it, and it must be one of the server's tiers. body_size_limit_bytes may not exceed the server's request limit. The verification secret is named either by an env var (auth.signing_secret_env for hmac, auth.bearer_token_env for bearer; never one of loomcycle's own secrets) or by auth.secret, a stored credential written \"$cred:<name>\" that must be a tenant-level credential of your tenant, read on every delivery so a rotation needs no restart; a literal secret is refused. While rotating, auth.previous_secret names the old credential and a delivery signed with either is accepted; naming a new auth.secret in a fork drops the previous one unless the fork names it too, and naming one form of the secret drops the other. Store such a credential with allowed_urls: [] so it can be compared but never sent. provider (github, gitea or gitlab) supplies defaults for auth.kind, auth.header, auth.delivery_id_header and event_header, applied when a delivery arrives; a field you set overrides it. events lists the only events that start anything, by the name the event header carries (gitlab's Push Hook is listed as push, Merge Request Hook as merge_request): with events set, a delivery whose event header is missing, repeated or malformed answers 400 bad_event, the provider's ping (github) answers 200 and starts nothing, an event not listed answers 202 ignored, and a listed event whose signed body lacks one of its shape's paths answers 400 event_shape_mismatch — the header is not signed, so the body must look like the event. Every listed event needs a shape: a list of rules its signed body must meet, each a JSONPath the body holds, !path for one it must not hold (missing or null), or path == value for a field equal to a JSON string, number or boolean (typed; a string is quoted, e.g. $.object_kind == \"push\"); at least one rule must not be an absence. A shape lets through any signed body that meets its rules, whatever event it came from: github has shapes for push, pull_request, issues, issue_comment, create, delete, release, pull_request_review, pull_request_review_comment and workflow_run, gitea for push, issue_comment, pull_request_comment, create, delete and release, gitlab (which signs nothing) for push, tag_push, merge_request, note, issue and pipeline; gitea's issues and pull_request have none because other events send the same body; event_shapes, a map from an event to its rules, gives one for any other event or replaces the provider's, and a fork merges it key by key ([] removes one). Without events every delivery is handled as before. filters is a list of conditions on the signed body that every delivery must meet before anything starts, each {path, op, value} or {path, op, value_from}: op is eq, ne, glob (a string pattern in which * matches any run of characters, / included, and ? exactly one; nothing else is special) or in (value is a list); a value is a string, number or boolean, and value_from metadata.<key> reads that key of the definition's own metadata instead. Comparison is typed, so a string never equals a number, and a path that is missing, null or of another type than the value misses for every op, ne included. A delivery that misses a filter answers 202 filtered and starts nothing; filters runs after the event check, and a fork replaces the whole list ([] removes it). dedup_key is a list of 1 to 16 JSONPaths into the signed body whose values together name one event (for a github push: $.repository.id, $.ref, $.after): a delivery with the same values is answered as a duplicate (200, deduped, with the run it started) whatever its delivery id or bytes, on every replica and across restarts, for dedup_ttl_seconds (default 604800, 7 days; at most 90 days). A delivery whose body lacks one of the paths, or holds null there, answers 400 dedup_key_unresolvable. The key records the run it started, and is taken again by the next delivery only when that run ended failed; a run that completed or was cancelled, or whose row is gone, keeps it held. A fork replaces the whole list ([] removes it and its lifetime). user_bearer sets the spawned run's bearer (what ${run.user_bearer} in an MCP server's headers becomes) and takes only a stored credential written \"$cred:<name>\": it is not opened when the delivery arrives but resolved for each MCP request, for that server's URL, as a tenant-level credential of the run's tenant (never a user's or an agent's), so a credential restricted by allowed_urls works. It must exist at tenant level in your tenant when written, and is refused on delivery channel or team. Every payload_mapping path must parse ($, .key and [N] only). Keys are read as written: a key the definition does not have, a key repeated, a key that matches a field only by ignoring case, and a server-set key (operator_key_restricted, isolated, capture_disabled) are refused.",
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
	if err := s.checkServerLimits(def); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := config.CheckWebhookMappingPaths(def.PayloadMapping); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := checkWebhookSecretAuthority(ctx, s.Credentials, def.Auth); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if err := checkUserBearerAuthority(ctx, s.Credentials, def.UserBearer); err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	warnLiteralUserCredentials(in.Name, def)
	// RFC N: the spawned run executes as the CREATING principal's tenant,
	// resolved from the authoritative run identity in ctx — never silently
	// empty. F30: an un-stamped tenant made a `spawn` webhook resolve a
	// dynamic agent under "" while AgentDef persisted it under the principal's
	// tenant ("default" for the legacy token) → "unknown agent". An explicit
	// overlay tenant_id wins only when it names the author's own tenant, or
	// the author is an admin. Mirrors AgentDef.execCreate's stamp.
	ident := tools.RunIdentity(ctx)
	if def.TenantID == "" {
		def.TenantID = ident.TenantID
	}
	if res, refused := refuseForeignExecTenant(ctx, "create", def.TenantID); refused {
		return res, nil
	}
	// RFC AX: capture the author's operator-key restriction (server
	// authority — unconditional so the payload/overlay can't set it) so the
	// webhook receiver stamps the fired run with the creator's grant.
	def.OperatorKeyRestricted = operatorKeyRestrictedFromCtx(ctx, s.Cfg)
	// RFC BX P2b: capture the author's isolation status (server
	// authority) so the webhook receiver stamps the fired run confined.
	def.Isolated = tools.AuthorIsolated(ctx)
	// A create on a name whose current def was restored without its literal
	// credentials writes a new version of it: the marker carries over unless
	// this create supplies every stripped key, as a fork's would.
	resupply, err := s.inheritCaptureDisabled(ctx, ident.TenantID, in.Name, &def, in.Overlay)
	if err != nil {
		return errResult(fmt.Sprintf("create: %s", err)), nil
	}
	if len(resupply.Missing) > 0 {
		log.Printf("webhookdef %q: create stays disabled — it did not supply the credential keys a snapshot stripped: %s",
			in.Name, strings.Join(resupply.Missing, ", "))
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

	// RFC N: the def-ROW owning tenant is ALWAYS the authoritative principal
	// (never overlay) — it keys the active pointer + the inbound URL-tenant
	// route. Distinct from def.TenantID above, which is the run-EXECUTION
	// tenant inside the definition JSON (overlay may set it).
	tenantID := ident.TenantID
	row := store.WebhookDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		TenantID:         tenantID,
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
		if err := s.Store.WebhookDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("create: promote: %s", err)), nil
		}
	}
	resp := webhookRowResponse(created, promote)
	resupply.annotate(resp)
	return okJSON(resp)
}

// inheritCaptureDisabled gives a create the capture_disabled marker of the def
// it supersedes — the caller's tenant's active version of the name, else its
// latest — and resolves it against the create's own overlay exactly as a fork
// does. The result's Missing is nil when there was no marker or the create
// supplied every key.
func (s *WebhookDef) inheritCaptureDisabled(ctx context.Context, tenantID, name string, def *mergedWebhookDef, overlay json.RawMessage) (credentialResupply, error) {
	var body json.RawMessage
	row, err := s.Store.WebhookDefGetActive(ctx, tenantID, name)
	switch {
	case err == nil:
		body = row.Definition
	case isNotFound(err):
		rows, lerr := s.Store.WebhookDefListByName(ctx, name)
		if lerr != nil {
			return credentialResupply{}, lerr
		}
		best := -1
		for i, r := range rows {
			if r.TenantID == tenantID && (best < 0 || r.Version > rows[best].Version) {
				best = i
			}
		}
		if best < 0 {
			return credentialResupply{}, nil
		}
		body = rows[best].Definition
	default:
		return credentialResupply{}, err
	}
	stripped, marked := captureDisabledKeys(body)
	if !marked {
		return credentialResupply{}, nil
	}
	def.CaptureDisabled = &mergedWebhookCaptureDisabled{StrippedCredentials: stripped}
	return resolveWebhookCaptureDisabled(def, overlay, webhookEnvGate(s.Cfg))
}

// ---- fork ----

func (s *WebhookDef) execFork(ctx context.Context, policy tools.WebhookDefPolicyValue, in webhookDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("fork: missing required field: name"), nil
	}

	// RFC N: fork resolves the parent within the caller's own tenant first
	// (then the shared "" base); the new version's def-ROW owning tenant is
	// ALWAYS the caller's authoritative tenant.
	ident := tools.RunIdentity(ctx)
	tenantID := ident.TenantID

	// Resolve the parent. Paths (mirror AgentDef):
	//   1. parent_def_id supplied → pin (refuse another tenant's private def)
	//   2. parent_def_id empty + own-tenant active pointer → use it
	//   3. else shared "" active pointer (for non-"" tenants) → use it
	//   4. neither → name must have a yaml template; bootstrap v1
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
		// Allow forking the shared "" base or the caller's own def; refuse
		// another tenant's private def unless substrate:admin.
		if !forkParentVisible(ctx, row.TenantID, tenantID) {
			return errResult(fmt.Sprintf("fork: parent_def_id %q not found", parentDefID)), nil
		}
		if row.Name != in.Name {
			return errResult(fmt.Sprintf("fork: parent_def_id %q has name %q, refusing to fork under name %q", parentDefID, row.Name, in.Name)), nil
		}
		parent = row
	} else {
		row, err := s.Store.WebhookDefGetActive(ctx, tenantID, in.Name)
		if err == nil {
			parent = row
			parentDefID = row.DefID
		} else {
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				return errResult(fmt.Sprintf("fork: %s", err)), nil
			}
			// No own-tenant active pointer. Fall back to the SHARED ("") base
			// (same precedence lookup.Webhook walks); the fork still lands
			// under the caller's tenant. Skip when tenantID is already "".
			if tenantID != "" {
				if shared, serr := s.Store.WebhookDefGetActive(ctx, "", in.Name); serr == nil {
					parent = shared
					parentDefID = shared.DefID
				} else if !errors.As(serr, &nf) {
					return errResult(fmt.Sprintf("fork: %s", serr)), nil
				}
			}
			// Still no parent → bootstrap from yaml, else refuse.
			if parentDefID == "" {
				static, ok := s.Cfg.Webhooks[in.Name]
				if !ok {
					return errResult(fmt.Sprintf("fork: no parent — name %q has neither a DB version (own tenant or shared \"\") nor a static cfg.Webhooks entry", in.Name)), nil
				}
				bootstrap, berr := s.bootstrapStatic(ctx, in.Name, static)
				if berr != nil {
					// Concurrent first-fork may have already bootstrapped v1;
					// re-read own-tenant active pointer before propagating.
					if row2, gerr := s.Store.WebhookDefGetActive(ctx, tenantID, in.Name); gerr == nil {
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
	// A parent restored without its literal credentials carries a marker;
	// this fork clears it only by re-supplying every stripped key.
	resupply, err := resolveWebhookCaptureDisabled(&def, in.Overlay, webhookEnvGate(s.Cfg))
	if err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if len(resupply.Missing) > 0 {
		log.Printf("webhookdef %q: fork stays disabled — it did not re-supply the credential keys a snapshot stripped: %s",
			in.Name, strings.Join(resupply.Missing, ", "))
	}
	if err := validateWebhookDef(def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := s.checkServerLimits(def); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := config.CheckWebhookMappingPaths(def.PayloadMapping); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := checkWebhookSecretAuthority(ctx, s.Credentials, def.Auth); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	if err := checkUserBearerAuthority(ctx, s.Credentials, def.UserBearer); err != nil {
		return errResult(fmt.Sprintf("fork: %s", err)), nil
	}
	warnLiteralUserCredentials(in.Name, def)
	// Run-execution tenant (in the definition JSON) defaults to the forking
	// principal's tenant unless the parent/overlay already carries one; a
	// non-admin may not end up with another tenant's, either way.
	if def.TenantID == "" {
		def.TenantID = ident.TenantID
	}
	if res, refused := refuseForeignExecTenant(ctx, "fork", def.TenantID); refused {
		return res, nil
	}
	// RFC AX: re-capture the forker's operator-key restriction (server
	// authority) — a fork is a new version, its authority is the forker's grant.
	def.OperatorKeyRestricted = operatorKeyRestrictedFromCtx(ctx, s.Cfg)
	// RFC BX P2b: capture the author's isolation status (server
	// authority) so the webhook receiver stamps the fired run confined.
	def.Isolated = tools.AuthorIsolated(ctx)
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

	row := store.WebhookDefRow{
		DefID:            mintDefID(),
		Name:             in.Name,
		ParentDefID:      parentDefID,
		Definition:       defJSON,
		Description:      in.Description,
		CreatedByAgentID: ident.AgentID,
		TenantID:         tenantID,
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
		if err := s.Store.WebhookDefSetActive(ctx, tenantID, in.Name, created.DefID, ident.AgentID); err != nil {
			return errResult(fmt.Sprintf("fork: promote: %s", err)), nil
		}
	}
	resp := webhookRowResponse(created, promote)
	resupply.annotate(resp)
	return okJSON(resp)
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
	// RFC N: refuse cross-tenant reads with the same opaque not-found a
	// missing def returns.
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("get: def_id %q not found", in.DefID)), nil
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
	// RFC N: filter to the caller's own tenant; a substrate:admin sees all.
	tenantID := tools.RunIdentity(ctx).TenantID
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if !defCallerIsAdmin(ctx) && r.TenantID != tenantID {
			continue
		}
		out = append(out, webhookRowResponseMap(r))
	}
	return okJSONCount(map[string]any{"name": in.Name, "versions": out}, len(out))
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
	// RFC N: refuse cross-tenant retire (global by-def_id mutation).
	if !defCallerIsAdmin(ctx) && row.TenantID != tools.RunIdentity(ctx).TenantID {
		return errResult(fmt.Sprintf("retire: def_id %q not found", in.DefID)), nil
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
		if err := checkWebhookOverlayKeys(overlay); err != nil {
			return mergedWebhookDef{}, err
		}
		var ov mergedWebhookDef
		if err := json.Unmarshal(overlay, &ov); err != nil {
			return mergedWebhookDef{}, fmt.Errorf("parse overlay: %w", err)
		}
		// Checked on the overlay as well as on the result, because naming
		// one form of the secret drops the other from the parent: both in
		// one overlay would otherwise lose one without a word.
		if (ov.Auth.Secret != "" || ov.Auth.PreviousSecret != "") && (ov.Auth.SigningSecretEnv != "" || ov.Auth.BearerTokenEnv != "") {
			return mergedWebhookDef{}, fmt.Errorf("auth.secret and auth.previous_secret replace auth.signing_secret_env and auth.bearer_token_env: set one of the two forms, not both")
		}
		base.applyOverlay(ov)
	}
	// Unlike A2AServerCardDef, config.Webhook carries no Name field — the
	// webhook's addressable name is the registry key (the `name` arg)
	// only, exactly like A2AAgentDef. So no name stamp here.
	return base, nil
}

// webhookServerKeys are keys of the stored shape that the server sets and an
// overlay never does. applyOverlay ignores them; an overlay that names one is
// refused, so an author is told the value they wrote was not taken.
var webhookServerKeys = map[string]string{
	"operator_key_restricted": "it is taken from the author's token",
	"isolated":                "it is taken from the author's token",
	"capture_disabled":        "only a snapshot restore sets it",
}

// checkWebhookOverlayKeys refuses an overlay that would not be read as
// written: a key no field takes, a key repeated, a key matched only by
// ignoring its case, or a key the server sets. Go's decoder drops an unknown
// key without a word, so a misspelt or newer field (an "events" list sent to
// a server that does not know it) became a webhook without it. Applied to
// what an author sends, never to a stored definition, which a snapshot
// restore re-validates.
func checkWebhookOverlayKeys(overlay json.RawMessage) error {
	var top map[string]json.RawMessage
	if json.Unmarshal(overlay, &top) == nil {
		keys := make([]string, 0, len(top))
		for k := range top {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if why, ok := webhookServerKeys[k]; ok {
				return fmt.Errorf("overlay key %q cannot be set: %s", k, why)
			}
		}
	}
	if issues := teamgraph.KeyIssuesOf(overlay, reflect.TypeOf(mergedWebhookDef{})); len(issues) > 0 {
		return fmt.Errorf("overlay: %s", issues[0].Msg)
	}
	return nil
}

// checkServerLimits refuses what this server could not honour: a user_tier
// it does not define (the receiver would refuse every delivery), and a body
// size limit above its request limit (a delivery is never read past that).
// Checked at create and fork, where the server's config is at hand; a stored
// definition is not refused for it, and the receiver caps the limit anyway.
func (s *WebhookDef) checkServerLimits(def mergedWebhookDef) error {
	if s.Cfg == nil {
		return nil
	}
	if def.UserTier != "" && len(s.Cfg.UserTiers) > 0 {
		if _, ok := s.Cfg.UserTiers[def.UserTier]; !ok {
			return fmt.Errorf("user_tier %q is not one of this server's user tiers", def.UserTier)
		}
	}
	if limit := s.Cfg.Env.MaxRequestBytes; limit > 0 && int64(def.BodySizeLimitBytes) > limit {
		return fmt.Errorf("body_size_limit_bytes %d is above this server's request limit of %d bytes", def.BodySizeLimitBytes, limit)
	}
	return nil
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
		// RFC N: bootstrap lands in the caller's tenant ("" for the boot
		// webhook-bootstrap identity → the shared base).
		TenantID: ident.TenantID,
	}
	created, err := s.Store.WebhookDefCreate(ctx, row)
	if err != nil {
		return store.WebhookDefRow{}, err
	}
	if err := s.Store.WebhookDefSetActive(ctx, ident.TenantID, name, created.DefID, ident.AgentID); err != nil {
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
	// RFC N: boot bootstrap operates on the caller's tenant — the
	// webhook-bootstrap identity carries "" (the shared base).
	tenantID := tools.RunIdentity(ctx).TenantID
	seeded := 0
	for _, name := range names {
		_, err := s.Store.WebhookDefGetActive(ctx, tenantID, name)
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

// warnLiteralUserCredentials logs a registration-time nudge when a webhook def
// carries a LITERAL bearer in user_credentials (vs user_credentials_from_env).
// F32: a literal here is baked into the signed / content-hashed / snapshotted
// def and is readable at rest. We WARN rather than refuse — RFC F deliberately
// permits the literal for fork-time explicit values, so refusing would break a
// legitimate path; the env-name form (user_credentials_from_env) keeps the
// secret out of the DB and is the preferred posture.
func warnLiteralUserCredentials(name string, def mergedWebhookDef) {
	if len(def.UserCredentials) > 0 {
		log.Printf("webhookdef %q: user_credentials carries %d literal value(s) baked into the stored def (readable at rest) — prefer user_credentials_from_env to keep secrets out of the DB (F32)", name, len(def.UserCredentials))
	}
}

// ValidateWebhookDefBody re-runs the authoring validation over a stored
// webhook def body. A snapshot restore calls it (injected by the restore call
// sites) before writing a body, so a restored def is one an author could
// have created on this host.
func ValidateWebhookDefBody(body json.RawMessage) error {
	var def mergedWebhookDef
	if err := json.Unmarshal(body, &def); err != nil {
		return fmt.Errorf("definition does not decode as a webhook def: %w", err)
	}
	return validateWebhookDef(def)
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
		if def.Team != "" || len(def.Vars) > 0 {
			return fmt.Errorf("delivery=spawn forbids team and vars (set delivery: team to start a team walk)")
		}
	case "team":
		if err := config.CheckWebhookTeamDelivery(def.Team, def.Vars); err != nil {
			return err
		}
		// One list of refused fields for this tool and the yaml load.
		if field := config.WebhookTeamDeliveryForbids(config.Webhook{
			Agent:                  def.Agent,
			Channel:                def.Channel,
			UserTier:               def.UserTier,
			UserCredentials:        def.UserCredentials,
			UserCredentialsFromEnv: def.UserCredentialsFromEnv,
			UserBearer:             def.UserBearer,
			Metadata:               def.Metadata,
			OnComplete:             def.OnComplete,
			SyncResponse:           config.WebhookSyncResponse{Enabled: def.SyncResponse.Enabled},
			PayloadMapping:         def.PayloadMapping,
		}); field != "" {
			return fmt.Errorf("delivery=team forbids %s", field)
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
		if def.UserBearer != "" {
			return fmt.Errorf("delivery=channel forbids user_bearer: it authorizes a run, and a channel delivery starts none")
		}
		for k := range def.PayloadMapping {
			if strings.HasPrefix(k, "user_credentials.") {
				return fmt.Errorf("delivery=channel forbids payload_mapping key %q (RFC H Decision 11: channel mode cannot map user credentials)", k)
			}
		}
		if def.Team != "" || len(def.Vars) > 0 {
			return fmt.Errorf("delivery=channel forbids team and vars (set delivery: team to start a team walk)")
		}
	default:
		return fmt.Errorf("unknown delivery %q (must be one of: spawn, channel, team)", def.Delivery)
	}

	// The auth block as a delivery sees it: a provider's defaults in the
	// fields it leaves empty (a gitlab definition is a bearer one).
	events := config.Webhook{
		Auth:     config.WebhookAuth(def.Auth),
		Provider: def.Provider, EventHeader: def.EventHeader,
		Events: def.Events, EventShapes: def.EventShapes,
		Filters: def.Filters, Metadata: def.Metadata,
		DedupKey: def.DedupKey, DedupTTLSeconds: def.DedupTTLSeconds,
	}
	if err := validateWebhookAuth(mergedWebhookAuth(events.WithProviderDefaults().Auth)); err != nil {
		return err
	}
	if err := config.CheckWebhookEvents(events); err != nil {
		return err
	}
	if err := config.CheckWebhookFilters(events); err != nil {
		return err
	}
	if err := config.CheckTriggerUserBearer(def.UserBearer); err != nil {
		return err
	}
	if err := config.CheckWebhookDedupKey(events); err != nil {
		return err
	}

	// Every value in user_credentials_from_env must be a valid env-var
	// name (WH-4 checks resolvability against the allowlist).
	for k, v := range def.UserCredentialsFromEnv {
		if !envVarNameRe.MatchString(v) {
			return fmt.Errorf("user_credentials_from_env[%q] = %q is not a valid env-var name (must match [A-Z][A-Z0-9_]*)", k, v)
		}
	}

	// Every payload_mapping value must be non-empty. Its syntax is checked
	// at create and fork (config.CheckWebhookMappingPaths), not here: a
	// snapshot restore re-runs this and must accept what it accepted.
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

// validateWebhookAuth checks a webhook's auth block: auth.kind ∈ {"", "hmac",
// "bearer", "none"} ("" treated as hmac), the secret its kind needs named by a
// well-formed env-var name or by a stored credential (auth.secret), and
// nothing it does not use. A team's own webhooks are checked by it too, so
// they are held to exactly a WebhookDef's rules.
//
// Shape only: whether a stored credential exists, and whether the author may
// name what it names, is checkWebhookSecretAuthority's, at create and fork. A
// snapshot restore re-runs this and must accept what it accepted before.
func validateWebhookAuth(a mergedWebhookAuth) error {
	if err := config.CheckWebhookSecretRefs(config.WebhookAuth(a)); err != nil {
		return err
	}
	switch a.Kind {
	case "", "hmac":
		if a.SigningSecretEnv == "" && a.Secret == "" {
			return fmt.Errorf("auth.kind=hmac requires auth.signing_secret_env or auth.secret")
		}
		if a.SigningSecretEnv != "" && !envVarNameRe.MatchString(a.SigningSecretEnv) {
			return fmt.Errorf("auth.signing_secret_env %q is not a valid env-var name (must match [A-Z][A-Z0-9_]*)", a.SigningSecretEnv)
		}
		// SHA-256 is the only digest the receiver implements (verifyHMAC
		// hardcodes sha256.New). The Algorithm field is carried through
		// every Def site, so an unsupported value would be silently dropped
		// and the sender's valid signatures rejected with an opaque 401 —
		// the never-silently-degrade contract (RFC H Decision 9) demands a
		// loud refusal here instead. "" defaults to sha256.
		if alg := strings.ToLower(strings.TrimSpace(a.Algorithm)); alg != "" && alg != "sha256" {
			return fmt.Errorf("auth.algorithm %q unsupported (only sha256 is implemented)", a.Algorithm)
		}
	case "bearer":
		if a.BearerTokenEnv == "" && a.Secret == "" {
			return fmt.Errorf("auth.kind=bearer requires auth.bearer_token_env or auth.secret")
		}
		if a.BearerTokenEnv != "" && !envVarNameRe.MatchString(a.BearerTokenEnv) {
			return fmt.Errorf("auth.bearer_token_env %q is not a valid env-var name (must match [A-Z][A-Z0-9_]*)", a.BearerTokenEnv)
		}
	case "none":
		// Trusted-network escape hatch: the receiver performs NO verification.
		// It is honored only when the operator sets
		// LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED=1; otherwise every delivery
		// 503s "unauthenticated_mode_disabled". Forbid declaring a secret that
		// would never be consumed — silent dead config is worse than a loud
		// refusal (RFC H Decision 9, never-silently-degrade).
		if a.SigningSecretEnv != "" || a.BearerTokenEnv != "" || a.Secret != "" || a.PreviousSecret != "" {
			return fmt.Errorf("auth.kind=none forbids signing_secret_env / bearer_token_env / secret / previous_secret (no verification is performed)")
		}
	default:
		return fmt.Errorf("unknown auth.kind %q (must be one of: hmac, bearer, none)", a.Kind)
	}
	return nil
}

// checkWebhookSecretAuthority refuses, at create and fork, a verification
// secret the author may not name. A fork is judged on what it writes, the
// secret it carries over from its parent included.
//
//   - An env var that is one of loomcycle's own secrets (the operator bearer,
//     the database DSN, the credential key, ...), for every author: the
//     receiver refuses to read one, so the definition could never work.
//   - A stored secret ("$cred:<name>") that is not a tenant-level credential
//     of the author's tenant — the tenant the receiver reads it from. An
//     author with no tenant would name the shared tenant's credentials, and is
//     refused. An operator (substrate:admin, or open-mode / stdio) is not
//     checked, so a credential may be created after its definition.
//
// Not part of validateWebhookAuth: a snapshot restore re-runs that, and an
// author's tenant is not a restore's.
func checkWebhookSecretAuthority(ctx context.Context, creds *credential.Engine, a mergedWebhookAuth) error {
	for _, env := range []struct{ field, name string }{{"auth.signing_secret_env", a.SigningSecretEnv}, {"auth.bearer_token_env", a.BearerTokenEnv}} {
		if env.name != "" && config.IsInfraSecretEnv(env.name) {
			return fmt.Errorf("%s %q is one of loomcycle's own secrets and cannot verify a webhook", env.field, env.name)
		}
	}
	if a.Secret == "" || callerHasOperatorAuthority(ctx) {
		return nil
	}
	tenantID := tools.RunIdentity(ctx).TenantID
	for _, ref := range []struct{ field, value string }{{"auth.secret", a.Secret}, {"auth.previous_secret", a.PreviousSecret}} {
		if ref.value == "" {
			continue
		}
		name, _ := credential.ParseRef(ref.value) // the shape check passed
		if tenantID == "" {
			return fmt.Errorf("%s %q may not be set by an author with no tenant: it would read the shared tenant's credentials", ref.field, ref.value)
		}
		if creds == nil {
			return fmt.Errorf("%s %q: this server stores no credentials", ref.field, ref.value)
		}
		found, err := creds.HasToCompare(ctx, tenantID, name)
		if errors.Is(err, credential.ErrExpired) {
			// The check worked; the answer is no, and the error says why.
			return fmt.Errorf("%s %q: %v", ref.field, ref.value, err)
		}
		if err != nil {
			return fmt.Errorf("%s %q: could not check the credential: %v", ref.field, ref.value, err)
		}
		if !found {
			return fmt.Errorf("%s %q names no tenant-level credential in tenant %q. "+
				"Create it first with CredentialDef (op=create, scope=tenant, name=%q); "+
				"allowed_urls: [] keeps it from ever being sent anywhere", ref.field, ref.value, tenantID, name)
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
	Enabled                bool                   `json:"enabled,omitempty"`
	Delivery               string                 `json:"delivery,omitempty"`
	Agent                  string                 `json:"agent,omitempty"`
	Channel                string                 `json:"channel,omitempty"`
	Auth                   mergedWebhookAuth      `json:"auth,omitempty"`
	RateLimit              mergedWebhookRateLimit `json:"rate_limit,omitempty"`
	BodySizeLimitBytes     int                    `json:"body_size_limit_bytes,omitempty"`
	UserCredentialsFromEnv map[string]string      `json:"user_credentials_from_env,omitempty"`
	UserCredentials        map[string]string      `json:"user_credentials,omitempty"` // RFC F fork-time explicit values (ScheduleDef parity)
	// UserTier pins the started run's user tier, as config.Webhook's does: a
	// delivery never chooses it. It was missing from this shape, so a
	// runtime-authored def that set it lost it without a word.
	UserTier       string                    `json:"user_tier,omitempty"`
	Metadata       map[string]any            `json:"metadata,omitempty"`  // non-secret, trusted agent metadata
	TenantID       string                    `json:"tenant_id,omitempty"` // tenant the spawned run executes as (RFC N follow-up); def-authored, never from payload
	PayloadMapping map[string]string         `json:"payload_mapping,omitempty"`
	SyncResponse   mergedWebhookSyncResp     `json:"sync_response,omitempty"`
	OnComplete     []config.ScheduledRunHook `json:"on_complete,omitempty"`
	// Provider, EventHeader, Events and EventShapes mirror config.Webhook's:
	// which events start anything, and the shape each must have.
	Provider    string              `json:"provider,omitempty"`
	EventHeader string              `json:"event_header,omitempty"`
	Events      []string            `json:"events,omitempty"`
	EventShapes map[string][]string `json:"event_shapes,omitempty"`
	// Filters mirrors config.Webhook's: conditions on the signed body every
	// delivery must meet to start anything.
	Filters []config.WebhookFilter `json:"filters,omitempty"`
	// DedupKey / DedupTTLSeconds mirror config.Webhook's: the signed fields
	// that name one event, and how long that name is held.
	DedupKey        []string `json:"dedup_key,omitempty"`
	DedupTTLSeconds int      `json:"dedup_ttl_seconds,omitempty"`
	// UserBearer mirrors config.Webhook's: the spawned run's bearer, a
	// "$cred:<name>" reference resolved for each MCP request.
	UserBearer string `json:"user_bearer,omitempty"`
	// OperatorKeyRestricted is the RFC AX bit CAPTURED from the authoring
	// principal at create/fork (server authority, NOT payload/overlay-set); the
	// webhook receiver copies it into RunInput so a fired run keeps its creator's
	// restriction. No content_sha256 for webhooks; omitempty keeps gate-off
	// bodies byte-identical.
	OperatorKeyRestricted bool `json:"operator_key_restricted,omitempty"`
	// Isolated is the RFC BX P2b confinement bit CAPTURED from the authoring
	// principal at create/fork (server authority, NOT payload/overlay-set); the
	// webhook receiver copies it into RunInput so a fired run keeps its creator's
	// confinement. Mirrors OperatorKeyRestricted; omitempty keeps pre-P2b bodies
	// byte-identical. Drift-tested against config.Webhook / SubstrateWebhookDef.
	Isolated bool `json:"isolated,omitempty"`
	// Team / Vars are the delivery=team target: the team whose walk a delivery
	// starts (detached, the raw body as its input), and the walk's variables
	// as strict-subset JSONPaths into that body. Whether the team exists and
	// declares these names is checked when a delivery arrives, in the def's
	// own tenant.
	Team string            `json:"team,omitempty"`
	Vars map[string]string `json:"vars,omitempty"`
	// CaptureDisabled is the marker a snapshot restore writes on a def whose
	// literal user_credentials values were stripped from the snapshot (they
	// never travel). Server authority: only restore sets it, and applyOverlay
	// never copies it, so an overlay can neither set nor clear it. The
	// receiver treats a marked def as disabled whatever `enabled` says; a fork
	// clears the marker only when its own overlay re-supplies EVERY listed
	// key, so no delivery runs without the credentials the def was authored
	// with.
	CaptureDisabled *mergedWebhookCaptureDisabled `json:"capture_disabled,omitempty"`
}

// mergedWebhookCaptureDisabled lists the credential keys a snapshot stripped
// and a fork has not yet re-supplied.
type mergedWebhookCaptureDisabled struct {
	StrippedCredentials []string `json:"stripped_credentials,omitempty"`
}

// resolveWebhookCaptureDisabled applies a fork's overlay to the marker the
// def inherited from its parent (same rule as the ScheduleDef fork). Every
// key re-supplied by the overlay: the marker is dropped and the overlay's
// `enabled` stands. Otherwise the marker keeps the keys still missing and the
// def stays disabled whatever the overlay says. The result's Missing is nil
// when the marker cleared or was never there.
func resolveWebhookCaptureDisabled(def *mergedWebhookDef, overlay json.RawMessage, gate credentialEnvGate) (credentialResupply, error) {
	if def.CaptureDisabled == nil {
		return credentialResupply{}, nil
	}
	r, err := credentialsStillMissing(def.CaptureDisabled.StrippedCredentials, overlay, gate)
	if err != nil {
		return credentialResupply{}, err
	}
	if len(r.Missing) == 0 {
		def.CaptureDisabled = nil
		return r, nil
	}
	def.CaptureDisabled = &mergedWebhookCaptureDisabled{StrippedCredentials: r.Missing}
	def.Enabled = false
	return r, nil
}

// webhookEnvGate mirrors the webhook receiver, which main wires with the same
// WebhookEnvAllowlist (via webhook.BuildEnvAllowlist).
func webhookEnvGate(cfg *config.Config) credentialEnvGate {
	return credentialEnvGate{
		allowlist: cfg.WebhookEnvAllowlist(),
		knobs:     "LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST or LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST",
	}
}

// mergeCredentialMap returns base with overlay's keys laid over it, key by
// key, as a fresh map (base may alias a static config map). An empty overlay
// value removes the key: the receiver skips an empty literal anyway, and an
// empty env-var name would fail validation, so removing it is the only way a
// fork can drop a key now that a smaller map no longer replaces the parent's.
func mergeCredentialMap(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overlay {
		if v == "" {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// mergedWebhookAuth mirrors config.WebhookAuth.
type mergedWebhookAuth struct {
	Kind             string `json:"kind,omitempty"`
	Algorithm        string `json:"algorithm,omitempty"`
	Header           string `json:"header,omitempty"`
	SigningSecretEnv string `json:"signing_secret_env,omitempty"`
	DeliveryIDHeader string `json:"delivery_id_header,omitempty"`
	BearerTokenEnv   string `json:"bearer_token_env,omitempty"`
	Secret           string `json:"secret,omitempty"`
	PreviousSecret   string `json:"previous_secret,omitempty"`
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
	//
	// delivery=team adds a third target (team, with its vars) under the same
	// rule: an overlay naming exactly one target clears the other two.
	overlayHasAgent := ov.Agent != ""
	overlayHasChannel := ov.Channel != ""
	overlayHasTeam := ov.Team != ""
	if overlayHasAgent {
		d.Agent = ov.Agent
		if !overlayHasChannel && !overlayHasTeam {
			d.Channel = ""
			d.Team, d.Vars = "", nil
		}
	}
	if overlayHasChannel {
		d.Channel = ov.Channel
		if !overlayHasAgent && !overlayHasTeam {
			d.Agent = ""
			d.Team, d.Vars = "", nil
		}
	}
	if overlayHasTeam {
		d.Team = ov.Team
		if !overlayHasAgent && !overlayHasChannel {
			d.Agent = ""
			d.Channel = ""
		}
	}
	// Replaced wholesale, like payload_mapping: the variables a delivery
	// fills are one statement, and a fork that names fewer means fewer.
	if ov.Vars != nil {
		d.Vars = ov.Vars
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
	// The two forms of a secret replace each other: an overlay that names
	// one drops the other its parent carried, so a fork moving to a stored
	// secret is not refused for keeping the env name, nor the reverse. A
	// previous secret belongs to the secret it preceded: naming a new secret
	// drops it unless the overlay names one too.
	switch {
	case ov.Auth.Secret != "":
		d.Auth.Secret, d.Auth.PreviousSecret = ov.Auth.Secret, ov.Auth.PreviousSecret
		d.Auth.SigningSecretEnv, d.Auth.BearerTokenEnv = "", ""
	case ov.Auth.SigningSecretEnv != "" || ov.Auth.BearerTokenEnv != "":
		d.Auth.Secret, d.Auth.PreviousSecret = "", ""
	case ov.Auth.PreviousSecret != "":
		d.Auth.PreviousSecret = ov.Auth.PreviousSecret
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
	if ov.UserTier != "" {
		d.UserTier = ov.UserTier
	}
	// Credentials maps merge key by key, as ScheduleDef's do: a fork that
	// re-supplies the keys a snapshot stripped must not drop the parent's
	// other credentials by restating a smaller map.
	d.UserCredentialsFromEnv = mergeCredentialMap(d.UserCredentialsFromEnv, ov.UserCredentialsFromEnv)
	d.UserCredentials = mergeCredentialMap(d.UserCredentials, ov.UserCredentials)
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
	if ov.Provider != "" {
		d.Provider = ov.Provider
	}
	if ov.EventHeader != "" {
		d.EventHeader = ov.EventHeader
	}
	if len(ov.Events) > 0 {
		d.Events = ov.Events
	}
	// event_shapes merges key by key, so a fork changing one event's shape
	// does not restate the others; an empty list removes that event's. Into
	// a copy: buildDefinition can start from the static entry, whose map is
	// the loaded config's.
	if len(ov.EventShapes) > 0 {
		merged := make(map[string][]string, len(d.EventShapes)+len(ov.EventShapes))
		for e, paths := range d.EventShapes {
			merged[e] = paths
		}
		for e, paths := range ov.EventShapes {
			if len(paths) == 0 {
				delete(merged, e)
			} else {
				merged[e] = paths
			}
		}
		if len(merged) == 0 {
			merged = nil
		}
		d.EventShapes = merged
	}
	// filters is replaced whole: the conditions are read together, and a
	// fork restating the list is clearer than one merged by position. An
	// empty list removes them.
	if ov.Filters != nil {
		d.Filters = ov.Filters
	}
	// dedup_key is replaced whole, like filters; removing it removes the
	// lifetime it had, which means nothing without it.
	if ov.DedupKey != nil {
		d.DedupKey = ov.DedupKey
		if len(ov.DedupKey) == 0 {
			d.DedupKey, d.DedupTTLSeconds = nil, 0
		}
	}
	if ov.DedupTTLSeconds != 0 {
		d.DedupTTLSeconds = ov.DedupTTLSeconds
	}
	if ov.UserBearer != "" {
		d.UserBearer = ov.UserBearer
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
			Secret:           w.Auth.Secret,
			PreviousSecret:   w.Auth.PreviousSecret,
		},
		RateLimit: mergedWebhookRateLimit{
			RequestsPerMinute: w.RateLimit.RequestsPerMinute,
			Burst:             w.RateLimit.Burst,
		},
		BodySizeLimitBytes:     w.BodySizeLimitBytes,
		UserTier:               w.UserTier,
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
		OnComplete:  w.OnComplete,
		Provider:    w.Provider,
		EventHeader: w.EventHeader,
		Events:      w.Events,
		EventShapes: w.EventShapes,
		Filters:     w.Filters,
		DedupKey:    w.DedupKey,
		// The lifetime goes with the key: a fork of a static definition that
		// leaves both out keeps both.
		DedupTTLSeconds: w.DedupTTLSeconds,
		UserBearer:      w.UserBearer,
		Team:            w.Team,
		Vars:            w.Vars,
	}
	return out
}
