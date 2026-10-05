package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// teamdef_localwebhooks.go — a team's own webhooks, on the authoring side.
//
// A local webhook's auth block is a WebhookDef's, decoded into the same type
// and checked by the same function (validateWebhookAuth), so a team cannot
// declare an endpoint a WebhookDef could not be. Its secrets are env-var
// NAMES, resolved when a delivery arrives under the receiver's own allowlist,
// exactly as a runtime WebhookDef's are. On top of that, everything that would
// make a delivery more than a message published into the team is refused by
// name: a delivery has no agent, team, run, credentials, tier or tenant of its
// own.
//
// Declaring one is gated like creating a WebhookDef: it opens an endpoint
// anyone can POST to, so an author who could not create a webhook definition
// (an agent inside a run — that policy is set only on the operator planes)
// cannot open one by writing a team.

// LocalWebhookDefinition is one of a team's own webhooks, decoded.
type LocalWebhookDefinition struct {
	// Auth is verified exactly as a WebhookDef's auth is.
	Auth config.WebhookAuth
	// Channel is the team's own channel a delivery publishes into, without
	// "./".
	Channel string
	// UserIDPath is the JSONPath in the body naming the delivery's user, ""
	// when the webhook maps none.
	UserIDPath string
	// UserScoped: the channel is scope=user, so a delivery is published only
	// under the user it names.
	UserScoped bool
}

// localWebhookBody is a local webhook as written. Auth is a WebhookDef's own
// auth type.
type localWebhookBody struct {
	Auth           mergedWebhookAuth `json:"auth"`
	Channel        string            `json:"channel"`
	PayloadMapping map[string]string `json:"payload_mapping"`
}

// localWebhookRefused names, for a field a WebhookDef takes and a team's own
// webhook does not, why.
var localWebhookRefused = map[string]string{
	"delivery":                  "a team's own webhook always publishes the request body into one of the team's own channels",
	"agent":                     "a team's own webhook starts nothing; it publishes into the team, and a starter reading the channel is what it wakes",
	"team":                      "a team's own webhook belongs to its team",
	"vars":                      "a team's own webhook starts no walk to set variables of",
	"sync_response":             "a delivery ends once its body is published; there is no run to wait for",
	"on_complete":               "a delivery ends once its body is published; there is no run to follow",
	"user_credentials":          "no run starts for a delivery, so there is nothing to hand credentials to",
	"user_credentials_from_env": "no run starts for a delivery, so there is nothing to hand credentials to",
	"metadata":                  "the message a delivery publishes is the request body",
	"user_tier":                 "no run starts for a delivery, so there is no tier to pin",
	"tenant_id":                 "a team's own webhook belongs to the team's tenant",
	"enabled":                   "it answers while a walk of the team runs, and at no other time",
	"rate_limit":                "a team's own webhook takes the receiver's default rate",
	"body_size_limit_bytes":     "a team's own webhook takes the receiver's default body size limit",
}

// decodeLocalWebhook decodes a local webhook's body strictly: a field it does
// not take is refused by name, never dropped.
func decodeLocalWebhook(body json.RawMessage) (localWebhookBody, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return localWebhookBody{}, fmt.Errorf("must be an object of auth, channel and payload_mapping")
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "auth", "channel", "payload_mapping":
			continue
		}
		msg := fmt.Sprintf("%q is not a field of a team's own webhook, which takes only auth, channel and payload_mapping", k)
		if why, ok := localWebhookRefused[k]; ok {
			msg += " (" + why + ")"
		}
		return localWebhookBody{}, fmt.Errorf("%s", msg)
	}
	var out localWebhookBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return localWebhookBody{}, fmt.Errorf("not a webhook definition: %v", err)
	}
	return out, nil
}

