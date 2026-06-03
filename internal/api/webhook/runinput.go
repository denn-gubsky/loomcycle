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

	goal := proj.Fields["goal"]
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
		// UserID + UserTier are projected from the EXTERNAL, attacker-
		// influenceable payload when the operator maps them. UserTier
		// selects the provider/model policy and UserID keys on_complete
		// scope / quota — so a payload value steers routing/scope. The
		// caller (deliverSpawn) rejects an unknown UserTier (parity with the
		// HTTP path); constraining WHICH valid tier a payload may select, or
		// pinning these from the Def, is an operator-config decision (the
		// values are only as trustworthy as the per-Def signing secret).
		UserID:          proj.Fields["user_id"],
		UserTier:        proj.Fields["user_tier"],
		UserCredentials: creds,
		// Metadata is the static, operator-authored def blob → TRUSTED.
		// PayloadMetadata is projected from the inbound body → UNTRUSTED.
		Metadata:        w.Metadata,
		PayloadMetadata: payloadMeta,
		// IdempotencyKey is set by the caller (deliverSpawn) to the
		// delivery id, keeping this builder's signature focused on the
		// Def + projected payload. See RFC H Decision 10 "Layer 2".
	}
}
