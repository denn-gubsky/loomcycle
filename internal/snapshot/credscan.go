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
//
// The older def sections are scanned too, where their bodies name a
// credential by reference: every header map (an MCP server def's headers, an
// http HookDef body's headers, the inline hook webhooks of an agent, a team or
// a channel), and an MCP server def's url and stdio env, which are expanded
// from the environment when the server is dialed or spawned (defRefs).
//
// An A2A peer's auth.bearer_credential_ref is deliberately not scanned: the
// A2A tool resolves it from the RUN's per-run credentials, which each caller
// supplies with its run — not from the credential store or the environment —
// so nothing on this host could say whether it will resolve.

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
	// agent is the agent a run of this definition resolves credentials
	// for, when the section knows it (an agent def's own name); the body's
	// agent wins.
	agent string
	body  json.RawMessage
	// refs, when set, are the references the section already derived from
	// the body; the body is then not decoded as a trigger.
	refs []credRef
}

func (c *credScan) add(where, tenantID string, body json.RawMessage) {
	if c == nil {
		return
	}
	c.targets = append(c.targets, credScanTarget{where: where, tenantID: tenantID, body: body})
}

// addRefs queues references a section derived itself: a body whose
// credential fields are not a trigger's (a memory backend's api_key_env).
func (c *credScan) addRefs(where, tenantID string, refs []credRef) {
	c.addAgentRefs(where, tenantID, "", refs)
}

// addAgentRefs is addRefs for a body whose $cred: references resolve for a
// known agent — an agent def's own — so an agent-scoped credential is found.
func (c *credScan) addAgentRefs(where, tenantID, agent string, refs []credRef) {
	if c == nil || len(refs) == 0 {
		return
	}
	c.targets = append(c.targets, credScanTarget{where: where, tenantID: tenantID, agent: agent, refs: refs})
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

// defRefs lists the references in a definition body: in every header map it
// carries, wherever nested — any object under a "headers" key whose values
// are all strings, the walk the capture findings make — and in the top-level
// fields named in extra, a string or a map of strings. prefix is the body's
// own path ("hooks" for a channel's hooks column). Keys are walked sorted, so
// the order is stable.
func defRefs(prefix string, body json.RawMessage, extra ...string) []credRef {
	if len(body) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	var out []credRef
	add := func(field, value string) {
		for _, m := range credRefRe.FindAllStringSubmatch(value, -1) {
			out = append(out, credRef{field: field, cred: m[0], name: m[2]})
		}
		for _, m := range envRefRe.FindAllStringSubmatch(value, -1) {
			out = append(out, credRef{field: field, env: m[1]})
		}
	}
	addMap := func(path string, m map[string]string) {
		for _, k := range sortedMapKeys(m) {
			add(path+"."+k, m[k])
		}
	}
	if top, ok := v.(map[string]any); ok {
		for _, k := range extra {
			switch t := top[k].(type) {
			case string:
				add(joinPath(prefix, k), t)
			default:
				if m, ok := stringMap(t); ok {
					addMap(joinPath(prefix, k), m)
				}
			}
		}
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := joinPath(path, k)
				if k == "headers" {
					if m, ok := stringMap(t[k]); ok {
						addMap(p, m)
						continue
					}
				}
				walk(p, t[k])
			}
		case []any:
			for i, e := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk(prefix, v)
	return out
}

// credRef is one reference found in a body.
type credRef struct {
	field string // "user_credentials_from_env.slack"
	env   string // an env var name, or
	cred  string // "$cred:<name>" / "$ghapp:<name>" as written
	name  string // the credential name inside cred
	// tenantOnly marks a reference that resolves only at tenant level (a
	// remote peer's api_key_env), so no per-user credential could supply it.
	tenantOnly bool
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
		refs := t.refs
		if refs == nil {
			if err := json.Unmarshal(t.body, &b); err != nil {
				continue // the section validator already accepted it; nothing to name
			}
			refs = refsOf(b)
		}
		tenant := b.TenantID
		if tenant == "" {
			tenant = t.tenantID
		}
		agent := b.Agent
		if agent == "" {
			agent = t.agent
		}
		for _, r := range refs {
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
				if opts.CredentialExists(ctx, tenant, agent, b.UserID, r.name) {
					continue
				}
				if r.tenantOnly {
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"missing credential: %s: %s references %s, which no tenant-level credential for tenant %q provides on this host",
						t.where, r.field, r.cred, tenant))
					continue
				}
				if b.UserID != "" {
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"missing credential: %s: %s references %s, which no credential on this host provides for tenant %q",
						t.where, r.field, r.cred, tenant))
					continue
				}
				// Most definitions run for whichever user starts the run, so
				// a per-user credential can be the one that resolves — and
				// there is no user to ask about. Say what was checked rather
				// than calling the reference missing.
				levels := "tenant-level"
				if agent != "" {
					levels = "tenant-level or agent-level"
				}
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"missing credential: %s: %s references %s; no %s credential for tenant %q provides it on this host; per-user credentials were not checked",
					t.where, r.field, r.cred, levels, tenant))
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
