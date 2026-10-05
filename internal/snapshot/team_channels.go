package snapshot

import (
	"context"
	"fmt"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A team's own channel keeps its messages and cursors under
// "_team/<team>/<name>" in the team's tenant, and deleting the team drops
// them (TeamDefDelete). A snapshot carries those rows like any channel's, so a
// restore of an older snapshot after the team was deleted, or into a host
// that never had the team, would write rows nothing can reach — no one
// outside the team addresses the prefix, and lists hide it — and that a team
// created later under the name would inherit.
//
// So such a row restores only when its tenant holds a version of that team,
// retired or not (what TeamDefDelete removes), counting what the restore
// itself just wrote: team_defs restore before channels. A restored team gets
// its own channels' rows back. The skipped rows are counted and reported once
// per team; a later restore into a host that has the team brings them back,
// since nothing was written for them.

// teamChannelGate decides, per row, whether a channel row may restore. One
// store read per (tenant, team), cached for the restore.
type teamChannelGate struct {
	s         store.Store
	defined   map[[2]string]bool // {tenant, team} → the tenant holds that team
	unknown   map[[2]string]bool // {tenant, team} → the check failed
	skipped   map[string]int     // qualified team → rows skipped, team not here
	malformed int                // rows under the prefix that name no team channel
	unchecked int                // rows skipped because the check failed
	faults    []string
}

func newTeamChannelGate(s store.Store) *teamChannelGate {
	return &teamChannelGate{s: s, defined: map[[2]string]bool{}, unknown: map[[2]string]bool{}, skipped: map[string]int{}}
}

// admit reports whether the row of channel in tenantID restores. A name outside
// the team prefix always does. A name under it restores only when it is a
// well-formed team channel name of a team the tenant holds; a malformed one
// is unreachable by any spelling and never does. A store fault refuses the
// row (a re-restore brings it back) and is reported.
func (g *teamChannelGate) admit(ctx context.Context, tenantID, channel string) bool {
	if !store.IsTeamChannelName(channel) {
		return true
	}
	team, _, ok := store.SplitTeamChannelName(channel)
	if !ok {
		g.malformed++
		return false
	}
	key := [2]string{tenantID, team}
	defined, seen := g.defined[key]
	if !seen && !g.unknown[key] {
		rows, err := g.s.TeamDefListByName(ctx, team)
		if err != nil {
			g.unknown[key] = true
			g.faults = append(g.faults, fmt.Sprintf("channels: could not check team %s for its own channels' rows, so they were not restored: %v", qualifiedName(tenantID, team), err))
		} else {
			for _, r := range rows {
				if r.TenantID == tenantID {
					defined = true
					break
				}
			}
			g.defined[key] = defined
		}
	}
	switch {
	case g.unknown[key]:
		g.unchecked++
	case !defined:
		g.skipped[qualifiedName(tenantID, team)]++
	}
	return defined
}

// report adds one warning per skipped team, sorted, plus any store fault, and
// returns the number of rows skipped.
func (g *teamChannelGate) report(result *RestoreResult) int {
	result.Warnings = append(result.Warnings, g.faults...)
	teams := make([]string, 0, len(g.skipped))
	total := g.malformed + g.unchecked
	if g.malformed > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"channels: %d message/cursor row(s) under the reserved team prefix not restored: the name is not a team channel's", g.malformed))
	}
	for t, n := range g.skipped {
		teams = append(teams, t)
		total += n
	}
	sort.Strings(teams)
	for _, t := range teams {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"channels: %d message/cursor row(s) of team %s's own channels not restored: that team is not defined here", g.skipped[t], t))
	}
	return total
}
