package webhook

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// MountAdmin registers the WH-5b triage routes WRAPPED in adminAuth. Unlike
// Mount (the unauthed receiver POST, which does its own per-WebhookDef auth),
// these endpoints expose operator-only introspection and MUST sit behind the
// global LOOMCYCLE_AUTH_TOKEN bearer — they reveal which deliveries arrived
// and let an operator dry-run a signature, so they are never attacker-facing.
//
// adminAuth is the same recovery+bearer wrapper the HTTP server hands the
// A2A extraMux hook, so these routes get the identical posture as the other
// /v1/_* admin endpoints.
func (rec *Receiver) MountAdmin(reg Registrar, adminAuth func(http.Handler) http.Handler) {
	reg.Handle("GET /v1/_webhooks/{name}/recent-deliveries",
		adminAuth(http.HandlerFunc(rec.handleRecentDeliveries)))
	reg.Handle("POST /v1/_webhooks/{name}/test",
		adminAuth(http.HandlerFunc(rec.handleTest)))
}

// triageTenant is the tenant a triage request resolves its webhook under: the
// ?tenant= focus, or else the caller's own tenant. With the focus the webhook
// is the def POST /v1/_webhooks/{tenant}/{name} resolves to.
//
// Both triage routes are substrate:admin-gated by the HTTP server's /v1/_*
// catch-all, so every caller that reaches here is an admin, the legacy
// bearer, or open mode (no principal), and each of those may focus any
// tenant. The focus is still ignored from anyone else, so widening that gate
// later cannot turn it into a cross-tenant read.
func triageTenant(r *http.Request) string {
	p, hasPrincipal := auth.PrincipalFromContext(r.Context())
	if q := r.URL.Query().Get("tenant"); q != "" {
		if !hasPrincipal || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) {
			return q
		}
	}
	return p.TenantID // "" when there is no principal
}

// handleRecentDeliveries returns the recorded triage entries for a webhook,
// newest-first, capped at min(limit, recentBufferCap, ring length). The
// webhook is the one triageTenant resolves; the ring is keyed the same way as
// the rate-limit bucket (webhookKey), so two tenants' same-named webhooks have
// separate lists.
//
// 404 when the def does not resolve or has never been invoked (so a typo is
// distinguishable from a quiet-but-real webhook). The entries carry ONLY
// non-sensitive fields (delivery id, verdict, received_at, run id) — no
// credentials, no payloads.
func (rec *Receiver) handleRecentDeliveries(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_, owner, ok := lookup.ResolveWebhook(r.Context(), rec.store, rec.cfg, triageTenant(r), name)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_webhook", "")
		return
	}

	limit := recentBufferCap
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > recentBufferCap {
		limit = recentBufferCap
	}

	entries, ok := rec.recentSnapshot(webhookKey(owner, name), limit)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_webhook", "")
		return
	}
	if entries == nil {
		entries = []deliveryRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhook_name": name,
		"deliveries":   entries,
	})
}

// testResult is the dry-run validator response. It NEVER carries credential
// values — only the resolved credential KEY names — so an operator can
// confirm a payload_mapping wires the right keys without leaking the secrets
// the run would receive.
type testResult struct {
	WouldAccept     bool            `json:"would_accept"`
	Verdict         string          `json:"verdict"`
	RunInputPreview runInputPreview `json:"run_input_preview"`
}

// runInputPreview is the non-sensitive projection of the RunInput a real
// delivery would build. CredentialKeys lists the credential map's KEYS only.
type runInputPreview struct {
	Agent          string   `json:"agent"`
	UserID         string   `json:"user_id"`
	Goal           string   `json:"goal"`
	CredentialKeys []string `json:"credential_keys"`
}

// handleTest is the admin dry-run validator. It verifies the signature and
// runs the payload projection against the posted body+headers, then returns
// whether a real delivery WOULD be accepted plus a non-sensitive preview of
// the RunInput. It creates NO run, publishes NO channel message, records NO
// dedup entry, consumes NO rate-limit token, and adds NO triage record — it
// is a pure read-only validation against the resolved WebhookDef.
//
// Credentials are NEVER returned (only their key names). The goal IS returned
// because it is the operator-projected field the dry-run is meant to confirm;
// it is the same value a delivery would put in the run prompt.
func (rec *Receiver) handleTest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// The same def recent-deliveries lists, so an admin dry-runs a
	// tenant-owned webhook with the same ?tenant= focus.
	wd, _, ok := lookup.ResolveWebhook(r.Context(), rec.store, rec.cfg, triageTenant(r), name)
	if !ok || webhookInert(wd) {
		// Same 404 posture as the receiver: an unknown or inert webhook is not
		// addressable for a dry-run either, so the dry-run never reports
		// would_accept for a def a real delivery would refuse.
		writeError(w, http.StatusNotFound, "unknown_webhook", "")
		return
	}

	limit := wd.BodySizeLimitBytes
	if limit <= 0 {
		limit = defaultBodySizeLimitBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "")
		return
	}

	// 1. Authenticate (no token consumed, no dedup recorded). Mirror the
	//    receive path: auth.kind=none skips verification only when the operator
	//    opted in, otherwise the dry-run reports the same refusal a delivery
	//    would get. Map failures to the same verdict labels the receive path uses.
	if strings.EqualFold(strings.TrimSpace(wd.Auth.Kind), "none") {
		if !rec.allowUnauthenticated {
			writeJSON(w, http.StatusOK, testResult{
				WouldAccept:     false,
				Verdict:         verdictUnresolved,
				RunInputPreview: runInputPreview{},
			})
			return
		}
		// none + opted in → fall through to projection (would accept).
	} else if verr := verifySignature(wd.Auth, body, r.Header.Get, rec.envAllowlist, rec.getenv, rec.now); verr != nil {
		verdict := verdictRejectedSig
		var ae *authError
		if errors.As(verr, &ae) && ae.verdict == verdictUnresolved {
			verdict = verdictUnresolved
		}
		// A bad signature short-circuits: no projection preview (the body is
		// unauthenticated). would_accept=false, empty preview.
		writeJSON(w, http.StatusOK, testResult{
			WouldAccept:     false,
			Verdict:         verdict,
			RunInputPreview: runInputPreview{},
		})
		return
	}

	// 2. Project the payload (same allowlisted mapping the receive path uses).
	proj, perr := projectPayload(wd.PayloadMapping, body)
	if perr != nil {
		writeJSON(w, http.StatusOK, testResult{
			WouldAccept:     false,
			Verdict:         "rejected_mapping",
			RunInputPreview: runInputPreview{},
		})
		return
	}

	// 3. Build the RunInput preview (no run is created). We reuse buildRunInput
	//    so the preview matches exactly what a delivery would produce, then
	//    project ONLY the non-sensitive fields — credential VALUES are
	//    dropped, keys retained.
	in := buildRunInput(wd, proj, rec.envAllowlist, rec.getenv, rec.logf)
	keys := make([]string, 0, len(in.UserCredentials))
	for k := range in.UserCredentials {
		keys = append(keys, k)
	}

	writeJSON(w, http.StatusOK, testResult{
		WouldAccept: true,
		Verdict:     verdictAccepted,
		RunInputPreview: runInputPreview{
			Agent:  in.Agent,
			UserID: in.UserID,
			// Mirror what a real delivery sends: the mapped goal, or the raw
			// body when the def declares no `goal` target (F28).
			Goal:           effectiveGoal(wd, proj),
			CredentialKeys: keys,
		},
	})
}
