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

// logTokenRewrap reports, at every boot under a derived pepper, what the
// token-hash rewrap did (RFC ES F3). The def_ids are listed so a row nobody
// minted stands out.
func logTokenRewrap(rep lchttp.TokenRewrapReport) {
	if len(rep.Rewrapped) > 0 {
		log.Printf("auth: rewrapped %d unpeppered token hash(es) under the derived pepper; def_ids: %s",
			len(rep.Rewrapped), capIDs(rep.Rewrapped))
	}
	log.Printf("auth: tokens moving to the current pepper at their next login: %d wrapped, %d under a previous LOOMCYCLE_SECRET_KEY",
		rep.Wrapped, rep.Previous)
	if rep.OtherKey > 0 {
		log.Printf("auth: WARNING: %d token(s) hashed under a LOOMCYCLE_SECRET_KEY that is neither the current nor the previous one; they will not authenticate (set that key as LOOMCYCLE_SECRET_KEY_PREVIOUS, or re-mint them)", rep.OtherKey)
	}
	if len(rep.Refused) > 0 {
		log.Printf("auth: WARNING: %d token row(s) carry an unpeppered hash written after the first boot on the derived pepper — a server on it never writes one, so they came from a database edit or a replica without LOOMCYCLE_SECRET_KEY; they will not authenticate; def_ids: %s",
			len(rep.Refused), capIDs(rep.Refused))
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
