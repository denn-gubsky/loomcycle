package store

import (
	"context"
	"time"
)

// AuthReplayStore remembers one-time credential values across replicas (RFC
// FA P3). A gate whose credential is valid for minutes — a signed STS request,
// later an external gate's assertion jti — admits each value here once, so a
// captured one is refused by every replica, not only the one that saw it
// first. Keys are hashes chosen by the caller; no row holds a credential.
type AuthReplayStore interface {
	// AuthReplayAdmit records (scope, key) as used until expires. It reports
	// false when the pair is already recorded with an expiry after now. Check
	// and insert are one statement: of two concurrent admits of one pair,
	// exactly one reports true.
	AuthReplayAdmit(ctx context.Context, scope, key string, expires, now time.Time) (bool, error)
	// AuthReplayDeleteExpired removes rows that expired before `before`.
	AuthReplayDeleteExpired(ctx context.Context, before time.Time) (int, error)
}
