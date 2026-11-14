package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/denn-gubsky/loomcycle/internal/embedded"
)

// RunPresets implements `loomcycle presets [show <name>]` (RFC AQ §2.3).
//
//	loomcycle presets            — list the embedded presets/bundles + descriptions
//	loomcycle presets show NAME  — print one unit's YAML (read it, or fork it)
//
// These are the introspection surface for the embedded config units that
// LOOMCYCLE_PRESETS / --preset layer as the base of the config stack. Bundles
// (agent + inline skills) are listed alongside presets (pure provider config).
func RunPresets(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return listPresets(stdout)
	}
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return fail(stderr, "usage: loomcycle presets show <name>")
		}
		data, err := embedded.Show(args[1])
		if err != nil {
			return fail(stderr, "%v", err)
		}
		stack, err := embedded.ResolveUnits([]string{args[1]})
		if err != nil {
			return fail(stderr, "%v", err)
		}
		// A unit that includes others is only the top of what it selects, so
		// say what is layered under it. A comment line: the output stays YAML.
		if len(stack) > 1 {
			names := make([]string, len(stack))
			for i, u := range stack {
				names[i] = u.Name
			}
			fmt.Fprintf(stdout, "# %s expands to this stack (base first, the last one wins): %s\n", args[1], strings.Join(names, ", "))
			fmt.Fprintf(stdout, "# Below is %s itself; `loomcycle presets show <name>` prints any of the others.\n\n", args[1])
		}
		stdout.Write(data)
		return 0
	default:
		return fail(stderr, "unknown presets subcommand %q (want: show, or no args to list)", args[0])
	}
}

func listPresets(stdout io.Writer) int {
	units := embedded.Units()
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tINCLUDES\tDESCRIPTION")
	for _, u := range units {
		includes := "-"
		if len(u.Includes) > 0 {
			includes = strings.Join(u.Includes, ",")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Name, u.Kind, includes, u.Description)
	}
	tw.Flush()
	fmt.Fprintf(stdout, "\nSelect with LOOMCYCLE_PRESETS=%s or --preset (layered as the config base, in order).\n", exampleSelection(units))
	return 0
}

// exampleSelection picks a sensible example for the usage line: base + the first
// bundle if present, else just the first unit's name.
func exampleSelection(units []embedded.Unit) string {
	var base, bundle string
	for _, u := range units {
		if u.Name == "base" {
			base = "base"
		}
		if bundle == "" && u.Kind == "bundle" {
			bundle = u.Name
		}
	}
	switch {
	case base != "" && bundle != "":
		return base + "," + bundle
	case base != "":
		return base
	case len(units) > 0:
		return units[0].Name
	default:
		return "base"
	}
}

// RunEnvTemplate implements `loomcycle env-template` (RFC AQ §2.3): print the
// embedded .env.insecure.example (the non-secret env catalogue). Operators pipe
// it to scaffold .env.insecure; RFC AR's install dialog renders its options.
func RunEnvTemplate(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		return fail(stderr, "usage: loomcycle env-template (no arguments)")
	}
	stdout.Write(embedded.EnvTemplate())
	return 0
}
