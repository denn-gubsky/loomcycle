package webhook

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// F23 regression: a LOOMCYCLE_*-prefixed VERIFY secret resolves even when the
// explicit allowlist is empty (the namespace auto-allow, mirroring YAML
// ${LOOMCYCLE_*} expansion). On the unfixed code resolveSecret returned
// verdictUnresolved here — the exact "secret_unresolvable / env_allowlist=0"
// trap that blocked exp4.
func TestResolveSecret_LoomcyclePrefixAutoAllowed(t *testing.T) {
	env := mapGetenv(map[string]string{"LOOMCYCLE_GITEA_WEBHOOK_SECRET": "s3cr3t"})
	got, err := resolveSecret("LOOMCYCLE_GITEA_WEBHOOK_SECRET", map[string]bool{}, env)
	if err != nil {
		t.Fatalf("LOOMCYCLE_* secret should auto-resolve with empty allowlist, got %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("resolved value = %q, want s3cr3t", got)
	}
}

// A non-LOOMCYCLE_, non-allowlisted, non-static-declared name stays gated —
// the security floor for runtime-authored defs is preserved.
func TestResolveSecret_NonPrefixedNotDeclaredStillGated(t *testing.T) {
	env := mapGetenv(map[string]string{"GITEA_SECRET": "s3cr3t"})
	if _, err := resolveSecret("GITEA_SECRET", map[string]bool{}, env); err == nil {
		t.Fatal("non-prefixed secret not in allowlist should be unresolvable, got nil error")
	}
	// Allowlisting it explicitly (as BuildEnvAllowlist does for static defs)
	// makes it resolve.
	if _, err := resolveSecret("GITEA_SECRET", map[string]bool{"GITEA_SECRET": true}, env); err != nil {
		t.Fatalf("allowlisted secret should resolve, got %v", err)
	}
}

// Auto-allow does not paper over an unset/empty value — that is still a
// config error the operator must fix.
func TestResolveSecret_LoomcyclePrefixButUnset_StillUnresolved(t *testing.T) {
	env := mapGetenv(map[string]string{}) // LOOMCYCLE_X present in name, absent in env
	if _, err := resolveSecret("LOOMCYCLE_X", map[string]bool{}, env); err == nil {
		t.Fatal("auto-allowed but unset secret should be unresolvable, got nil error")
	}
}

// BuildEnvAllowlist seeds the union of the two explicit knobs AND every
// secret/credential env NAME declared by a STATIC webhook — so an
// operator-authored webhook's own (even non-LOOMCYCLE_) secret resolves
// without a separate allowlist entry.
func TestBuildEnvAllowlist_SeedsStaticDeclaredAndMergesKnobs(t *testing.T) {
	cfg := &config.Config{
		Webhooks: map[string]config.Webhook{
			"gitea": {
				Enabled: true,
				Auth:    config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "GITEA_WEBHOOK_SECRET"},
			},
			"bear": {
				Enabled:                true,
				Auth:                   config.WebhookAuth{Kind: "bearer", BearerTokenEnv: "BEARER_TOKEN_ENV"},
				UserCredentialsFromEnv: map[string]string{"api": "GITEA_API_TOKEN"},
			},
		},
	}
	cfg.Env.SchedulerEnvAllowlist = []string{"SCHED_VAR"}
	cfg.Env.WebhooksEnvAllowlist = []string{"WEBHOOK_VAR"}

	allow := BuildEnvAllowlist(cfg)
	for _, want := range []string{
		"GITEA_WEBHOOK_SECRET", // static hmac signing secret (non-prefixed)
		"BEARER_TOKEN_ENV",     // static bearer token
		"GITEA_API_TOKEN",      // static user_credentials_from_env value
		"SCHED_VAR",            // LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST
		"WEBHOOK_VAR",          // LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST
	} {
		if !allow[want] {
			t.Errorf("allowlist missing %q (got %v)", want, allow)
		}
	}
	// A name nobody declared is not present.
	if allow["UNRELATED"] {
		t.Error("allowlist contains an undeclared name")
	}
}

