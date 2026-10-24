package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RFC FA §3.7c: what happens to a user a gate (or silence) says is gone.
//
//	auth:
//	  lifecycle:
//	    on_removed: { disable: at_once, unbind_after: 168h, erase_after: never, reactivate: operator }
//	    dormant_after: never                # or 90d: no login through any gate for that long
//
// Three stages that never skip: disable at once (sessions end, issued tokens
// retire, nothing is deleted), unbind after unbind_after (gate-managed group
// memberships and the user's own ACL bindings go; the login identities stay so
// a returning person is recognised), erase after erase_after (the RFC CF
// subject erasure) — by default never: an operator runs it.

// Removal policy defaults (RFC FA D13).
const (
	DefaultUnbindAfter = 7 * 24 * time.Hour
	// minDormantAfter keeps a typo ("90m") from disabling every user on the
	// next sweep.
	minDormantAfter = 24 * time.Hour
	maxLifecycleAge = 10 * 365 * 24 * time.Hour
	// ReactivateOperator: only an operator re-enables a removed user.
	ReactivateOperator = "operator"
	// ReactivateAuto: a removed user who signs in again through a trusted
	// gate is re-enabled (refused until a gate's login path applies it).
	ReactivateAuto = "auto"
)

// AuthOnRemovedConfig is auth.lifecycle.on_removed. Durations are Go
// durations or a whole number of days ("7d"); erase_after also takes never.
type AuthOnRemovedConfig struct {
	Disable     string `yaml:"disable,omitempty"`
	UnbindAfter string `yaml:"unbind_after,omitempty"`
	EraseAfter  string `yaml:"erase_after,omitempty"`
	Reactivate  string `yaml:"reactivate,omitempty"`
}

// AuthRemovalPolicy is the resolved removal policy.
type AuthRemovalPolicy struct {
	UnbindAfter time.Duration
	// EraseAfter is measured from the disable; zero means never (an
	// operator runs the erasure).
	EraseAfter time.Duration
	Reactivate string
	// DormantAfter is zero for never.
	DormantAfter time.Duration
}

// RemovalPolicy is the deployment's removal policy, defaulted.
func (p AuthPolicy) RemovalPolicy() AuthRemovalPolicy {
	r := p.Removal
	if r.UnbindAfter == 0 && r.Reactivate == "" {
		r.UnbindAfter = DefaultUnbindAfter
	}
	if r.Reactivate == "" {
		r.Reactivate = ReactivateOperator
	}
	return r
}

// parseLifecycleAge parses a Go duration or "<n>d". never → 0 when
// allowNever.
func parseLifecycleAge(where, v string, allowNever bool) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "never" {
		if !allowNever {
			return 0, fmt.Errorf("%s: never is not allowed here", where)
		}
		return 0, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("%s: %q is not a duration (a Go duration such as 168h, or whole days such as 7d)", where, v)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, fmt.Errorf("%s: %q is not a duration (a Go duration such as 168h, or whole days such as 7d)", where, v)
		}
	}
	if d < 0 || d > maxLifecycleAge {
		return 0, fmt.Errorf("%s: must be between 0 and %s", where, maxLifecycleAge)
	}
	return d, nil
}

// resolveRemoval validates auth.lifecycle.on_removed and dormant_after.
// erase_after needs LOOMCYCLE_AUDIT_LOG_PATH: an erasure is refused unless
// who erased whom can be recorded, so a policy that schedules one without an
// audit sink would only fail every day.
func resolveRemoval(c *Config) (AuthRemovalPolicy, error) {
	o := c.Auth.Lifecycle.OnRemoved
	r := AuthRemovalPolicy{UnbindAfter: DefaultUnbindAfter, Reactivate: ReactivateOperator}
	switch strings.TrimSpace(o.Disable) {
	case "", "at_once":
	default:
		return r, fmt.Errorf("auth.lifecycle.on_removed.disable: %q (the only value is at_once — a removed user is disabled immediately)", o.Disable)
	}
	var err error
	if strings.TrimSpace(o.UnbindAfter) != "" {
		if r.UnbindAfter, err = parseLifecycleAge("auth.lifecycle.on_removed.unbind_after", o.UnbindAfter, false); err != nil {
			return r, err
		}
	}
	if e := strings.TrimSpace(o.EraseAfter); e != "" {
		if r.EraseAfter, err = parseLifecycleAge("auth.lifecycle.on_removed.erase_after", e, true); err != nil {
			return r, err
		}
		if e != "never" && r.EraseAfter == 0 {
			return r, fmt.Errorf("auth.lifecycle.on_removed.erase_after: must be positive, or never")
		}
		if r.EraseAfter > 0 && r.EraseAfter < r.UnbindAfter {
			return r, fmt.Errorf("auth.lifecycle.on_removed.erase_after (%s) is before unbind_after (%s): the stages never skip", r.EraseAfter, r.UnbindAfter)
		}
		if r.EraseAfter > 0 && strings.TrimSpace(c.Env.AuditLogPath) == "" {
			return r, fmt.Errorf("auth.lifecycle.on_removed.erase_after: an erasure is recorded or not done; set LOOMCYCLE_AUDIT_LOG_PATH")
		}
	}
	switch strings.TrimSpace(o.Reactivate) {
	case "", ReactivateOperator:
	case ReactivateAuto:
		return r, fmt.Errorf("auth.lifecycle.on_removed.reactivate: auto is not supported yet: no gate's login path in this release re-enables a removed user")
	default:
		return r, fmt.Errorf("auth.lifecycle.on_removed.reactivate: %q (want operator)", o.Reactivate)
	}
	if d := strings.TrimSpace(c.Auth.Lifecycle.DormantAfter); d != "" {
		if r.DormantAfter, err = parseLifecycleAge("auth.lifecycle.dormant_after", d, true); err != nil {
			return r, err
		}
		if d != "never" && r.DormantAfter < minDormantAfter {
			return r, fmt.Errorf("auth.lifecycle.dormant_after: at least %s, or never", minDormantAfter)
		}
	}
	return r, nil
}
