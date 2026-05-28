package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/recipes"
)

// envOverlayRoot is the env var operators set to opt into the
// filesystem overlay. Documented in the help topic and the CLI's
// per-subcommand error messages.
const envOverlayRoot = "LOOMCYCLE_MCP_RECIPES_ROOT"

// reorderFlagsFirst rearranges args so all flag-like tokens (--foo,
// --foo=bar, -x) come BEFORE positional args, which is what Go's
// stdlib flag.Parse requires. Lets operators use the more natural
// CLI form documented in the RFC (`append-to-config NAME --to=FILE`)
// without losing the stdlib parsing.
//
// Limitations: assumes flags don't take a space-separated value
// (i.e. `--to FILE` doesn't work; `--to=FILE` does). Loomcycle's
// CLI convention is `--key=value` throughout, so this is a fit.
func reorderFlagsFirst(args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}

// RunMCPRegistry dispatches the `loomcycle mcp-registry <verb>`
// subcommand family. Seven verbs total: list / show / append-to-config
// (consumption) + add / remove / enable / disable (library management).
//
// Returns:
//
//	0 — success.
//	1 — operational failure (filesystem I/O, parse error on operator yaml).
//	2 — user/config error (unknown verb, missing flags, invalid input).
func RunMCPRegistry(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: loomcycle mcp-registry <verb> [args]")
		fmt.Fprintln(stderr, "Verbs: list, show, append-to-config, add, remove, enable, disable")
		return 2
	}
	switch args[0] {
	case "list":
		return runMCPRegistryList(args[1:], stdout, stderr)
	case "show":
		return runMCPRegistryShow(args[1:], stdout, stderr)
	case "append-to-config":
		return runMCPRegistryAppend(args[1:], stdout, stderr)
	case "add":
		return runMCPRegistryAdd(args[1:], stdout, stderr)
	case "remove":
		return runMCPRegistryRemove(args[1:], stdout, stderr)
	case "enable":
		return runMCPRegistryEnable(args[1:], stdout, stderr)
	case "disable":
		return runMCPRegistryDisable(args[1:], stdout, stderr)
	default:
		return fail(stderr, "unknown mcp-registry verb %q (want list / show / append-to-config / add / remove / enable / disable)", args[0])
	}
}

// loadLibraryForCLI is the small wrapper every verb uses. Centralises
// the env-var read so per-subcommand code doesn't repeat os.Getenv.
// Returns library + overlay-root path (empty when unset).
func loadLibraryForCLI(stderr io.Writer) (*recipes.Library, string, int) {
	overlay := os.Getenv(envOverlayRoot)
	lib, err := recipes.LoadLibrary(overlay)
	if err != nil {
		return nil, "", failOp(stderr, "load recipe library: %v", err)
	}
	return lib, overlay, 0
}

// runMCPRegistryList — `loomcycle mcp-registry list [--format=json] [--include-disabled]`.
func runMCPRegistryList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-registry list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "table", "output format: table | json")
	includeDisabled := fs.Bool("include-disabled", false, "include disabled recipes in the output")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	lib, _, code := loadLibraryForCLI(stderr)
	if code != 0 {
		return code
	}
	names := lib.Enabled()
	if *includeDisabled {
		names = lib.All()
	}

	type row struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Transport   string `json:"transport"`
		Source      string `json:"source"`
		Disabled    bool   `json:"disabled"`
	}
	rows := make([]row, 0, len(names))
	for _, name := range names {
		rec, _, _ := lib.Get(name)
		rows = append(rows, row{
			Name:        name,
			Description: rec.Description(),
			Transport:   rec.HasTransport(),
			Source:      rec.Source,
			Disabled:    lib.IsDisabled(name),
		})
	}

	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return mapStatus(enc.Encode(map[string]any{"recipes": rows}), stderr, "encode json")
	}

	// Table format. Operator-friendly column layout.
	fmt.Fprintf(stdout, "%-22s %-10s %-12s %-50s\n", "NAME", "TRANSPORT", "SOURCE", "DESCRIPTION")
	for _, r := range rows {
		name := r.Name
		if r.Disabled {
			name += " (disabled)"
		}
		desc := r.Description
		if len(desc) > 50 {
			desc = desc[:47] + "..."
		}
		fmt.Fprintf(stdout, "%-22s %-10s %-12s %-50s\n", name, r.Transport, r.Source, desc)
	}
	return 0
}

