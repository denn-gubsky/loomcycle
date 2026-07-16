package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RunAgents handles the `agents` subcommand. Today only `list` is
// implemented; future verbs (e.g. `agents show <name>` to dump one
// agent's full prompt + tool list) hook off the same dispatcher.
//
// Returns:
//
//	0  — printed cleanly.
//	2  — config load / parse failure.
func RunAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: loomcycle agents list [--config <yaml>]")
		return 2
	}
	switch args[0] {
	case "list":
		return runAgentsList(args[1:], stdout, stderr)
	default:
		return fail(stderr, "unknown agents verb %q (want \"list\")", args[0])
	}
}

func runAgentsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "loomcycle.yaml", "path to config YAML")
	jsonOutput := fs.Bool("json", false, "emit JSON instead of the human-readable table")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadLayeredConfig(*cfgPath)
	if err != nil {
		return fail(stderr, "config: %v", err)
	}

	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	if *jsonOutput {
		return runAgentsListJSON(stdout, stderr, cfg, names)
	}

	if len(names) == 0 {
		fmt.Fprintln(stdout, "(no agents configured)")
		return 0
	}

	// Determine column widths so the table doesn't wrap on a typical
	// terminal. Names + providers + models are short; tools is the
	// chatty column we truncate to keep things readable.
	for _, name := range names {
		def := cfg.Agents[name]
		provider, model, pattern, err := cfg.ResolveAgentModel(name)
		if err != nil {
			return fail(stderr, "agent %q: %v", name, err)
		}
		// RFC BG: a model_pattern alias has no concrete model until run time;
		// show the glob so the operator sees what the alias points at.
		if pattern != "" {
			model = pattern
		}
		tools := strings.Join(def.Tools, ",")
		if tools == "" {
			tools = "(none)"
		}
		systemPromptSrc := "inline"
		if def.SystemPromptFile != "" {
			systemPromptSrc = def.SystemPromptFile
		}
		skills := "(none)"
		if len(def.Skills) > 0 {
			skills = strings.Join(def.Skills, ",")
		}
		maxTokens := "default"
		if def.MaxTokens > 0 {
			maxTokens = fmt.Sprintf("%d", def.MaxTokens)
		}

		fmt.Fprintf(stdout, "%s\n", name)
		fmt.Fprintf(stdout, "  provider     : %s\n", provider)
		fmt.Fprintf(stdout, "  model        : %s\n", model)
		fmt.Fprintf(stdout, "  max_tokens   : %s\n", maxTokens)
		fmt.Fprintf(stdout, "  system_prompt: %s\n", systemPromptSrc)
		fmt.Fprintf(stdout, "  tools        : %s\n", tools)
		fmt.Fprintf(stdout, "  skills       : %s\n", skills)
		fmt.Fprintln(stdout)
	}
	return 0
}

func runAgentsListJSON(stdout, stderr io.Writer, cfg *config.Config, names []string) int {
	// Hand-rolled JSON to avoid pulling encoding/json's dependency
	// graph into a thin CLI helper. The shape is small (per-agent
	// object), so the manual builder keeps quoted-string handling
	// honest without inviting injection from agent names.
	fmt.Fprintln(stdout, "[")
	for i, name := range names {
		def := cfg.Agents[name]
		provider, model, pattern, err := cfg.ResolveAgentModel(name)
		if err != nil {
			return fail(stderr, "agent %q: %v", name, err)
		}
		// RFC BG: surface the glob for a model_pattern alias (resolved at run time).
		if pattern != "" {
			model = pattern
		}
		fmt.Fprintln(stdout, "  {")
		fmt.Fprintf(stdout, "    \"name\": %s,\n", jsonString(name))
		fmt.Fprintf(stdout, "    \"provider\": %s,\n", jsonString(provider))
		fmt.Fprintf(stdout, "    \"model\": %s,\n", jsonString(model))
		fmt.Fprintf(stdout, "    \"max_tokens\": %d,\n", def.MaxTokens)
		fmt.Fprintf(stdout, "    \"system_prompt_file\": %s,\n", jsonString(def.SystemPromptFile))
		fmt.Fprintf(stdout, "    \"tools\": %s,\n", jsonStringArray(def.Tools))
		fmt.Fprintf(stdout, "    \"skills\": %s\n", jsonStringArray(def.Skills))
		if i+1 < len(names) {
			fmt.Fprintln(stdout, "  },")
		} else {
			fmt.Fprintln(stdout, "  }")
		}
	}
	fmt.Fprintln(stdout, "]")
	return 0
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func jsonStringArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(jsonString(it))
	}
	b.WriteByte(']')
	return b.String()
}