// LocalWebhookOf decodes and checks the webhook `name` of def: its auth is a
// WebhookDef's, its channel one the team declares, its payload_mapping maps at
// most user_id with a supported JSONPath, and a user-scoped channel's webhook
// maps user_id. The authoring check, the restore check and the walk that arms
// it all read a webhook through here, so they agree on what it is.
func LocalWebhookOf(def teamgraph.Definition, name string) (LocalWebhookDefinition, error) {
	body, ok := def.LocalWebhook(name)
	if !ok {
		return LocalWebhookDefinition{}, fmt.Errorf("the team declares no webhook of its own named %q", name)
	}
	wb, err := decodeLocalWebhook(body)
	if err != nil {
		return LocalWebhookDefinition{}, err
	}
	if err := validateWebhookAuth(wb.Auth); err != nil {
		return LocalWebhookDefinition{}, err
	}
	local, isLocal := teamgraph.LocalRef(wb.Channel)
	if !isLocal {
		return LocalWebhookDefinition{}, fmt.Errorf("channel %q must name one of the team's own channels as \"./<name>\"", wb.Channel)
	}
	chBody, declared := def.LocalChannel(local)
	if !declared {
		return LocalWebhookDefinition{}, fmt.Errorf("channel %q names a channel the team does not declare under local.channels", wb.Channel)
	}
	ch, err := decodeLocalChannel(chBody)
	if err != nil {
		return LocalWebhookDefinition{}, fmt.Errorf("channel %q: %w", wb.Channel, err)
	}
	targets := make([]string, 0, len(wb.PayloadMapping))
	for target := range wb.PayloadMapping {
		targets = append(targets, target)
	}
	sort.Strings(targets) // the same refusal twice
	for _, target := range targets {
		if target != "user_id" {
			return LocalWebhookDefinition{}, fmt.Errorf("payload_mapping target %q: only user_id applies — a delivery publishes the whole body, attributed to the user user_id names", target)
		}
	}
	userPath := wb.PayloadMapping["user_id"]
	if _, mapped := wb.PayloadMapping["user_id"]; mapped {
		if _, err := jsonpath.Parse(userPath); err != nil {
			return LocalWebhookDefinition{}, fmt.Errorf("payload_mapping user_id: %q is not a supported JSONPath ($, .key and [N] only): %w", userPath, err)
		}
	}
	userScoped := ch.Scope == "user"
	if userScoped && userPath == "" {
		return LocalWebhookDefinition{}, fmt.Errorf("channel %q is scope=user, so a delivery is published under the user it names: payload_mapping must map user_id", wb.Channel)
	}
	return LocalWebhookDefinition{
		Auth:       config.WebhookAuth(wb.Auth),
		Channel:    local,
		UserIDPath: userPath,
		UserScoped: userScoped,
	}, nil
}

// checkLocalWebhooks refuses, at create and fork, a definition declaring a
// webhook its author may not open or that could not work. A fork is judged on
// what it writes: the webhooks it carries over from its parent are judged
// under the forker's authority like the ones it adds.
func checkLocalWebhooks(ctx context.Context, team string, def teamgraph.Definition) error {
	names := def.LocalWebhookNames()
	if len(names) == 0 {
		return nil
	}
	policy := tools.WebhookDefPolicy(ctx)
	// The WebhookDef tool's own scope check, on the name the webhook is shown
	// by. It reads only the policy, so a zero tool serves.
	var webhookDefs WebhookDef
	for _, name := range names {
		if err := webhookDefs.checkScopeForName(policy, teamgraph.QualifiedLocalName(team, name)); err != nil {
			return fmt.Errorf("local.webhooks[%q]: a team's own webhook opens an endpoint anyone can POST to, "+
				"so declaring one takes the authority to create a webhook definition: %w", name, err)
		}
	}
	// One segment of the URL the webhook is reached at.
	if err := teamgraph.ValidateName(team); err != nil {
		return fmt.Errorf("local: a team that declares its own webhooks needs a name of one segment "+
			"(A-Z a-z 0-9 _ -), because each is reached under the team's name: %w", err)
	}
	for _, name := range names {
		if _, err := LocalWebhookOf(def, name); err != nil {
			return fmt.Errorf("local.webhooks[%q]: %w", name, err)
		}
	}
	return nil
}
