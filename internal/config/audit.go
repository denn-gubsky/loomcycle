package config

import (
	"fmt"
	"strings"
)

// AuditStoreRequired reports whether the deployment's posture makes the
// store-backed audit stream (RFC FZ Part E) mandatory: a sign-in gate is
// configured, auth.mode is strict, or LOOMCYCLE_ACL is not compat. Those are
// the deployments where who signed in, minted a token or changed a rule is a
// question someone other than the single operator will ask, so the stream is
// on without configuration and boot refuses if it cannot open. A
// single-operator deployment (the legacy token, no gate, compat ACL) keeps
// the file-or-nothing behaviour unless LOOMCYCLE_AUDIT_STORE=1.
//
// Call it after load: it reads the resolved auth policy.
func (c *Config) AuditStoreRequired() bool {
	return auditStoreRequired(len(c.ResolvedAuth.Gates) > 0, c.ResolvedAuth.Strict(), c.Env.ACLMode)
}

// AuditStoreEnabled reports whether the store-backed audit stream is on:
// required by the posture, or asked for.
func (c *Config) AuditStoreEnabled() bool {
	return c.Env.AuditStore || c.AuditStoreRequired()
}

// auditStoreOn is AuditStoreEnabled for use during load, before the auth
// policy is resolved: the mode is read from the raw yaml and env values.
func (c *Config) auditStoreOn(gates bool) bool {
	strict := strings.EqualFold(strings.TrimSpace(c.Auth.Mode), AuthModeStrict) ||
		strings.EqualFold(strings.TrimSpace(c.Env.AuthMode), AuthModeStrict)
	return c.Env.AuditStore || auditStoreRequired(gates, strict, c.Env.ACLMode)
}

func auditStoreRequired(gates, strict bool, aclMode string) bool {
	if gates || strict {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(aclMode)) {
	case "", "compat":
		return false
	}
	return true
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
		if c.AuditStoreRequired() {
			why = "a sign-in gate, auth.mode: strict or a LOOMCYCLE_ACL other than compat is configured"
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
