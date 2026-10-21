package main

import (
	"fmt"
	"log"
	"strings"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
)

// legacyTokenLogMaxIDs caps the def_ids listed in the boot line; a large
// pre-pepper deployment would otherwise print thousands.
const legacyTokenLogMaxIDs = 50

// logLegacyTokenSnapshot reports, at every boot under a derived pepper, how
// many operator tokens are still on a legacy hash (RFC ES D2) — including
// none, which is when LOOMCYCLE_TOKEN_LEGACY_HASH=off is safe. The def_ids
// are listed so a row nobody minted stands out.
func logLegacyTokenSnapshot(snap lchttp.LegacyTokenSnapshot, acceptsUnpeppered bool) {
	n := len(snap.IDs)
	if n == 0 {
		if acceptsUnpeppered {
			log.Printf("auth: 0 legacy tokens — you can set LOOMCYCLE_TOKEN_LEGACY_HASH=off")
		} else {
			log.Printf("auth: 0 legacy tokens")
		}
		return
	}
	ids := snap.IDs
	more := ""
	if len(ids) > legacyTokenLogMaxIDs {
		more = fmt.Sprintf(" and %d more", len(ids)-legacyTokenLogMaxIDs)
		ids = ids[:legacyTokenLogMaxIDs]
	}
	unpeppered := "move to the current pepper at their next login"
	if !acceptsUnpeppered {
		unpeppered = "will NOT authenticate (LOOMCYCLE_TOKEN_LEGACY_HASH=off)"
	}
	log.Printf("auth: %d token(s) on a legacy hash: %d without a pepper (%s), %d under a previous LOOMCYCLE_SECRET_KEY (move at their next login); def_ids: %s%s",
		n, snap.Untagged, unpeppered, n-snap.Untagged, strings.Join(ids, ", "), more)
}
