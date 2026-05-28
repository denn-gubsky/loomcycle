package claudeimport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/recipes"
)

// claudeMCPFile is the shape of .claude/mcp.json and per-project
// .mcp.json — top-level `mcpServers` map (the Claude convention),
// plus an optional `registries` field which has no loomcycle
// equivalent.
type claudeMCPFile struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Registries map[string]json.RawMessage `json:"registries"`
}

// claudeMCPServer is one server entry inside `mcpServers`. We use
// RawMessage for the structurally-rich fields so the recipe-emit
// path can pass them through byte-stably.
type claudeMCPServer struct {
	Command json.RawMessage `json:"command,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Env     json.RawMessage `json:"env,omitempty"`
	URL     json.RawMessage `json:"url,omitempty"`
	Headers json.RawMessage `json:"headers,omitempty"`
}

// envVarRefRe matches ${FOO} env-var references. The walker rewrites
// these through the LOOMCYCLE_* allowlist; references that are already
// LOOMCYCLE_-prefixed pass through unchanged.
var envVarRefRe = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// walkMCP reads .claude/mcp.json AND (if present) the parent
// directory's .mcp.json (the per-project convention). Each top-level
// server entry becomes one MCPEntry in the report.
func walkMCP(claudeRoot string, opts WalkOptions, report *ImportReport) error {
	candidates := []string{
		filepath.Join(claudeRoot, "mcp.json"),
		filepath.Join(filepath.Dir(claudeRoot), ".mcp.json"),
	}
	// Dedup in case .claude/ is the project root.
	seen := map[string]bool{}
	uniq := candidates[:0]
	for _, c := range candidates {
		abs, _ := filepath.Abs(c)
		if !seen[abs] {
			seen[abs] = true
			uniq = append(uniq, c)
		}
	}
	for _, path := range uniq {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("mcp %s: stat: %v", path, err))
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("mcp %s: read: %v", path, err))
			continue
		}
		if err := parseMCPFile(path, raw, opts, report); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("mcp %s: parse: %v", path, err))
		}
	}
	return nil
}

// parseMCPFile parses one .mcp.json document and appends per-server
// entries to the report. The Claude convention wraps the map under
// `mcpServers:`; some operator-authored files use a bare top-level
// map. Try the wrapped form first, fall back to the bare form.
func parseMCPFile(path string, raw []byte, opts WalkOptions, report *ImportReport) error {
	var wrapped claudeMCPFile
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.MCPServers != nil {
		// Wrapped: handle mcpServers map + registries field.
		names := make([]string, 0, len(wrapped.MCPServers))
		for n := range wrapped.MCPServers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			emitMCPServer(name, wrapped.MCPServers[name], path, opts, report)
		}
		// Surface registries: as unmapped per the RFC.
		regNames := make([]string, 0, len(wrapped.Registries))
		for n := range wrapped.Registries {
			regNames = append(regNames, n)
		}
		sort.Strings(regNames)
		for _, n := range regNames {
			report.Unmapped = append(report.Unmapped, &UnmappedField{
				SourcePath: path,
				Field:      fmt.Sprintf("registries[%q]", n),
				Hint: "Loomcycle has no remote-registry surface (RFC C1 sharp edge: " +
					"airgapped-friendly). If a server from this registry is in scope, " +
					"add it manually via mcp_servers: or register at runtime via the " +
					"MCPServerDef tool (POST /v1/_mcpserverdef).",
			})
		}
		return nil
	}
	// Bare top-level server map: try parsing as map[string]json.RawMessage.
	var bare map[string]json.RawMessage
	if err := json.Unmarshal(raw, &bare); err != nil {
		return fmt.Errorf("expected mcp.json shape (mcpServers wrapper OR bare server map): %w", err)
	}
	names := make([]string, 0, len(bare))
	for n := range bare {
		// Skip the registries: key if it accidentally appears at top
		// level (operator-authored variation).
		if n == "registries" || n == "mcpServers" {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		emitMCPServer(name, bare[name], path, opts, report)
	}
	return nil
}

// emitMCPServer materialises one MCPEntry from a single Claude shape
// server. Recipe-library match runs unless opts.NoRecipeMatch.
func emitMCPServer(name string, raw json.RawMessage, sourcePath string,
	opts WalkOptions, report *ImportReport) {
	var srv claudeMCPServer
	if err := json.Unmarshal(raw, &srv); err != nil {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("mcp %s: server %q: %v", sourcePath, name, err))
		return
	}

	// Transport detection: command implies stdio; url implies http.
	transport := "stdio"
	if len(srv.URL) > 0 {
		transport = "http"
	}
	if len(srv.Command) == 0 && len(srv.URL) == 0 {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("mcp %s: server %q: neither command nor url; skipping",
				sourcePath, name))
		return
	}

	entry := &MCPEntry{
		Name:       name,
		SourcePath: sourcePath,
		Transport:  transport,
	}

	// Env-var allowlist rewrites: scan command/args/env/headers for
	// ${FOO} references and rewrite to ${LOOMCYCLE_FOO} where the
	// LOOMCYCLE_-prefixed name is in the operator's allowlist.
	rewrittenEnv, envRewrites := rewriteEnvRefs(srv.Env, opts.EnvAllowlist)
	rewrittenArgs, argRewrites := rewriteEnvRefs(srv.Args, opts.EnvAllowlist)
	rewrittenHeaders, hdrRewrites := rewriteEnvRefs(srv.Headers, opts.EnvAllowlist)
	all := append([]string{}, envRewrites...)
	all = append(all, argRewrites...)
	all = append(all, hdrRewrites...)
	// Dedup + sort.
	seen := map[string]bool{}
	for _, r := range all {
		if !seen[r] {
			seen[r] = true
			entry.EnvVarRewrites = append(entry.EnvVarRewrites, r)
		}
	}
	sort.Strings(entry.EnvVarRewrites)

	// Recipe-library match by package (if enabled and library present).
	var matchedRecipe *recipes.Recipe
	if !opts.NoRecipeMatch && opts.Library != nil {
		probe := &recipes.Recipe{Name: name, Args: rewrittenArgs, URL: srv.URL}
		probePkg := probe.PackageName()
		for _, libName := range opts.Library.Enabled() {
			r, _, ok := opts.Library.Get(libName)
			if !ok {
				continue
			}
			if r.PackageName() == probePkg && probePkg != "" {
				matchedRecipe = r
				entry.RecipeMatch = r.Name
				entry.RecipeSource = r.Source
				break
			}
		}
	}

	// Yaml emission: use the matched recipe under the operator's name
	// (preserves the operator's server-name vocabulary) OR build a
	// literal-port recipe from the parsed fields.
	if !opts.NoYAML {
		var srcRecipe *recipes.Recipe
		if matchedRecipe != nil {
			// Clone the recipe with the operator's chosen name.
			r := *matchedRecipe
			r.Name = name
			srcRecipe = &r
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("mcp %s: REWRITE mcp_servers.%s → C1 recipe %q (%s)",
					sourcePath, name, matchedRecipe.Name, matchedRecipe.Source))
		} else {
			srcRecipe = buildLiteralRecipe(name, transport,
				srv.Command, rewrittenArgs, rewrittenEnv,
				srv.URL, rewrittenHeaders)
		}
		// Render as yaml fragment for the dry-run report.
		entry.YAMLFragment = renderRecipeAsYAMLFragment(srcRecipe)
	}

	// --emit-recipes overlay-write planning: fill EmitRecipePath +
	// EmitRecipeJSON. Actual write happens in the CLI's --write phase.
	if opts.EmitRecipes && opts.OverlayRoot != "" {
		jsonBody, err := buildEmitRecipeJSON(name, transport,
			srv.Command, rewrittenArgs, rewrittenEnv,
			srv.URL, rewrittenHeaders)
		if err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("mcp %s: emit-recipes for %s: %v", sourcePath, name, err))
		} else {
			entry.EmitRecipePath = filepath.Join(opts.OverlayRoot, name+".json")
			entry.EmitRecipeJSON = string(jsonBody)
		}
	}

	report.MCPServers = append(report.MCPServers, entry)
}

// rewriteEnvRefs scans a JSON RawMessage for ${FOO} patterns and
// rewrites them to ${LOOMCYCLE_FOO} (unless they're already
// LOOMCYCLE_-prefixed). Returns the rewritten JSON + the list of
// human-readable "FOO → LOOMCYCLE_FOO" strings for the report.
//
// allowlist is consulted to flag rewrites where the LOOMCYCLE_-name
// isn't in the operator's env allowlist — those entries get a
// "not in env allowlist" suffix.
func rewriteEnvRefs(raw json.RawMessage, allowlist map[string]bool) (json.RawMessage, []string) {
	if len(raw) == 0 {
		return raw, nil
	}
	s := string(raw)
	var rewrites []string
	out := envVarRefRe.ReplaceAllStringFunc(s, func(m string) string {
		// extract the name inside the braces
		inner := m[2 : len(m)-1]
		if strings.HasPrefix(inner, "LOOMCYCLE_") {
			return m // already prefixed
		}
		// Special-case: a small set of substrate substitution patterns
		// that loomcycle handles internally (e.g. ${run.credentials.X})
		// would be model-specific — ${} containing "." or lowercase
		// can't match envVarRefRe (which is upper+underscore), so this
		// branch is only hit by literal env-var refs.
		newName := "LOOMCYCLE_" + inner
		note := ""
		if allowlist != nil && !allowlist[newName] {
			note = " (NOT in env allowlist — add it manually)"
		}
		rewrites = append(rewrites, fmt.Sprintf("${%s} → ${%s}%s", inner, newName, note))
		return "${" + newName + "}"
	})
	return json.RawMessage(out), rewrites
}

// buildLiteralRecipe constructs an in-memory recipes.Recipe from a
// parsed Claude server entry. Used for the no-match path and the
// --emit-recipes overlay write.
func buildLiteralRecipe(name, transport string,
	command, args, env, url, headers json.RawMessage) *recipes.Recipe {
	r := &recipes.Recipe{
		Name:    name,
		Command: command,
		Args:    args,
		Env:     env,
		URL:     url,
		Headers: headers,
		Loomcycle: &recipes.LoomcycleMeta{
			Description:        fmt.Sprintf("Imported from .claude/ (%s).", transport),
			Transport:          transport,
			PoolSize:           2,
			ScheduleCompatible: false,
		},
	}
	return r
}

// buildEmitRecipeJSON serialises a literal-port recipe as JSON
// suitable for writing to $LOOMCYCLE_MCP_RECIPES_ROOT/<name>.json.
// Filled placeholder for the recipe_emit.go commit; the actual
// helper that constructs the JSON lives there.
func buildEmitRecipeJSON(name, transport string,
	command, args, env, url, headers json.RawMessage) ([]byte, error) {
	r := buildLiteralRecipe(name, transport, command, args, env, url, headers)
	return json.MarshalIndent(r, "", "  ")
}

// renderRecipeAsYAMLFragment produces the yaml fragment for one
// recipe — what the operator would see in their `mcp_servers:` block.
// Built from recipeToYAMLValueNode (the public AppendToConfig path
// internally), but here we just need a string for display.
func renderRecipeAsYAMLFragment(r *recipes.Recipe) string {
	// Construct a one-entry mapping {name: recipe} and yaml-marshal.
	// The transport field is required, so we surface a placeholder if
	// the recipe doesn't have one.
	transport := r.HasTransport()
	if transport == "" {
		return fmt.Sprintf("%s: <invalid recipe — neither command nor url>\n", r.Name)
	}
	// Build the value mapping in field order.
	entry := map[string]any{}
	entry["transport"] = transport
	if len(r.Command) > 0 {
		var v any
		_ = json.Unmarshal(r.Command, &v)
		entry["command"] = v
	}
	if len(r.Args) > 0 {
		var v any
		_ = json.Unmarshal(r.Args, &v)
		entry["args"] = v
	}
	if len(r.Env) > 0 {
		var v any
		_ = json.Unmarshal(r.Env, &v)
		entry["env"] = v
	}
	if len(r.URL) > 0 {
		var v any
		_ = json.Unmarshal(r.URL, &v)
		entry["url"] = v
	}
	if len(r.Headers) > 0 {
		var v any
		_ = json.Unmarshal(r.Headers, &v)
		entry["headers"] = v
	}
	if r.Loomcycle != nil && r.Loomcycle.PoolSize > 0 {
		entry["pool_size"] = r.Loomcycle.PoolSize
	}
	doc := map[string]any{r.Name: entry}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Sprintf("%s: <yaml marshal error: %v>\n", r.Name, err)
	}
	return string(out)
}
