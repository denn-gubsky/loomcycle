// mcp_install.go — `loomcycle mcp install` subcommand.
//
// Prints copy-paste-ready snippets for registering loomcycle as a
// stdio MCP server in Claude Code (via `claude mcp add`) and Claude
// Desktop (via claude_desktop_config.json). Pure read/print; never
// touches the user's Claude config file directly — auto-merging into
// someone else's JSON is a foot-gun (token leak, clobbered other
// servers) and the user's manual paste is one step they only do once.
//
// Three transports supported:
//
//	docker  — `docker run --rm -i denngubsky/loomcycle:latest mcp ...`
//	          Cleanest UX for users who already have Docker; uses -e
//	          flags to pass through API keys from the parent shell.
//	brew    — `loomcycle mcp ...` against a Homebrew-installed binary.
//	          Requires the user's loomcycle.yaml to NOT depend on env
//	          vars (Claude spawns with sparse env) — OR a wrapper.
//	binary  — `<absolute-path>/loomcycle mcp ...` against any binary
//	          on PATH. Same env caveat as brew.
//
// Auto-detect order: docker > brew > binary. Override with --transport.
//
// Output is split into two blocks: the `claude mcp add` one-liner
// (Claude Code's CLI) and the JSON snippet for Claude Desktop. Users
// pick whichever client they're on.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// mcpServerConfig mirrors the per-server shape inside the Claude
// Desktop `mcpServers` object: `{command, args, env?}`. Same shape is
// accepted by Claude Code's `claude mcp add --json` flag.
type mcpServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// RunMCPInstall implements `loomcycle mcp install [flags]`.
func RunMCPInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		transport  string
		configPath string
		serverName string
		jsonOnly   bool
		dockerImg  string
	)
	fs.StringVar(&transport, "transport", "", "transport: docker | brew | binary (default: auto-detect)")
	fs.StringVar(&configPath, "config", "", "path to loomcycle.yaml (default: ~/.config/loomcycle/loomcycle.yaml)")
	fs.StringVar(&serverName, "server-name", "loomcycle", "name for the MCP server in Claude's config")
	fs.BoolVar(&jsonOnly, "json", false, "print only the JSON snippet (no human-readable wrapper)")
	fs.StringVar(&dockerImg, "docker-image", "denngubsky/loomcycle:latest", "Docker image (transport=docker)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Resolve default config path under the user's home.
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return failOp(stderr, "cannot resolve home directory: %v", err)
		}
		configPath = filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml")
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return failOp(stderr, "cannot resolve --config path: %v", err)
	}

	// Auto-detect transport when not pinned by the operator.
	if transport == "" {
		transport = detectTransport()
	}

	cfg, notes, err := buildMCPServerConfig(transport, absConfig, dockerImg)
	if err != nil {
		return fail(stderr, "%v", err)
	}

	if jsonOnly {
		return printJSONOnly(stdout, serverName, cfg)
	}
	return printAll(stdout, serverName, cfg, transport, absConfig, notes)
}

// detectTransport picks the most operator-friendly transport that's
// actually installed on this machine. Docker first because it
// sidesteps env-var passing entirely via `-e KEY` flags; brew next
// because it's the idiomatic install on macOS/Linux; bare binary
// last because it requires the user to know their absolute path.
func detectTransport() string {
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker"
	}
	// Order matters: if `loomcycle` is on PATH AND `brew` is too, we
	// still prefer "brew" because the JSON snippet emits a shorter
	// command (just "loomcycle") and the user is more likely to know
	// they brew-installed it.
	hasBrew := false
	if _, err := exec.LookPath("brew"); err == nil {
		hasBrew = true
	}
	if _, err := exec.LookPath("loomcycle"); err == nil {
		if hasBrew {
			return "brew"
		}
		return "binary"
	}
	// Nothing found — default to docker and let the docs explain how
	// to install Docker. Anything else would lie about what's available.
	return "docker"
}