// runMCPRegistryShow — `loomcycle mcp-registry show <name> [--bundled]`.
func runMCPRegistryShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-registry show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bundledOnly := fs.Bool("bundled", false, "force the bundled version even if an overlay shadows it")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle mcp-registry show <name> [--bundled]")
		return 2
	}
	name := fs.Arg(0)

	overlay := os.Getenv(envOverlayRoot)
	libRoot := overlay
	if *bundledOnly {
		libRoot = "" // load bundled-only
	}
	lib, err := recipes.LoadLibrary(libRoot)
	if err != nil {
		return failOp(stderr, "load recipe library: %v", err)
	}
	rec, _, ok := lib.Get(name)
	if !ok {
		if *bundledOnly {
			return fail(stderr, "recipe %q has no bundled version", name)
		}
		return fail(stderr, "recipe %q not found (try `loomcycle mcp-registry list`)", name)
	}
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return failOp(stderr, "encode JSON: %v", err)
	}
	fmt.Fprintln(stdout, string(out))
	return 0
}

// runMCPRegistryAppend — `loomcycle mcp-registry append-to-config <name> --to=<file> [--force]`.
func runMCPRegistryAppend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-registry append-to-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("to", "", "path to the operator's loomcycle.yaml")
	force := fs.Bool("force", false, "overwrite an existing mcp_servers.<name> entry")
	skipEnvCheck := fs.Bool("skip-env-check", false, "do not refuse on env-var allowlist mismatches (use at your own risk)")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle mcp-registry append-to-config <name> --to=<file> [--force]")
		return 2
	}
	if *target == "" {
		return fail(stderr, "--to=<file> is required")
	}
	name := fs.Arg(0)

	lib, _, code := loadLibraryForCLI(stderr)
	if code != 0 {
		return code
	}
	rec, _, ok := lib.Get(name)
	if !ok {
		return fail(stderr, "recipe %q not found", name)
	}

	// Read existing env-var allowlist from the target's config. Best-
	// effort — if the file can't load yet (e.g. brand-new operator),
	// skip the check + emit a warning. Same posture as `loomcycle
	// validate` when run pre-config-creation.
	var allowlist map[string]bool
	if !*skipEnvCheck {
		allowlist = readEnvAllowlist(*target)
		if allowlist == nil {
			fmt.Fprintln(stderr, "warning: could not load env allowlist from", *target, "— skipping env-var check (use --skip-env-check to silence)")
		}
	}

	out, err := recipes.AppendToConfig(rec, *target, recipes.AppendOptions{
		Force:        *force,
		EnvAllowlist: allowlist,
	})
	if err != nil {
		var exists *recipes.ErrEntryExists
		if errors.As(err, &exists) {
			return fail(stderr, "%v\n  Hint: rerun with --force to overwrite, or remove the existing entry manually.", err)
		}
		var missingEnv *recipes.ErrMissingEnvVars
		if errors.As(err, &missingEnv) {
			fmt.Fprintln(stderr, err.Error())
			fmt.Fprintln(stderr, "  Hint: add the missing names to your `env.allowlist:` block (or LOOMCYCLE_ENV_ALLOWLIST env var) before retrying.")
			fmt.Fprintln(stderr, "  Or override with --skip-env-check if you understand the implications.")
			return 2
		}
		return failOp(stderr, "%v", err)
	}
	if err := os.WriteFile(*target, out, 0o644); err != nil {
		return failOp(stderr, "write %s: %v", *target, err)
	}
	fmt.Fprintf(stdout, "✓ wrote mcp_servers.%s to %s\n", name, *target)
	return 0
}

// runMCPRegistryAdd — `loomcycle mcp-registry add <path> [--name=<n>] [--force]`.
func runMCPRegistryAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-registry add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nameOverride := fs.String("name", "", "override the recipe name (default: basename of <path> without .json)")
	force := fs.Bool("force", false, "overwrite an existing overlay file of the same name")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle mcp-registry add <path> [--name=<n>] [--force]")
		return 2
	}
	path := fs.Arg(0)

	data, err := os.ReadFile(path)
	if err != nil {
		return failOp(stderr, "read %s: %v", path, err)
	}
	name := *nameOverride
	if name == "" {
		base := pathBaseWithoutExt(path)
		name = base
	}

	lib, _, code := loadLibraryForCLI(stderr)
	if code != 0 {
		return code
	}
	rec, err := lib.AddOverlay(name, data, *force)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "✓ added %s → %s\n", name, rec.Path)
	return 0
}

