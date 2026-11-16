package teamgraph

import (
	"errors"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// CheckConfigTeams holds the yaml's teams (RFC GS) to what a TeamDef create
// refuses in a definition itself: its keys as written and its graph. Every
// problem in every team is returned in one error, each at its yaml path
// ("teams.review.states[1].handler.agent: …"); nil when there is none.
//
// The config load cannot run this — config sits below this package — so each
// caller that loads a config to use it runs it next, and a broken team stops a
// boot, a reload and `validate` alike, as a broken agent does.
//
// The bodies under local.* are decoded by their own kinds' readers, which sit
// above this package: here only a repeated key in one is caught, and the rest
// when the boot creates the team through the create path.
func CheckConfigTeams(cfg *config.Config) error {
	names := make([]string, 0, len(cfg.Teams))
	for name := range cfg.Teams {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []string
	for _, name := range names {
		raw := cfg.Teams[name].Definition
		def, err := Parse(raw)
		if err != nil {
			problems = append(problems, "teams."+name+": "+err.Error())
			continue
		}
		issues := KeyIssues(raw, &def, nil)
		if len(issues) == 0 {
			issues = ValidateAll(def)
		}
		for _, is := range issues {
			path := "teams." + name
			if is.Path != "" {
				path += "." + is.Path
			}
			problems = append(problems, path+": "+is.Msg)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