// buildMCPServerConfig assembles the per-server JSON shape for a
// transport. Returns the config plus any human-readable notes the
// caller should print under it (env-var caveats, install hints).
func buildMCPServerConfig(transport, configPath, dockerImg string) (mcpServerConfig, []string, error) {
	switch transport {
	case "docker":
		mountDir := filepath.Dir(configPath)
		return mcpServerConfig{
			Command: "docker",
			Args: []string{
				"run", "--rm", "-i",
				"-v", mountDir + ":/etc/loomcycle:ro",
				// API keys flow through via -e <KEY> (no value) — Docker
				// pulls them from the parent shell at run time. Operator
				// MUST set these in the shell that launches Claude Code.
				"-e", "ANTHROPIC_API_KEY",
				"-e", "OPENAI_API_KEY",
				"-e", "DEEPSEEK_API_KEY",
				"-e", "GEMINI_API_KEY",
				"-e", "LOOMCYCLE_AUTH_TOKEN",
				dockerImg,
				"mcp", "--config", "/etc/loomcycle/loomcycle.yaml",
			},
		}, []string{
			"Docker passes API keys from your parent shell via -e flags.",
			"Make sure ANTHROPIC_API_KEY (or another provider key) is exported",
			"in the shell that launches Claude Code/Desktop. Add more -e KEY",
			"entries to args[] for any other env vars your loomcycle.yaml uses.",
		}, nil

	case "brew", "binary":
		binPath, err := exec.LookPath("loomcycle")
		if err != nil {
			return mcpServerConfig{}, nil, fmt.Errorf(
				"loomcycle binary not on PATH — install via Homebrew (`brew install denn-gubsky/loomcycle/loomcycle`) or download a release, then re-run; use --transport docker to skip this check",
			)
		}
		return mcpServerConfig{
			Command: binPath,
			Args:    []string{"mcp", "--config", configPath},
		}, []string{
			"Claude spawns MCP servers with a sparse environment. If your",
			"loomcycle.yaml uses ${ANTHROPIC_API_KEY} or similar ${...}",
			"placeholders, populate the \"env\" object below with concrete",
			"values, OR wrap the command with a shell script that sources",
			"your .env file before exec'ing the binary (see loomcycle-mcp.sh",
			"in the repo for a reference wrapper).",
		}, nil

	default:
		return mcpServerConfig{}, nil, fmt.Errorf(
			"unknown transport %q (want: docker | brew | binary)", transport,
		)
	}
}

// printJSONOnly emits a single-key JSON object so the caller can
// pipe it into `jq` or paste it under an existing `mcpServers` block.
func printJSONOnly(w io.Writer, name string, cfg mcpServerConfig) int {
	wrapper := map[string]mcpServerConfig{name: cfg}
	b, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return 1
	}
	fmt.Fprintln(w, string(b))
	return 0
}

// printAll prints the full human-readable output: transport summary,
// Claude Code `claude mcp add --json` one-liner, Claude Desktop JSON
// snippet for manual paste, then the notes block + config file paths.
func printAll(w io.Writer, name string, cfg mcpServerConfig, transport, configPath string, notes []string) int {
	fmt.Fprintf(w, "Transport: %s\n", transport)
	fmt.Fprintf(w, "Config:    %s\n", configPath)
	fmt.Fprintln(w)

	// ── Claude Code (CLI) ───────────────────────────────────────────
	jsonInline, err := json.Marshal(cfg)
	if err != nil {
		return 1
	}
	fmt.Fprintln(w, "── Claude Code (CLI) ───────────────────────────────────────")
	fmt.Fprintln(w, "Register the server (one command):")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  claude mcp add-json %s '%s'\n", name, string(jsonInline))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Verify it loaded:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  claude mcp list")
	fmt.Fprintln(w)

	// ── Claude Desktop (paste into JSON) ────────────────────────────
	fmt.Fprintln(w, "── Claude Desktop ──────────────────────────────────────────")
	fmt.Fprintln(w, "Edit your claude_desktop_config.json:")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", claudeDesktopConfigPath())
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Paste this entry under the top-level \"mcpServers\" object")
	fmt.Fprintln(w, "(or create it if it doesn't exist):")
	fmt.Fprintln(w)

	// Re-marshal as pretty-printed under an indented "mcpServers" key
	// so the user can drop the lines verbatim.
	pretty, _ := json.MarshalIndent(cfg, "    ", "  ")
	fmt.Fprintln(w, "  \"mcpServers\": {")
	fmt.Fprintf(w, "    %q: %s\n", name, string(pretty))
	fmt.Fprintln(w, "  }")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Restart Claude Desktop after saving.")
	fmt.Fprintln(w)

	// ── Notes ───────────────────────────────────────────────────────
	if len(notes) > 0 {
		fmt.Fprintln(w, "── Notes ───────────────────────────────────────────────────")
		for _, line := range notes {
			fmt.Fprintln(w, line)
		}
		fmt.Fprintln(w)
	}

	// ── Other transports ────────────────────────────────────────────
	others := otherTransports(transport)
	if len(others) > 0 {
		fmt.Fprintln(w, "── Other transports ────────────────────────────────────────")
		fmt.Fprintf(w, "Re-run with --transport %s to see the snippet for that path.\n", strings.Join(others, " | "))
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Full docs: docs/MCP_SERVER.md")
	return 0
}

// otherTransports lists the transports NOT chosen so the trailing
// hint shows the operator their alternatives.
func otherTransports(chosen string) []string {
	all := []string{"docker", "brew", "binary"}
	out := make([]string, 0, len(all)-1)
	for _, t := range all {
		if t != chosen {
			out = append(out, t)
		}
	}
	return out
}

// claudeDesktopConfigPath returns the platform-specific default
// location of Claude Desktop's MCP config file. The macOS path is
// authoritative; Linux + Windows are best-effort guesses based on
// upstream Claude Desktop conventions (the app's own docs are the
// source of truth — see docs/MCP_SERVER.md for the live link).
func claudeDesktopConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "<your-home>/Library/Application Support/Claude/claude_desktop_config.json"
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		// Best-effort: %APPDATA%\Claude\claude_desktop_config.json
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json")
	default: // linux + everything else
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	}
}
