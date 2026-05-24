package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/denn-gubsky/loomcycle/cmd/loomcycle/embedded"
)

// RunInit creates a default loomcycle.yaml + README.md in the
// operator's config directory. Two modes:
//
//   - Non-interactive (default in CI / when stdin isn't a TTY): writes
//     the bundled example yaml verbatim. The operator edits it later.
//   - Interactive (auto-on when stdin is a TTY; --no-interactive forces
//     non-interactive): a minimal 3-question wizard picks the primary
//     provider, the env-var name to read its key from, and the HTTP
//     listen address. Everything else stays as commented sections.
//
// Returns:
//
//	0  — wrote both files; printed env-var suggestions
//	1  — files already exist and --force wasn't passed; or write error
//	2  — flag-parse error
//
// CLAUDE.md security rule §2 is load-bearing here: the wizard prints
// env-var suggestions to stdout for the operator to paste into their
// shell rc themselves. We never write secrets to disk.
func RunInit(args []string, stdout, stderr io.Writer) int {
	return runInitWithStdin(args, os.Stdin, stdout, stderr)
}

// runInitWithStdin is the testable seam — separates the input source
// from os.Stdin so tests can drive the wizard with a bytes.Buffer.
func runInitWithStdin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pathFlag := fs.String("path", "", "destination directory (default: $XDG_CONFIG_HOME/loomcycle/ or ~/.config/loomcycle/)")
	interactive := fs.Bool("interactive", false, "force interactive wizard (default: auto-on when stdin is a TTY)")
	noInteractive := fs.Bool("no-interactive", false, "force non-interactive mode (writes the example yaml verbatim)")
	force := fs.Bool("force", false, "overwrite existing files (default: refuse with a clear error)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: loomcycle init [--path <dir>] [--interactive|--no-interactive] [--force]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Writes loomcycle.yaml + README.md to the operator's config directory.")
		fmt.Fprintln(stderr, "Auto-on wizard when stdin is a TTY; never writes secrets to disk.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	destDir := *pathFlag
	if destDir == "" {
		dir, err := defaultConfigDir()
		if err != nil {
			return fail(stderr, "init: %v", err)
		}
		destDir = dir
	}

	// Auto-detect interactive mode: --interactive forces ON,
	// --no-interactive forces OFF, neither = TTY check.
	wizard := *interactive
	if !*interactive && !*noInteractive {
		wizard = isStdinTTY(stdin)
	}
	if *interactive && *noInteractive {
		return fail(stderr, "init: --interactive and --no-interactive are mutually exclusive")
	}

	yamlPath := filepath.Join(destDir, "loomcycle.yaml")
	docPath := filepath.Join(destDir, "README.md")

	if !*force {
		if existing := firstExistingFile(yamlPath, docPath); existing != "" {
			return failOp(stderr, "init: %s already exists; pass --force to overwrite", existing)
		}
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fail(stderr, "init: create dir %s: %v", destDir, err)
	}

	// The yaml is ALWAYS written verbatim from the bundled example.
	// The wizard's job is to collect operator intent and print
	// next-steps; it never rewrites the yaml. An earlier draft did
	// append a second `agents:` / `env:` block at the bottom, but
	// yaml.v3's last-wins behavior on duplicate top-level keys made
	// that destructive: every example agent except the rewritten
	// default was silently dropped at parse time. The non-mutating
	// path keeps the wizard "informational" and the yaml intact.
	provider, envVar, listenAddr := "anthropic", "ANTHROPIC_API_KEY", "127.0.0.1:8787"
	if wizard {
		var err error
		provider, envVar, listenAddr, err = runWizard(stdin, stdout)
		if err != nil {
			return fail(stderr, "init: wizard: %v", err)
		}
	}

	if err := os.WriteFile(yamlPath, embedded.ExampleYAML(), 0o644); err != nil {
		return fail(stderr, "init: write %s: %v", yamlPath, err)
	}
	if err := os.WriteFile(docPath, embedded.LocalReadme(), 0o644); err != nil {
		return fail(stderr, "init: write %s: %v", docPath, err)
	}

	fmt.Fprintf(stdout, "Wrote %s\n", yamlPath)
	fmt.Fprintf(stdout, "Wrote %s\n", docPath)
	fmt.Fprintln(stdout)
	if wizard {
		fmt.Fprintln(stdout, "Add these to your shell rc (e.g. ~/.zshrc):")
		fmt.Fprintln(stdout, "    export LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32)")
		if provider != "skip" {
			fmt.Fprintf(stdout, "    export %s=<your-key-here>\n", envVar)
		}
		if listenAddr != "127.0.0.1:8787" {
			fmt.Fprintf(stdout, "    export LOOMCYCLE_LISTEN_ADDR=%s\n", listenAddr)
		}
		if provider != "skip" {
			fmt.Fprintln(stdout)
			fmt.Fprintf(stdout, "To pin the default agent to %s, edit your loomcycle.yaml's `agents.default` block:\n", provider)
			fmt.Fprintln(stdout, "    agents:")
			fmt.Fprintln(stdout, "      default:")
			fmt.Fprintf(stdout, "        provider: %s\n", provider)
			fmt.Fprintln(stdout, "        # ... existing fields stay as-is")
		}
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Then read %s and run `loomcycle doctor` to verify.\n", docPath)
	} else {
		fmt.Fprintln(stdout, "Next steps:")
		fmt.Fprintf(stdout, "  1. Read %s for the env-var reference.\n", docPath)
		fmt.Fprintln(stdout, "  2. Set the required environment variables in your shell rc:")
		fmt.Fprintln(stdout, "       export LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32)")
		fmt.Fprintln(stdout, "       export ANTHROPIC_API_KEY=<your-key>   # or OPENAI_API_KEY, DEEPSEEK_API_KEY")
		fmt.Fprintln(stdout, "  3. Run `loomcycle doctor` to verify your setup.")
		fmt.Fprintln(stdout, "  4. Run `loomcycle` to start the server.")
	}
	return 0
}

