package webhook

import (
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// credentialsPrefix is the payload-mapping target namespace that overlays
// resolved credentials. A mapping key "user_credentials.GITHUB_TOKEN"
// projects a value from the request body into the GITHUB_TOKEN credential.
const credentialsPrefix = "user_credentials."

// metadataPrefix is the payload-mapping target namespace that routes a body
// field into the run's UNTRUSTED PayloadMetadata. A mapping key
// "run_metadata.repo" projects $.repository.full_name into payload_metadata.repo,
// which the agent receives fenced (LLM: <run_metadata> block; code-js:
// input.payload_metadata). Distinct from the static, TRUSTED w.Metadata.
const metadataPrefix = "run_metadata."

// collectPrefixed returns the projected fields whose target begins with prefix,
// re-keyed by the name after the prefix. Empty names and empty VALUES are
// skipped: projectPayload records an absent optional path as "" (and in
// MissingKeys), and a missing optional field must not surface as a real
// {name:""} entry — neither a blanked credential nor a phantom metadata key.
// Shared by the user_credentials.* and run_metadata.* projection passes so the
// two namespaces can't drift in scan/skip semantics. Returns nil when empty.
func collectPrefixed(fields map[string]string, prefix string) map[string]string {
	var out map[string]string
	for target, val := range fields {
		if !strings.HasPrefix(target, prefix) {
			continue
		}
		name := strings.TrimPrefix(target, prefix)
		if name == "" || val == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[name] = val
	}
	return out
}

// effectiveGoal returns the text that becomes the agent's single
// `webhook_payload` segment.
//
//   - If the def declares a `goal` target in payload_mapping, that projected
//     value is used verbatim — the operator chose WHICH field is the goal, and
//     it is respected even when it resolves empty (their explicit choice).
//   - Otherwise the agent receives the RAW signed body (F28). Without this, a
//     webhook with no `goal` mapping spawned the agent with an empty payload and
//     it silently no-op'd — the opposite of the GitHub-pattern expectation that
//     "the agent receives the event". projectPayload already validated RawBody as
//     JSON, so it is deterministic, safe text; it is still fenced as an
//     untrusted-block by the caller (the value is attacker-influenceable).
//
// Indexing a nil PayloadMapping is safe (zero value, mapped=false).
func effectiveGoal(w config.Webhook, proj projectResult) string {
	if _, mapped := w.PayloadMapping["goal"]; mapped {
		return proj.Fields["goal"]
	}
	return string(proj.RawBody)
}

// buildRunInput converts a resolved webhook Def + projected payload into the
// RunInput the runner consumes. It MIRRORS the scheduler's buildRunInput
// credential pattern (env-allowlist gate over user_credentials_from_env)
// and ADDS the webhook-specific rule: a payload_mapping target under
// `user_credentials.<name>` overlays the env-resolved value, and the
// PAYLOAD WINS. Rationale: the webhook payload is the live, per-delivery
// secret (e.g. a per-call token the sender minted), whereas the env var is
// the static fallback; the more specific source should override.
//
// getenv is injected so tests drive the env branches deterministically.
// logf may be nil.
func buildRunInput(w config.Webhook, proj projectResult, envAllowlist map[string]bool, getenv func(string) string, logf func(format string, args ...any)) runner.RunInput {
	creds := make(map[string]string)

	// 1. Env-resolved credentials, gated by the allowlist (scheduler parity).
	for k, envName := range w.UserCredentialsFromEnv {
		if !envAllowlist[envName] {
			if logf != nil {
				logf("webhook: env var %q for credential key %q not in allowlist — skipping", envName, k)
			}
			continue
		}
		v := getenv(envName)
		if v == "" {
			if logf != nil {
				logf("webhook: env var %q for credential key %q is empty — skipping", envName, k)
			}
			continue
		}
		creds[k] = v
	}

	// 1b. Fork-time explicit credentials (RFC F, ScheduleDef parity) override
	//     the env-resolved value. The payload overlay (step 2) still wins over
	//     these, preserving "the live per-delivery token is most specific".
	//     Precedence: env-resolved < fork-time user_credentials < payload.
	for k, v := range w.UserCredentials {
		if v == "" {
			continue
		}
		creds[k] = v
	}

	// 2. Payload overlay: user_credentials.<name> mapping targets win over the
	//    env-resolved + fork-time value for the same key. collectPrefixed skips
	//    empty projected values, so an absent optional path does NOT clobber a
	//    prior source (a missing optional field shouldn't blank the fallback).
	for name, val := range collectPrefixed(proj.Fields, credentialsPrefix) {
		if _, had := creds[name]; had && logf != nil {
			logf("webhook: credential key %q has both a non-payload source + payload source — payload value wins", name)
		}
		creds[name] = val
	}

	// Non-secret payload metadata: payload_mapping targets under
	// `run_metadata.<name>` are projected from the (signed) inbound body —
	// attacker-influenceable, so UNTRUSTED. Collected from the otherwise-
	// discarded projection targets (empties skipped, like credentials) and
	// fenced downstream.
	var payloadMeta map[string]any
	if m := collectPrefixed(proj.Fields, metadataPrefix); len(m) > 0 {
		payloadMeta = make(map[string]any, len(m))
		for name, val := range m {
			payloadMeta[name] = val
		}
	}

	goal := effectiveGoal(w, proj)
	seg := loop.PromptSegment{
		Role: "user",
		Content: []loop.PromptContentBlock{
			// The goal is projected from an EXTERNAL, attacker-influenceable
			// webhook payload (a PR title, an issue body, a commit message).
			// Wrap it as an untrusted-block so the loop fences it in
			// <untrusted> tags before the model sees it — the operator's
			// payload_mapping chooses WHICH field becomes the goal, but the
			// VALUE is not trusted. (Contrast the scheduler, which uses
			// trusted-text: its prompt is operator-authored static text, not
			// live external input.) This is the prompt-injection boundary for
			// webhook-triggered runs.
			{Type: "untrusted-block", Kind: "webhook_payload", Text: goal},
		},
	}

	return runner.RunInput{
		Agent:    w.Agent,
		Segments: []loop.PromptSegment{seg},
		// UserID is projected from the EXTERNAL, attacker-influenceable payload
		// when the operator maps it (attribution / on_complete scope).
		//
		// UserTier is PINNED from the STATIC def `w` — NEVER from the payload:
		// it selects the provider/model policy (cost), so a signed sender must
		// not be able to pick the most expensive tier. A payload-mapped
		// user_tier is deliberately ignored here (load-time warning flags it).
		// deliverSpawn still rejects an unknown w.UserTier (typo guard).
		UserID:          proj.Fields["user_id"],
		UserTier:        w.UserTier,
		UserCredentials: creds,
		// Metadata is the static, operator-authored def blob → TRUSTED.
		// PayloadMetadata is projected from the inbound body → UNTRUSTED.
		Metadata:        w.Metadata,
		PayloadMetadata: payloadMeta,
		// TenantID picks which tenant's agents/skills/MCP resolve and whose
		// memory/runs the spawned run is isolated to. SECURITY: it comes from
		// the STATIC def `w` ONLY — NEVER from proj.Fields (the signed-but-
		// attacker-influenceable payload). There is deliberately no
		// payload_mapping / run_metadata path for tenant (RFC N follow-up).
		TenantID: w.TenantID,
		// RFC AX: the captured operator-key restriction — no principal is on ctx
		// at delivery time, so the def's captured bit is authority (anti-bypass).
		OperatorKeyRestricted: w.OperatorKeyRestricted,
		// IdempotencyKey is set by the caller (deliverSpawn) to the
		// delivery id, keeping this builder's signature focused on the
		// Def + projected payload. See RFC H Decision 10 "Layer 2".
	}
}
