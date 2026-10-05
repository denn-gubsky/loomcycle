package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TeamWebhookResolver is the seam the webhook receiver reaches a team's own
// webhooks through. Like TeamWalkStarter it exists so the receiver does not
// import internal/api/http, which owns the walks that arm those webhooks.
type TeamWebhookResolver interface {
	// ResolveTeamWebhook returns team `team`'s own webhook `name` in tenant,
	// when a running walk of that team armed it — on this instance or, through
	// the walk's lease in the store, on another.
	// false for every other case alike — no such team, no such webhook, no
	// walk running, a retired version — so that an outside caller cannot
	// tell them apart.
	ResolveTeamWebhook(ctx context.Context, tenant, team, name string) (TeamWebhook, bool)
}

// TeamWebhook is one armed team-local webhook, ready to take a delivery.
type TeamWebhook struct {
	// Auth is the webhook's auth block, verified exactly as a WebhookDef's.
	Auth config.WebhookAuth
	// PayloadMapping maps at most "user_id" to a JSONPath into the body.
	PayloadMapping map[string]string
	// Channel names the team's own channel deliveries publish into, as the
	// team writes it ("./events"), for the response.
	Channel string
	// Publish publishes a verified body into that channel, once, attributed
	// to userID (the payload's user_id, "" when it mapped none). Errors a
	// caller branches on: ErrTeamWebhookNeedsUser, ErrTeamWebhookGone.
	Publish func(ctx context.Context, userID string, body json.RawMessage) error
}

// ErrTeamWebhookNeedsUser — the webhook publishes into a user-scoped channel
// and the delivery names no user. The sender's payload is at fault. Wire:
// HTTP 400.
var ErrTeamWebhookNeedsUser = errors.New("team webhook: the delivery names no user for a user-scoped channel")

// ErrTeamWebhookGone — the walk that armed the webhook ended while the
// delivery was being verified. Wire: the receiver's unknown-webhook 404.
var ErrTeamWebhookGone = errors.New("team webhook: no walk of the team is running")
