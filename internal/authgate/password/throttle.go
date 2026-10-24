package password

import "time"

// Throttle arithmetic (RFC FA §6): per-account and per-source exponential
// backoff, and a hard per-account lockout after MaxFailures. The counters live
// in the store (user_credentials.failed_count for an account, auth_throttle for
// a source or a code), so every replica sees the same count.
type Throttle struct {
	// MaxFailures consecutive account failures lock it for Lockout.
	MaxFailures int
	Lockout     time.Duration
}

// Defaults (RFC FA §9: auth.password.max_failures, lockout_minutes).
const (
	DefaultMaxFailures = 10
	DefaultLockout     = 15 * time.Minute
	// accountFree failures pass before an account is delayed at all, so a
	// person who mistypes twice is never made to wait.
	accountFree = 3
	// SourceFree is generous: one office NAT is many people.
	SourceFree = 20
	// SourceWindow is how long a source's failures are remembered.
	SourceWindow = time.Hour
	// CodeMaxFailures wrong submissions against one invite or reset code
	// block it (the code itself is unguessable; this bounds a person or a
	// script hammering a link they hold with bad passwords).
	CodeMaxFailures = 20
	backoffBase     = time.Second
)

// AccountLockFor is the lock after the n-th consecutive failure of an account:
// nothing for the first few, then 1s, 2s, 4s… capped at Lockout, and Lockout
// at MaxFailures.
func (t Throttle) AccountLockFor(n int) time.Duration {
	if n >= t.MaxFailures {
		return t.Lockout
	}
	return Backoff(n, accountFree, t.Lockout)
}

// SourceBlockFor is the same curve for a source address, with more free
// failures and no hard lockout beyond the cap.
func (t Throttle) SourceBlockFor(n int) time.Duration { return Backoff(n, SourceFree, t.Lockout) }

// SourceLoginBlockFor paces one source's failures against one login: the
// account curve without its hard lockout, so a client guessing one account
// slows itself there without locking the account for its owner.
func (t Throttle) SourceLoginBlockFor(n int) time.Duration { return Backoff(n, accountFree, t.Lockout) }

// CodeBlockFor blocks a code for Lockout after CodeMaxFailures.
func (t Throttle) CodeBlockFor(n int) time.Duration {
	if n >= CodeMaxFailures {
		return t.Lockout
	}
	return 0
}

// Backoff is 0 for n <= free, then backoffBase·2^(n-free-1), capped.
func Backoff(n, free int, limit time.Duration) time.Duration {
	if n <= free {
		return 0
	}
	shift := n - free - 1
	if shift > 30 {
		return limit
	}
	d := backoffBase << shift
	if d > limit {
		return limit
	}
	return d
}
