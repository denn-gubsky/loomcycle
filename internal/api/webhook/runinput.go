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

	// 2. Payload overlay: user_credentials.<name> mapping targets win over
	//    the env-resolved value for the same key. An empty projected value
	//    (absent path) does NOT clobber an env-resolved credential — a
	//    missing optional field shouldn't blank out the static fallback.
	for target, val := range proj.Fields {
		if !strings.HasPrefix(target, credentialsPrefix) {
			continue
		}
		name := strings.TrimPrefix(target, credentialsPrefix)
		if name == "" || val == "" {
			continue
		}
		if _, hadEnv := creds[name]; hadEnv && logf != nil {
			logf("webhook: credential key %q has both env + payload source — payload value wins", name)
		}
		creds[name] = val
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
		Agent:           w.Agent,
		Segments:        []loop.PromptSegment{seg},
		UserID:          proj.Fields["user_id"],
		UserTier:        proj.Fields["user_tier"],
		UserCredentials: creds,
		// IdempotencyKey is set by the caller (deliverSpawn) to the
		// delivery id, keeping this builder's signature focused on the
		// Def + projected payload. See RFC H Decision 10 "Layer 2".
	}
}
