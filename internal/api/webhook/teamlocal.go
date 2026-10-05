package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"go.opentelemetry.io/otel/attribute"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// teamlocal.go — the route a team's own webhooks are delivered to.
//
// POST /v1/_teams/{team}/webhooks/{name} (the shared "" tenant's teams) and
// POST /v1/_teams/{tenant}/{team}/webhooks/{name}. A team's own webhook
// answers only while a walk of the team runs and armed it; the resolver says
// whether one has. A delivery is verified and guarded exactly as a WebhookDef
// delivery is — the same functions, in the same order — and then published,
// once, into the team's own channel the webhook names: the raw body, as a
// delivery=channel WebhookDef publishes it.
//
// The tenant is the route's, matched exactly: a team's own primitives belong
// to its tenant and a walk of the team runs only there, so there is no
// fallback to the shared layer as a WebhookDef has.
//
// Nothing is recorded in the recent-deliveries ring: the triage endpoints
// address a WebhookDef by name and cannot name a team's webhook, so a ring
// for one would only hold memory nothing reads. The span carries every
// verdict.

// teamWebhookKey names one team's own webhook in the dedup cache and the
// rate limiter. Its first segment holds a "/", which webhookKey's never does
// (both of its owner forms are query-escaped or the static marker), so no
// WebhookDef — of any tenant or name — shares a team webhook's key, and two
// teams' webhooks of one name do not share one either.
func teamWebhookKey(tenant, team, name string) string {
	return "team/" + url.QueryEscape(tenant) + "/" + url.QueryEscape(team) + ":" + url.QueryEscape(name)
}

// handleTeam receives one delivery to a team's own webhook.
func (rec *Receiver) handleTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenant, team, name := r.PathValue("tenant"), r.PathValue("team"), r.PathValue("name")
	display := team + "/" + name

	ctx, span := lcotel.Tracer().Start(ctx, "webhook.receive")
	defer span.End()
	span.SetAttributes(attribute.String("webhook.name", display), attribute.String("webhook.tenant", tenant))

	// 1. Resolve. No such team, no such webhook, no walk of the team running,
	//    a retired version: one answer for all, byte-identical to a
	//    WebhookDef that does not exist, before the body is read — so the
	//    route tells an unauthenticated caller nothing a WebhookDef's would
	//    not.
	hook, ok := rec.teamHooks.ResolveTeamWebhook(ctx, tenant, team, name)
	if !ok {
		rec.finish(span, "", "", "rejected_unknown", "")
		writeError(w, http.StatusNotFound, "unknown_webhook", "")
		return
	}
	whKey := teamWebhookKey(tenant, team, name)

	// 2. The raw body, under the receiver's default limit.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(defaultBodySizeLimitBytes)))
	if err != nil {
		rec.finish(span, "", "", "rejected_body", "")
		writeError(w, http.StatusBadRequest, "bad_body", "")
		return
	}

	// 3. Verify before parse.
	if !rec.authenticate(w, r, span, "", display, hook.Auth, body) {
		return
	}

	// 4. Replay guard, keyed to this team's webhook.
	did := deliveryID(hook.Auth, body, r.Header.Get)
	dk := newDeliveryKeys(whKey, did, body, signedEnvelope(hook.Auth, r.Header.Get))
	if rec.dedup.seenAny(dk) {
		rec.finish(span, "", did, verdictAcceptedReplay, "")
		rec.logf("team webhook %q: replayed delivery (delivery_id seen within TTL) — idempotent ack", display)
		writeJSON(w, http.StatusOK, map[string]string{"webhook_name": name, "delivery_id": did, "deduped": "true"})
		return
	}

	// 5. The body is JSON, and the user it names when the webhook maps one.
	proj, perr := projectPayload(hook.PayloadMapping, body)
	if perr != nil {
		rec.finish(span, "", did, "rejected_mapping", "")
		rec.logf("team webhook %q: payload mapping failed: %v", display, perr)
		writeError(w, http.StatusBadRequest, "bad_mapping", "")
		return
	}

	// 6. Rate limit, at the receiver's default rate.
	if okRate, retry := rec.limiter.allow(whKey, config.WebhookRateLimit{}); !okRate {
		rec.finish(span, "", did, "rejected_rate", "")
		writeRetryAfter(w, retry)
		return
	}

	// 7. Publish once into the team's channel.
	if err := hook.Publish(ctx, proj.Fields["user_id"], json.RawMessage(body)); err != nil {
		switch {
		case errors.Is(err, runner.ErrTeamWebhookNeedsUser):
			rec.finish(span, "", did, "rejected_mapping", "")
			rec.logf("team webhook %q: %v", display, err)
			writeError(w, http.StatusBadRequest, "bad_mapping", "")
		case errors.Is(err, runner.ErrTeamWebhookGone):
			// The walk ended while the delivery was verified: the webhook is
			// gone, as it would be had the delivery arrived a moment later.
			rec.finish(span, "", did, "rejected_unknown", "")
			writeError(w, http.StatusNotFound, "unknown_webhook", "")
		default:
			rec.finish(span, "", did, "rejected_publish", "")
			rec.logf("team webhook %q: channel publish failed: %v", display, err)
			writeError(w, http.StatusServiceUnavailable, "channel_unavailable", "")
		}
		return
	}
	// Recorded only now, so a delivery refused above stays retryable.
	rec.dedup.recordAccepted(dk)
	rec.finish(span, "", did, verdictAccepted, "")
	writeJSON(w, http.StatusAccepted, map[string]string{
		"webhook_name": name,
		"delivery_id":  did,
		"channel":      hook.Channel,
	})
}
