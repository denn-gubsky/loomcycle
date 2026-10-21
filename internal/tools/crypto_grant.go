package tools

import (
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// CryptoToolName is the name the Crypto tool registers under. The policy here
// is what that tool reads; the name is config's, so the boot advisories and
// this package agree on it without importing the tool.
const CryptoToolName = config.CryptoToolName

// CryptoPolicyValue is the per-run Crypto grant: which key scopes ("agent",
// "user", "tenant", "run") the run's key-backed crypto operations may use.
//
// DEFAULT-DENY. The zero value — no policy stamped, or an agent with no
// crypto_scopes — grants no key scope at all; the tool still offers its keyless
// operations, and that distinction is the tool's to enforce, not this value's.
// Scopes is always effective: de-duplicated and holding only known values.
type CryptoPolicyValue struct {
	Scopes []string
}

// Allows reports whether the policy grants key scope.
func (p CryptoPolicyValue) Allows(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// EffectiveCryptoScopes resolves an agent's declared crypto_scopes to the
// grant a run holds.
//
// Unlike memory_scopes or evaluation_scopes there is NO default: an unset list
// grants nothing. Every key scope is reach — a key under "user" or "tenant" is
// someone else's secret material as much as the agent's — so nothing about it
// belongs in a default. An explicit DenyAllScopes entry also means none.
//
// Unknown values are DROPPED rather than kept. Config load and the substrate
// write path refuse them first, so this only matters for a definition written
// before a value was retired: kept, it would sit in the policy granting
// nothing, but the tool would have to know not to trust it. Order is the
// declared order; duplicates are removed.
func EffectiveCryptoScopes(declared []string) []string {
	if deniedAll(declared) {
		return nil
	}
	var out []string
	for _, s := range declared {
		if config.IsCryptoScope(s) && !containsScope(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// NarrowCryptoScopes is a sub-agent's crypto grant: the scopes its own
// definition declares that its parent's run also holds, in the child's order.
//
// A child never gains a key scope its parent lacks — the same spawn invariant
// volumes follow — so an agent cannot reach a key by spawning a helper whose
// definition holds more than it does. Both sides are taken through
// EffectiveCryptoScopes first, so a deny-all on either side yields none.
func NarrowCryptoScopes(parent, child []string) []string {
	p := EffectiveCryptoScopes(parent)
	var out []string
	for _, s := range EffectiveCryptoScopes(child) {
		if containsScope(p, s) {
			out = append(out, s)
		}
	}
	return out
}

func containsScope(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
