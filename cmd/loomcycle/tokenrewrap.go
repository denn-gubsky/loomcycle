package main

import (
	"fmt"
	"log/slog"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
)

// tokenRewrapLogMaxIDs caps the def_ids listed in a boot line; a large
// pre-pepper deployment would otherwise print thousands.
const tokenRewrapLogMaxIDs = 50

// logTokenHashes reports, at every boot under a derived pepper, the live
// token rows still on an older form (RFC ES F3/C2), and the pepper's
// fingerprint, which `loomcycle tokens rewrap` prints too so an operator can
// check both run with the same LOOMCYCLE_SECRET_KEY. The def_ids of live
// unpeppered rows are listed so a row nobody minted stands out before anyone
// rewraps it; retired or expired ones are only counted, as they can never be
// rewrapped.
func logTokenHashes(rep lchttp.TokenHashReport, pepperFingerprint string) {
	// docs/CONFIGURATION.md quotes this line's text; keep it.
	slog.Info(fmt.Sprintf("auth: operator tokens use a pepper derived from LOOMCYCLE_SECRET_KEY, fingerprint %s; tokens moving to the current pepper at their next login: %d wrapped, %d under a previous LOOMCYCLE_SECRET_KEY",
		pepperFingerprint, rep.Wrapped, rep.Previous),
		"fingerprint", pepperFingerprint, "wrapped", rep.Wrapped, "previous", rep.Previous)
	if rep.OtherKey > 0 {
		slog.Warn("auth: tokens hashed under a LOOMCYCLE_SECRET_KEY that is neither the current nor the previous one; they will not authenticate (set that key as LOOMCYCLE_SECRET_KEY_PREVIOUS, or re-mint them)",
			"tokens", rep.OtherKey)
	}
	if len(rep.Untagged) > 0 {
		// An untagged row may also be from an explicit pepper since removed;
		// the two are indistinguishable, and the rewrap cannot bring that
		// kind back.
		//
		// A def_id comes from the database and a forged row chooses it, so it
		// goes in an attribute, never the message: the handler quotes an
		// attribute value, so it cannot break the log line or carry terminal
		// escapes.
		shown, more := capIDs(rep.Untagged)
		args := []any{"rows", len(rep.Untagged), "def_ids", shown}
		if more > 0 {
			args = append(args, "more_def_ids", more)
		}
		slog.Warn("auth: token rows carry an unpeppered hash and will not authenticate until rewrapped onto the derived pepper — tokens minted before LOOMCYCLE_SECRET_KEY was set, or rows written straight into the database. Review them with `loomcycle tokens rewrap` (lists them, writes nothing), then rewrap only the ones you minted; retire any you did not.",
			args...)
	}
	if rep.UntaggedDead > 0 {
		slog.Info("auth: retired or expired token rows carry an unpeppered hash; they can never log in and are never rewrapped", "rows", rep.UntaggedDead)
	}
}

// capIDs returns at most tokenRewrapLogMaxIDs of ids, and how many it left out.
func capIDs(ids []string) ([]string, int) {
	if len(ids) > tokenRewrapLogMaxIDs {
		return ids[:tokenRewrapLogMaxIDs], len(ids) - tokenRewrapLogMaxIDs
	}
	return ids, 0
}
