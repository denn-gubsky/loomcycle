package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// The missing-credential scan (RFC DP §3.1, OQ10).
//
// A restored trigger or card may name a credential the target does not have:
// an env var that is not set here, or a $cred:/$ghapp: reference no credential
// in the def's tenant provides. The source never lists its credentials — there
// is no manifest, so credential names never leave it — so the target derives
// the warnings itself, from the reference text already in the bodies it just
// restored, through two injected yes/no checks. Neither check returns a
// value, and a warning names only the definition, the field and the reference.

// credRefRe matches $cred:<name> and $ghapp:<name> (both resolved from the
// credential store); envRefRe matches ${NAME} for an env-var-shaped NAME, so
// ${run.*} placeholders are not mistaken for env vars. The name grammars
// mirror internal/credential and the header scanner.
var (
	credRefRe = regexp.MustCompile(`\$(cred|ghapp):([A-Za-z0-9_-]{1,128})`)
	envRefRe  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// credScan collects the restored definitions to check. A nil *credScan
// ignores adds, so a caller that does not scan passes nil.
type credScan struct {
	targets []credScanTarget
}

type credScanTarget struct {
	where    string // "webhook_def acme/gh" — no version, so a lineage warns once
	tenantID string // the row's owning tenant; the body's tenant_id wins
	body     json.RawMessage
}

func (c *credScan) add(where, tenantID string, body json.RawMessage) {
	if c == nil {
		return
	}
	c.targets = append(c.targets, credScanTarget{where: where, tenantID: tenantID, body: body})
}

// credScanBody is the subset of a schedule, webhook or server-card body that
// names credentials. Fields a kind does not have decode empty.
type credScanBody struct {
	Agent                  string            `json:"agent"`
	UserID                 string            `json:"user_id"`
	TenantID               string            `json:"tenant_id"`
	UserCredentials        map[string]string `json:"user_credentials"`
	UserCredentialsFromEnv map[string]string `json:"user_credentials_from_env"`
	Auth                   struct {
		Kind             string `json:"kind"`
		SigningSecretEnv string `json:"signing_secret_env"`
		BearerTokenEnv   string `json:"bearer_token_env"`
	} `json:"auth"`
	SignWithKeyEnv string `json:"sign_with_key_env"`
}

// credRef is one reference found in a body.
type credRef struct {
	field string // "user_credentials_from_env.slack"
	env   string // an env var name, or
	cred  string // "$cred:<name>" / "$ghapp:<name>" as written
	name  string // the credential name inside cred
}

// refsOf lists a body's references in a stable order.
func refsOf(b credScanBody) []credRef {
	var out []credRef
	for _, k := range sortedMapKeys(b.UserCredentialsFromEnv) {
		if v := b.UserCredentialsFromEnv[k]; v != "" {
			out = append(out, credRef{field: "user_credentials_from_env." + k, env: v})
		}
	}
	for _, k := range sortedMapKeys(b.UserCredentials) {
		v := b.UserCredentials[k]
		for _, m := range credRefRe.FindAllStringSubmatch(v, -1) {
			out = append(out, credRef{field: "user_credentials." + k, cred: m[0], name: m[2]})
		}
		for _, m := range envRefRe.FindAllStringSubmatch(v, -1) {
			out = append(out, credRef{field: "user_credentials." + k, env: m[1]})
		}
	}
	switch b.Auth.Kind {
	case "", "hmac":
		if b.Auth.SigningSecretEnv != "" {
			out = append(out, credRef{field: "auth.signing_secret_env", env: b.Auth.SigningSecretEnv})
		}
	case "bearer":
		if b.Auth.BearerTokenEnv != "" {
			out = append(out, credRef{field: "auth.bearer_token_env", env: b.Auth.BearerTokenEnv})
		}
	}
	if b.SignWithKeyEnv != "" {
		out = append(out, credRef{field: "sign_with_key_env", env: b.SignWithKeyEnv})
	}
	return out
}

// run checks every collected reference and appends one warning per missing
// (definition, field, reference). A check that is not wired is not guessed
// at: the references it would have checked are counted in one warning.
func (c *credScan) run(ctx context.Context, opts RestoreOptions, result *RestoreResult) {
	if c == nil || len(c.targets) == 0 {
		return
	}
	seen := map[string]bool{}
	uncheckedEnv, uncheckedCred := 0, 0
	for _, t := range c.targets {
		var b credScanBody
		if err := json.Unmarshal(t.body, &b); err != nil {
			continue // the section validator already accepted it; nothing to name
		}
		tenant := b.TenantID
		if tenant == "" {
			tenant = t.tenantID
		}
		for _, r := range refsOf(b) {
			key := t.where + "|" + r.field + "|" + r.env + r.cred
			if seen[key] {
				continue
			}
			seen[key] = true
			switch {
			case r.env != "":
				if opts.EnvSet == nil {
					uncheckedEnv++
					continue
				}
				if !opts.EnvSet(r.env) {
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"missing credential: %s: %s names env var %s, which is not set on this host", t.where, r.field, r.env))
				}
			case r.cred != "":
				if opts.CredentialExists == nil {
					uncheckedCred++
					continue
				}
				if !opts.CredentialExists(ctx, tenant, b.Agent, b.UserID, r.name) {
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"missing credential: %s: %s references %s, which no credential on this host provides for tenant %q",
						t.where, r.field, r.cred, tenant))
				}
			}
		}
	}
	if uncheckedEnv > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"missing-credential scan: %d env var reference(s) in restored definitions were not checked; no env check is wired on this restore", uncheckedEnv))
	}
	if uncheckedCred > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"missing-credential scan: %d credential reference(s) in restored definitions were not checked; no credential check is wired on this restore", uncheckedCred))
	}
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