// runWizard asks the minimal 3-question set. Returns the operator's
// (provider, env-var name, listen address) choices.
func runWizard(stdin io.Reader, stdout io.Writer) (provider, envVar, listenAddr string, err error) {
	reader := bufio.NewReader(stdin)
	fmt.Fprintln(stdout, "loomcycle init — interactive setup")
	fmt.Fprintln(stdout)

	provider, err = prompt(reader, stdout,
		"Which provider's API key do you have? [anthropic / openai / deepseek / skip]",
		"anthropic", validateProvider)
	if err != nil {
		return "", "", "", err
	}

	defaultEnvVar := defaultEnvVarFor(provider)
	envVar = defaultEnvVar
	if provider != "skip" {
		envVar, err = prompt(reader, stdout,
			fmt.Sprintf("Env var to read the key from"),
			defaultEnvVar, nil)
		if err != nil {
			return "", "", "", err
		}
	}

	listenAddr, err = prompt(reader, stdout,
		"HTTP listen address",
		"127.0.0.1:8787", nil)
	if err != nil {
		return "", "", "", err
	}
	fmt.Fprintln(stdout)
	return provider, envVar, listenAddr, nil
}

// prompt reads one line from the reader after writing a "? <q> [<def>]: "
// prompt to stdout. Empty input falls back to def. Optional validator
// re-prompts on invalid input.
func prompt(reader *bufio.Reader, stdout io.Writer, question, def string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(stdout, "? %s [%s]: ", question, def)
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		answer := strings.TrimSpace(line)
		if answer == "" {
			answer = def
		}
		if validate != nil {
			if verr := validate(answer); verr != nil {
				fmt.Fprintf(stdout, "  (%v — try again)\n", verr)
				continue
			}
		}
		return answer, nil
	}
}

func validateProvider(s string) error {
	switch s {
	case "anthropic", "openai", "deepseek", "skip":
		return nil
	}
	return fmt.Errorf("must be one of: anthropic / openai / deepseek / skip")
}

func defaultEnvVarFor(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI_API_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	}
	return "ANTHROPIC_API_KEY"
}

func defaultConfigDir() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "loomcycle"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "loomcycle"), nil
}

func firstExistingFile(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// isStdinTTY reports whether stdin appears to be an interactive
// terminal. The runtime path passes os.Stdin (a *os.File); tests can
// pass a bytes.Buffer which returns false here (good — tests don't
// want auto-wizard).
func isStdinTTY(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
