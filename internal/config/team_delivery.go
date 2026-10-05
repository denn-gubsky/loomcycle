package config

import (
	"fmt"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// A schedule or a webhook with `delivery: team` starts a walk of the named
// team instead of an agent run or a channel publish. The two checks below are
// what a definition can be held to when it is WRITTEN — the yaml load and the
// ScheduleDef / WebhookDef tools both call them, so a static entry and a
// runtime one cannot disagree about what a team delivery looks like.
//
// What they cannot check is whether the team exists or declares these names:
// teams are runtime definitions and may be created after the trigger. That is
// checked when the trigger fires, by the walk's own start.

// CheckScheduleTeamDelivery checks a schedule's team target: the team name,
// each variable name, and each value — a LITERAL, held to the same rule as a
// value a caller passes when starting a walk by hand.
func CheckScheduleTeamDelivery(team string, vars map[string]string) error {
	if err := checkTeamTarget(team, vars); err != nil {
		return err
	}
	for _, name := range sortedVarNames(vars) {
		if err := teamgraph.CheckVarValue(vars[name]); err != nil {
			return fmt.Errorf("vars %q: %w", name, err)
		}
	}
	return nil
}

// CheckWebhookTeamDelivery checks a webhook's team target: the team name, each
// variable name, and each value — a strict-subset JSONPath into the delivery's
// body, the same dialect payload_mapping uses. The projected value is checked
// when a delivery arrives; only the path can be checked here.
func CheckWebhookTeamDelivery(team string, vars map[string]string) error {
	if err := checkTeamTarget(team, vars); err != nil {
		return err
	}
	for _, name := range sortedVarNames(vars) {
		if _, err := jsonpath.Parse(vars[name]); err != nil {
			return fmt.Errorf("vars %q: %q is not a supported JSONPath ($, .key and [N] only): %w", name, vars[name], err)
		}
	}
	return nil
}

func checkTeamTarget(team string, vars map[string]string) error {
	if team == "" {
		return fmt.Errorf("delivery=team requires team")
	}
	if err := teamgraph.ValidateName(team); err != nil {
		return fmt.Errorf("team: %w", err)
	}
	if len(vars) > teamgraph.MaxVars {
		return fmt.Errorf("vars sets %d variables, more than the maximum %d", len(vars), teamgraph.MaxVars)
	}
	for _, name := range sortedVarNames(vars) {
		if err := teamgraph.ValidateVarName(name); err != nil {
			return fmt.Errorf("vars: %w", err)
		}
	}
	return nil
}

// sortedVarNames orders the names so the first refusal is the same one twice.
func sortedVarNames(vars map[string]string) []string {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
