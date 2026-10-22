package main

import (
	"fmt"
	"log"
	"strings"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
)

// tokenRewrapLogMaxIDs caps the def_ids listed in a boot line; a large
// pre-pepper deployment would otherwise print thousands.
const tokenRewrapLogMaxIDs = 50

// logTokenHashes reports, at every boot under a derived pepper, the token
// rows still on an older form (RFC ES F3/C2). The def_ids of unpeppered rows
// are listed so a row nobody minted stands out before anyone rewraps it.
func logTokenHashes(rep lchttp.TokenHashReport) {
	log.Printf("auth: tokens moving to the current pepper at their next login: %d wrapped, %d under a previous LOOMCYCLE_SECRET_KEY",
		rep.Wrapped, rep.Previous)
	if rep.OtherKey > 0 {
		log.Printf("auth: WARNING: %d token(s) hashed under a LOOMCYCLE_SECRET_KEY that is neither the current nor the previous one; they will not authenticate (set that key as LOOMCYCLE_SECRET_KEY_PREVIOUS, or re-mint them)", rep.OtherKey)
	}
	if len(rep.Untagged) > 0 {
		// An untagged row may also be from an explicit pepper since removed;
		// the two are indistinguishable, and the rewrap cannot bring that
		// kind back.
		log.Printf("auth: WARNING: %d token row(s) carry an unpeppered hash and will not authenticate until rewrapped onto the derived pepper — tokens minted before LOOMCYCLE_SECRET_KEY was set, or rows written straight into the database. Review them with `loomcycle tokens rewrap` (lists them, writes nothing), then rewrap only the ones you minted; retire any you did not. def_ids: %s",
			len(rep.Untagged), capIDs(rep.Untagged))
	}
}

func capIDs(ids []string) string {
	more := ""
	if len(ids) > tokenRewrapLogMaxIDs {
		more = fmt.Sprintf(" and %d more", len(ids)-tokenRewrapLogMaxIDs)
		ids = ids[:tokenRewrapLogMaxIDs]
	}
	return strings.Join(ids, ", ") + more
}
