package otel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
)

// AttrUserHash names a run's user on its spans without naming the subject: an
// exported span leaves the deployment, outside the reach of loomcycle's
// erasure, and the hash keeps a trace backend from holding the raw id. It is
// stable, so one user's traces are still found together; `loomcycle otel
// user-hash <tenant> <user>` computes it for an investigation.
const AttrUserHash = "loomcycle.user_hash"

// userIDPolicy is how a user id appears on a span. A nil key means no secret
// key is configured: the raw id is then emitted, because a hash keyed by
// nothing would be a lookup table, not a hash.
type userIDPolicy struct {
	key []byte
	raw bool
}

// userPolicy is set once by Init, beside the tracer provider and propagator it
// qualifies, which the OTEL API makes process-wide: the run, sub-run, resume
// and consolidation sites all name users without a handle threaded to them.
// Before Init it holds no key, which emits the raw id — spans before Init go
// to the no-op provider anyway.
var userPolicy atomic.Pointer[userIDPolicy]

func setUserIDPolicy(key []byte, raw bool) {
	userPolicy.Store(&userIDPolicy{key: key, raw: raw})
}

// SetUserIDPolicyForTest installs key and the raw opt-in for the duration of a
// test and returns the restore function.
func SetUserIDPolicyForTest(key []byte, raw bool) func() {
	prev := userPolicy.Load()
	setUserIDPolicy(key, raw)
	return func() { userPolicy.Store(prev) }
}

// UserHash is loomcycle.user_hash for user in tenant: the first 16 hex
// characters of HMAC-SHA256(key, tenant + "|" + user). The tenant is part of
// the input because a user id is unique only within its tenant.
func UserHash(key []byte, tenant, user string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(tenant + "|" + user))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

// userAttrs is the span attributes naming user: the hash, plus the raw id when
// the operator opted in (LOOMCYCLE_OTEL_RAW_USER_ID) or no key is configured.
func userAttrs(tenant, user string) []attribute.KeyValue {
	if user == "" {
		return nil
	}
	p := userPolicy.Load()
	if p == nil || len(p.key) == 0 {
		return []attribute.KeyValue{attribute.String(AttrUserID, user)}
	}
	kvs := []attribute.KeyValue{attribute.String(AttrUserHash, UserHash(p.key, tenant, user))}
	if p.raw {
		kvs = append(kvs, attribute.String(AttrUserID, user))
	}
	return kvs
}