// runMCPRegistryRemove — `loomcycle mcp-registry remove <name>`.
func runMCPRegistryRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-registry remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle mcp-registry remove <name>")
		return 2
	}
	name := fs.Arg(0)
	lib, _, code := loadLibraryForCLI(stderr)
	if code != 0 {
		return code
	}
	if err := lib.RemoveOverlay(name); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "✓ removed %s from overlay\n", name)
	return 0
}

// runMCPRegistryEnable — `loomcycle mcp-registry enable <name>`.
func runMCPRegistryEnable(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-registry enable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle mcp-registry enable <name>")
		return 2
	}
	name := fs.Arg(0)
	lib, _, code := loadLibraryForCLI(stderr)
	if code != 0 {
		return code
	}
	if err := lib.Enable(name); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "✓ enabled %s\n", name)
	return 0
}

// runMCPRegistryDisable — `loomcycle mcp-registry disable <name>`.
func runMCPRegistryDisable(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-registry disable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: loomcycle mcp-registry disable <name>")
		return 2
	}
	name := fs.Arg(0)
	lib, _, code := loadLibraryForCLI(stderr)
	if code != 0 {
		return code
	}
	if err := lib.Disable(name); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "✓ disabled %s\n", name)
	return 0
}

// readEnvAllowlist reads the operator's allowlisted env-var names
// from `<config>.env.allowlist:` (the cfg yaml's field). Returns nil
// when the file can't be parsed (CLI treats nil as "skip the check"
// + emits a warning rather than failing the append). The check is
// a UX-quality safeguard, not a security boundary — `loomcycle run`
// rejects unallowlisted ${LOOMCYCLE_*} expansion at boot regardless.
func readEnvAllowlist(configPath string) map[string]bool {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	// Hand-parse the relevant slice from yaml. Done lightly here
	// because importing internal/config to do it properly would
	// create an import-cycle hazard (config → cli → config).
	out := map[string]bool{}
	lines := strings.Split(string(data), "\n")
	inEnv := false
	inAllowlist := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Top-level env: block.
		if strings.HasPrefix(line, "env:") {
			inEnv = true
			inAllowlist = false
			continue
		}
		// Leave the env: block on next un-indented key.
		if inEnv && len(line) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inEnv = false
			inAllowlist = false
		}
		if inEnv && strings.HasPrefix(trimmed, "allowlist:") {
			inAllowlist = true
			continue
		}
		if inAllowlist {
			if strings.HasPrefix(trimmed, "- ") {
				name := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
				// Strip optional quotes.
				name = strings.Trim(name, `"'`)
				if name != "" {
					out[name] = true
				}
			} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				// Hit a non-list-item line; the allowlist block is over.
				inAllowlist = false
			}
		}
	}
	if len(out) == 0 {
		// No allowlist block found. Caller treats nil as "skip check".
		return nil
	}
	return out
}

// pathBaseWithoutExt strips the directory + ".json" suffix from a
// path. Used to derive a recipe name from a file path operator-side.
func pathBaseWithoutExt(path string) string {
	// Find last separator.
	i := strings.LastIndexAny(path, "/\\")
	base := path
	if i >= 0 {
		base = path[i+1:]
	}
	return strings.TrimSuffix(base, ".json")
}

// mapStatus converts a Go error to a CLI exit code, logging the
// message to stderr when present. Used by JSON-encode call sites
// that can fail despite earlier validation.
func mapStatus(err error, stderr io.Writer, what string) int {
	if err != nil {
		return failOp(stderr, "%s: %v", what, err)
	}
	return 0
}

// sortRowsByName is a tiny helper used by potential extension paths
// (future `list --sort=name`); kept here so tests can pin sort
// stability separately from list output.
//
// nolint:unused // exposed for symmetry; future verbs may need it.
var sortRowsByName = func(rows []string) { sort.Strings(rows) }
