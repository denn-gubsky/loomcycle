package config

import (
	"fmt"
	"strings"
)

// AuditStoreRequired reports whether the deployment's posture makes the
// store-backed audit stream (RFC FZ Part E) mandatory: a sign-in gate is
// configured, auth.mode is strict, LOOMCYCLE_ACL is not compat, or more than
// one principal is configured. Those are the deployments where who signed
// in, minted a token or changed a rule is a question someone other than the
// single operator will ask, so the stream is on without configuration and
// boot refuses if it cannot open. A single-operator deployment (one
// credential: the legacy token or one config principal, no gate, compat ACL)
// keeps the file-or-nothing behaviour unless LOOMCYCLE_AUDIT_STORE=1.
//
// "More than one principal" counts what config load can see: the legacy
// LOOMCYCLE_AUTH_TOKEN and each config principal whose token is set. More
// than one tenant implies more than one principal. Minted operator tokens
// live in the store; boot adds them (AuditStoreRequiredWith).
//
// Call it after load: it reads the resolved auth policy and principals.
func (c *Config) AuditStoreRequired() bool {
	return c.AuditRequiredReason(0) != ""
}

// AuditStoreRequiredWith is AuditStoreRequired counting storedPrincipals
// more principals: the live operator tokens a store holds at boot.
func (c *Config) AuditStoreRequiredWith(storedPrincipals int) bool {
	return c.AuditRequiredReason(storedPrincipals) != ""
}

// AuditRequiredReason says why the posture requires the store-backed audit
// stream, counting storedPrincipals live minted tokens besides the
// configured credentials; "" when it does not.
func (c *Config) AuditRequiredReason(storedPrincipals int) string {
	if r := auditPostureReason(len(c.ResolvedAuth.Gates) > 0, c.ResolvedAuth.Strict(), c.Env.ACLMode); r != "" {
		return r
	}
	if n := c.ConfiguredPrincipals() + storedPrincipals; n > 1 {
		return fmt.Sprintf("more than one principal is configured (%d: the legacy LOOMCYCLE_AUTH_TOKEN, config principals and live operator tokens)", n)
	}
	return ""
}

// ConfiguredPrincipals counts the credentials config declares: the legacy
// LOOMCYCLE_AUTH_TOKEN, and each config principal whose token is set.
func (c *Config) ConfiguredPrincipals() int {
	n := len(c.ResolvedPrincipals)
	if strings.TrimSpace(c.Env.AuthToken) != "" {
		n++
	}
	return n
}

// AuditStoreEnabled reports whether the store-backed audit stream is on:
// required by the posture, or asked for.
func (c *Config) AuditStoreEnabled() bool {
	return c.Env.AuditStore || c.AuditStoreRequired()
}

// auditStoreOn is AuditStoreEnabled for use during load, before the auth
// policy is resolved: the mode is read from the raw yaml and env values.
// The principals are resolved before the auth policy, so they count here.
func (c *Config) auditStoreOn(gates bool) bool {
	strict := strings.EqualFold(strings.TrimSpace(c.Auth.Mode), AuthModeStrict) ||
		strings.EqualFold(strings.TrimSpace(c.Env.AuthMode), AuthModeStrict)
	return c.Env.AuditStore || auditPostureReason(gates, strict, c.Env.ACLMode) != "" || c.ConfiguredPrincipals() > 1
}

func auditPostureReason(gates, strict bool, aclMode string) string {
	switch {
	case gates:
		return "a sign-in gate is configured"
	case strict:
		return "auth.mode is strict"
	}
	switch strings.ToLower(strings.TrimSpace(aclMode)) {
	case "", "compat":
		return ""
	}
	return "LOOMCYCLE_ACL is " + strings.TrimSpace(aclMode)
}

// checkAuditKey refuses, at load, an audit posture that cannot run: the
// store-backed stream signs its chain, and a retention prune signs the
// anchor it leaves, with a key derived from LOOMCYCLE_SECRET_KEY. Without
// it the server would refuse to start anyway; saying so here lets
// `loomcycle validate` report it, as it does for a password gate.
func (c *Config) checkAuditKey() error {
	if strings.TrimSpace(c.Env.SecretKey) != "" {
		return nil
	}
	if c.AuditStoreEnabled() {
		why := "LOOMCYCLE_AUDIT_STORE=1 is set"
		if r := c.AuditRequiredReason(0); r != "" {
			why = r
		}
		return fmt.Errorf("LOOMCYCLE_SECRET_KEY: the store-backed audit stream is on (%s) and signs its chain "+
			"with a key derived from LOOMCYCLE_SECRET_KEY, which is not set", why)
	}
	if c.Env.AuditRetention > 0 {
		return fmt.Errorf("LOOMCYCLE_AUDIT_RETENTION_DAYS: pruning the audit chain signs the anchor it leaves " +
			"with a key derived from LOOMCYCLE_SECRET_KEY, which is not set")
	}
	return nil
}