// Q2 (verify-only auto-allow): user_credentials_from_env — the agent-reachable
// path — does NOT get the LOOMCYCLE_* namespace auto-allow. A LOOMCYCLE_-named
// credential env is dropped unless it is in the explicit/seeded allowlist, so a
// runtime-authored webhook cannot inject an arbitrary LOOMCYCLE_* value into a
// run. (Contrast resolveSecret, which DOES auto-allow LOOMCYCLE_* verify
// secrets — those never reach the agent.)
func TestBuildRunInput_CredEnv_NoNamespaceAutoAllow(t *testing.T) {
	w := config.Webhook{
		Agent:                  "a",
		UserCredentialsFromEnv: map[string]string{"tok": "LOOMCYCLE_RUNTIME_CRED"},
	}
	env := mapGetenv(map[string]string{"LOOMCYCLE_RUNTIME_CRED": "value"})
	proj := projectResult{Fields: map[string]string{}}

	// Not allowlisted → dropped despite the LOOMCYCLE_ prefix.
	in := buildRunInput(w, proj, map[string]bool{}, env, nil)
	if _, ok := in.UserCredentials["tok"]; ok {
		t.Fatalf("LOOMCYCLE_-prefixed credential must NOT be auto-allowed for injection; got %v", in.UserCredentials)
	}

	// Explicitly allowlisted (as BuildEnvAllowlist would for a STATIC decl) → used.
	in = buildRunInput(w, proj, map[string]bool{"LOOMCYCLE_RUNTIME_CRED": true}, env, nil)
	if in.UserCredentials["tok"] != "value" {
		t.Fatalf("allowlisted credential should be injected; got %v", in.UserCredentials)
	}
}

// UnresolvableStaticSecrets surfaces exactly the static webhooks that will fail
// every delivery — the boot-time discoverability fix for F23/F24.
func TestUnresolvableStaticSecrets_Warnings(t *testing.T) {
	cfg := &config.Config{
		Webhooks: map[string]config.Webhook{
			"ok":         {Enabled: true, Auth: config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "LOOMCYCLE_OK"}},
			"disabled":   {Enabled: false, Auth: config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "LOOMCYCLE_OK"}},
			"notallowed": {Enabled: true, Auth: config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "GITEA_SECRET"}},
			"unset":      {Enabled: true, Auth: config.WebhookAuth{Kind: "hmac", SigningSecretEnv: "LOOMCYCLE_UNSET"}},
			"nosecret":   {Enabled: true, Auth: config.WebhookAuth{Kind: "hmac"}},
			"none":       {Enabled: true, Auth: config.WebhookAuth{Kind: "none"}},
		},
	}
	allow := BuildEnvAllowlist(cfg) // seeds GITEA_SECRET (static) — so it must NOT warn for "notallowed"
	env := mapGetenv(map[string]string{"LOOMCYCLE_OK": "v", "GITEA_SECRET": "v"})

	warns := UnresolvableStaticSecrets(cfg, allow, env)
	joined := strings.Join(warns, "\n")

	mustContain := []string{"disabled", "unset", "nosecret"}
	for _, name := range mustContain {
		if !strings.Contains(joined, "\""+name+"\"") {
			t.Errorf("expected a warning naming %q; warnings:\n%s", name, joined)
		}
	}
	mustNotContain := []string{"\"ok\"", "\"none\"", "\"notallowed\""}
	for _, frag := range mustNotContain {
		if strings.Contains(joined, frag) {
			t.Errorf("did not expect a warning for %s; warnings:\n%s", frag, joined)
		}
	}
}

// auth.kind=none is gated by AllowUnauthenticated: 503 when off, accepted when
// on. The trusted-network escape hatch must never silently accept by default.
func TestReceiver_NoneKind_GatedByFlag(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"goal":"go"}`)
	wh := config.Webhook{
		Enabled:        true,
		Delivery:       "spawn",
		Agent:          "a",
		Auth:           config.WebhookAuth{Kind: "none"},
		PayloadMapping: map[string]string{"goal": "$.goal"},
	}

	// Flag OFF → 503 unauthenticated_mode_disabled, no run.
	frOff := &fakeRunner{runID: "r", agentID: "a"}
	recOff := New(Deps{
		Cfg:    &config.Config{Webhooks: map[string]config.Webhook{"wh": wh}},
		Runner: frOff,
		Now:    fixedClock(now),
		Getenv: mapGetenv(map[string]string{}),
	})
	if w := doPost(recOff, "wh", body, http.Header{}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("flag off: status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if frOff.wasCalled() {
		t.Fatal("flag off: runner must not be invoked for none-auth")
	}

	// Flag ON → accepted (202), run spawned.
	frOn := &fakeRunner{runID: "r", agentID: "a"}
	recOn := New(Deps{
		Cfg:                  &config.Config{Webhooks: map[string]config.Webhook{"wh": wh}},
		Runner:               frOn,
		AllowUnauthenticated: true,
		Now:                  fixedClock(now),
		Getenv:               mapGetenv(map[string]string{}),
	})
	if w := doPost(recOn, "wh", body, http.Header{}); w.Code != http.StatusAccepted {
		t.Fatalf("flag on: status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if !frOn.wasCalled() {
		t.Fatal("flag on: runner should be invoked for accepted none-auth delivery")
	}
}
