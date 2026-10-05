package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"go.opentelemetry.io/otel/trace"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// verdictRejectedTeamStart is the triage verdict for a team delivery that
// passed the trust boundary and then started no walk.
const verdictRejectedTeamStart = "rejected_team_start"

// errTeamVarRefused marks a projected variable value the delivery may not
// carry. It never reaches the sender; the receiver logs the wrapped reason.
var errTeamVarRefused = errors.New("projected variable value refused")

// buildTeamWalkInput converts a resolved delivery=team Def + a verified body
// into the walk a delivery asks for. It is buildRunInput's twin, and draws the
// identity from the same places.
//
// What comes from the PAYLOAD, and is therefore untrusted:
//   - the walk's input: the raw body, already bounded by body_size_limit_bytes;
//   - each variable's VALUE, projected by the def's JSONPath through the same
//     projector payload_mapping uses. A value is checked here, at the trust
//     boundary, by the rule a value typed into a definition is held to — so one
//     carrying {{ or }} or over the size bound refuses the delivery instead of
//     reaching a prompt. An empty projection (an absent path, a null, an empty
//     string) is "not supplied", as it is for the other projection targets:
//     the team's default applies;
//   - the user the walk is attributed to, when the def maps user_id.
//
// What comes from the DEF and nowhere else: which team, which variable NAMES
// may be set, the tenant the walk runs in and looks its team up in, and the
// author's captured restriction bits. There is no payload path to any of them.
func buildTeamWalkInput(w config.Webhook, proj projectResult, body []byte) (runner.TeamWalkInput, error) {
	varsProj, err := projectPayload(w.Vars, body)
	if err != nil {
		return runner.TeamWalkInput{}, err
	}
	var vars map[string]string
	for _, name := range sortedKeys(varsProj.Fields) {
		val := varsProj.Fields[name]
		if val == "" {
			continue
		}
		if verr := teamgraph.CheckVarValue(val); verr != nil {
			return runner.TeamWalkInput{}, fmt.Errorf("%w: vars %q: %v", errTeamVarRefused, name, verr)
		}
		if vars == nil {
			vars = make(map[string]string, len(varsProj.Fields))
		}
		vars[name] = val
	}
	return runner.TeamWalkInput{
		Team:  w.Team,
		Vars:  vars,
		Input: string(body),
		// SECURITY: tenant and both bits come from the STATIC def `w` ONLY —
		// the fields, and the rule, buildRunInput follows for a spawned run.
		TenantID:              w.TenantID,
		UserID:                proj.Fields["user_id"],
		OperatorKeyRestricted: w.OperatorKeyRestricted,
		Isolated:              w.Isolated,
	}, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// deliverTeam starts a detached walk of the Def's team and answers 202 with
// the walk's run id — the shape an async spawn answers, since a walk is a run.
//
// It runs after the shared front half, so the body is already authenticated,
// deduplicated and rate-limited. Nothing here widens that: a delivery that
// starts no walk is not recorded as accepted, so the sender's retry is
// processed rather than dropped.
func (rec *Receiver) deliverTeam(ctx context.Context, w http.ResponseWriter, span trace.Span, name, whKey, did string, dk deliveryKeys, wd config.Webhook, proj projectResult, body []byte) {
	if rec.teams == nil {
		rec.finish(span, whKey, did, "rejected_no_runner", "")
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
		return
	}
	in, err := buildTeamWalkInput(wd, proj, body)
	if err != nil {
		if errors.Is(err, errTeamVarRefused) {
			rec.finish(span, whKey, did, verdictRejectedTeamStart, "")
			rec.logf("webhook %q: team delivery refused: %v", name, err)
			writeError(w, http.StatusBadRequest, "invalid_run", "")
			return
		}
		rec.finish(span, whKey, did, "rejected_mapping", "")
		rec.logf("webhook %q: vars mapping failed: %v", name, err)
		writeError(w, http.StatusBadRequest, "bad_mapping", "")
		return
	}
	// Durable dedup, as for a spawned run: the walk's run row carries the
	// webhook-scoped delivery keys, so a redelivery that outlived the in-memory
	// guard (or landed on another replica) finds the walk it already started.
	in.IdempotencyKey = dk.key
	in.DeliveryAltKey = dk.alt
	if existing, ok := rec.priorTeamWalk(ctx, dk, did); ok {
		rec.dedup.recordDuplicate(dk)
		rec.finish(span, whKey, did, verdictAccepted, existing.ID)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"webhook_name": name,
			"delivery_id":  did,
			"run_id":       existing.ID,
			"deduped":      "true",
		})
		return
	}

	// A detached ctx, as the async spawn uses: an accepted delivery's walk
	// must not die with the sender's connection, and nothing on the request is
	// the walk's authority — that is all in `in`.
	runID, err := rec.teams.StartTeamWalk(context.Background(), in)
	if err == nil {
		rec.dedup.recordAccepted(dk)
		rec.finish(span, whKey, did, verdictAccepted, runID)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"webhook_name": name,
			"delivery_id":  did,
			"run_id":       runID,
		})
		return
	}
	// Two deliveries of one event raced past the lookup above and the unique
	// index let one walk exist. This one lost; the delivery WAS handled.
	if errors.Is(err, store.ErrDuplicateIdempotencyKey) {
		resp := map[string]string{"webhook_name": name, "delivery_id": did, "deduped": "true"}
		winner := ""
		if existing, ok := rec.priorTeamWalk(context.Background(), dk, did); ok {
			winner = existing.ID
			resp["run_id"] = winner
		}
		rec.dedup.recordDuplicate(dk)
		rec.finish(span, whKey, did, verdictAccepted, winner)
		writeJSON(w, http.StatusAccepted, resp)
		return
	}
	rec.finish(span, whKey, did, verdictRejectedTeamStart, "")
	rec.logf("webhook %q: team walk did not start: %v", name, err)
	rec.teamStartErrorResponse(w, err)
}

// priorTeamWalk is the Layer-2 lookup for a team delivery: the walk already
// started for it. Only the webhook-scoped keys — a team delivery has no rows
// from before those keys existed, so priorDeliveryRun's bare-id fallback has
// nothing to find here.
func (rec *Receiver) priorTeamWalk(ctx context.Context, dk deliveryKeys, did string) (store.Run, bool) {
	if rec.store == nil || did == "" {
		return store.Run{}, false
	}
	existing, ok, err := rec.store.RunByDeliveryKeys(ctx, []string{dk.key, dk.alt})
	if err != nil || !ok {
		return store.Run{}, false
	}
	return existing, true
}

// teamStartErrorResponse maps a walk start's refusal to the wire status, in
// the vocabulary spawnSetupErrorResponse already answers with.
//
// ONE opaque 400 for everything the def or the payload got wrong. A missing
// team, a retired one, a variable it does not declare and a value it refuses
// all answer `invalid_run` with no detail — the answer an unknown agent gets —
// so a sender learns that the delivery will not work as sent and nothing about
// which teams exist. The reason goes to the operator's log.
func (rec *Receiver) teamStartErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runner.ErrTeamNotStartable), errors.Is(err, runner.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, "invalid_run", "")
	case errors.Is(err, runner.ErrTokenLimitExceeded):
		writeError(w, http.StatusTooManyRequests, "token_limit_exceeded", "")
	default:
		// A paused runtime, or a start that failed: retry with backoff.
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", "")
	}
}
