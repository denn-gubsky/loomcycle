package sqlshared

import (
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// teamdef_active.promoter_acl_owner (migration 0121) holds three states, which
// both stores read and write through these helpers so they cannot drift:
//
//	NULL  — promoted before the owner was recorded: unknown, not "nobody".
//	''    — recorded, and no subject's set bound the promoter.
//	JSON  — the promoter's authz.SubjectRef.
//
// A promote never writes NULL; only a pointer from before 0121, or a snapshot
// restore of one, carries it.

// TeamDefPromoterACLOwnerArg is the value a promote writes: the owner, or the
// empty string when there is none. Never NULL.
func TeamDefPromoterACLOwnerArg(owner *authz.SubjectRef) (string, error) {
	if owner == nil {
		return "", nil
	}
	b, err := json.Marshal(owner)
	if err != nil {
		return "", fmt.Errorf("teamdef_active promoter_acl_owner: %w", err)
	}
	return string(b), nil
}

// TeamDefPromoterACLOwnerRestoreArg is the value a snapshot restore writes for
// p: NULL for a pointer with no capture at all or one whose owner was never
// recorded, so a restore cannot turn "unknown" into "nobody".
func TeamDefPromoterACLOwnerRestoreArg(p *store.TeamDefPromoter) (any, error) {
	if p == nil || p.ACLOwnerUnrecorded {
		return nil, nil
	}
	return TeamDefPromoterACLOwnerArg(p.ACLOwner)
}

// SetTeamDefPromoterACLOwner reads the column into p (a captured promoter;
// nil is left alone — no capture says nothing about an owner). An unreadable
// value is an error, never "no owner": that would run the walk unnarrowed.
func SetTeamDefPromoterACLOwner(p *store.TeamDefPromoter, valid bool, col string) error {
	if p == nil {
		return nil
	}
	switch {
	case !valid:
		p.ACLOwnerUnrecorded = true
	case col == "":
	default:
		var owner authz.SubjectRef
		if err := json.Unmarshal([]byte(col), &owner); err != nil {
			return fmt.Errorf("teamdef_active promoter_acl_owner is unreadable: %w", err)
		}
		p.ACLOwner = &owner
	}
	return nil
}
