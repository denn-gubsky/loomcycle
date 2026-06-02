package cli

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunDoctor runs a sequence of health checks against the operator's
// loomcycle configuration + environment. Designed to be the first
// thing a frustrated operator runs when something doesn't work.
//
// Checks (in order):
//
//  1. Config file discoverable (./loomcycle.yaml → XDG → ~/.config)
//  2. Config parses cleanly
//  3. LOOMCYCLE_AUTH_TOKEN set (WARN when empty — the server boots
//     but every /v1/* request is allowed unauthenticated)
//  4. Per-configured provider: API-key env var set
//  5. Storage backend reachable (sqlite path writable; postgres DSN
//     non-empty)
//  6. HTTP listen address bindable (try-listen-then-close)
//
// Returns 0 when no check FAILed, 1 otherwise. WARNs don't fail the
// overall run.
//
// v0.11.1 scope: env-var + writability checks only. Per-provider
// network probes (Provider.Probe) require constructing the full
// provider registry and would slow the command to network latency;
// deferred to v0.11.2+ when that wiring is extracted.
func RunDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "explicit path to loomcycle.yaml (default: auto-discover)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: loomcycle doctor [--config <path>]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Runs 6 health checks against the operator's config + environment.")
		fmt.Fprintln(stderr, "Prints PASS/WARN/FAIL per check; exits 0 on clean, 1 on any FAIL.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Fprintln(stdout, "loomcycle doctor — system health check")
	fmt.Fprintln(stdout)

	r := &doctorRun{stdout: stdout}

	resolvedPath, found := resolveConfigForDoctor(*cfgPath)
	if !found {
		r.fail("Config found", fmt.Sprintf("no config at %s; run `loomcycle init`", strings.Join(configSearchPaths(), " or ")))
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, r.summary())
		return 1
	}
	r.pass("Config found", resolvedPath)

	cfg, err := loadConfigForDoctor(resolvedPath)
	if err != nil {
		r.fail("Config parses", err.Error())
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, r.summary())
		return 1
	}
	r.pass("Config parses", "")

	// Mirror the server: auto-load <configdir>/auth.env (set-if-unset)
	// BEFORE the token check, so doctor's verdict matches what `loomcycle`
	// will actually see. Without this, `init --with-token` followed by
	// `doctor` would falsely warn "unauthenticated" while the server runs
	// authed off the persisted token.
	tokenFromFile := false
	if _, n, lerr := LoadAuthEnv(resolvedPath); lerr == nil && n > 0 && os.Getenv("LOOMCYCLE_AUTH_TOKEN") != "" {
		tokenFromFile = true
	}

	if os.Getenv("LOOMCYCLE_AUTH_TOKEN") == "" {
		r.warn("LOOMCYCLE_AUTH_TOKEN set", "empty; every /v1/* request will be allowed unauthenticated")
	} else if tokenFromFile {
		r.pass("LOOMCYCLE_AUTH_TOKEN set", "loaded from auth.env (init --with-token)")
	} else {
		r.pass("LOOMCYCLE_AUTH_TOKEN set", "")
	}

	providers := providerListFromConfig(cfg)
	if len(providers) == 0 {
		r.warn("Providers", "none configured in provider_priority or per-agent providers")
	}
	for _, p := range providers {
		envVar := providerEnvVarName(p)
		if envVar == "" {
			r.pass(fmt.Sprintf("Provider %s", p), "no API key required (local provider)")
			continue
		}
		if os.Getenv(envVar) == "" {
			r.warn(fmt.Sprintf("Provider %s", p), fmt.Sprintf("%s not set", envVar))
		} else {
			r.pass(fmt.Sprintf("Provider %s", p), fmt.Sprintf("%s set", envVar))
		}
	}

	storageDetail, storageOK := checkStorageBackend(cfg)
	if storageOK {
		r.pass("Storage backend", storageDetail)
	} else {
		r.fail("Storage backend", storageDetail)
	}

	listenAddr := configListenAddr(cfg)
	if err := checkPortBindable(listenAddr); err != nil {
		r.fail("Listen address", fmt.Sprintf("%s not bindable: %v", listenAddr, err))
	} else {
		r.pass("Listen address", fmt.Sprintf("%s (bindable)", listenAddr))
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, r.summary())

	if r.failCount > 0 {
		return 1
	}
	return 0
}

// doctorRun accumulates PASS/WARN/FAIL counts as the checks run and
// emits per-check lines to stdout.
type doctorRun struct {
	stdout     io.Writer
	passCount  int
	warnCount  int
	failCount  int
	labelWidth int
}

func (r *doctorRun) pass(label, detail string) {
	r.passCount++
	r.emit("PASS", label, detail)
}
func (r *doctorRun) warn(label, detail string) {
	r.warnCount++
	r.emit("WARN", label, detail)
}
func (r *doctorRun) fail(label, detail string) {
	r.failCount++
	r.emit("FAIL", label, detail)
}
func (r *doctorRun) emit(level, label, detail string) {
	if detail == "" {
		fmt.Fprintf(r.stdout, "[%s]  %s\n", level, label)
	} else {
		fmt.Fprintf(r.stdout, "[%s]  %-22s: %s\n", level, label, detail)
	}
}
func (r *doctorRun) summary() string {
	wPlural := plural(r.warnCount, "warning")
	fPlural := plural(r.failCount, "failure")
	return fmt.Sprintf("%d %s, %d %s.", r.warnCount, wPlural, r.failCount, fPlural)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// providerEnvVarName returns the canonical API-key env var for a
// provider id. Empty when the provider is local (ollama-local) and
// thus needs no key.
func providerEnvVarName(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	case "ollama":
		// Ollama Cloud — hosted via api.ollama.com — needs a key.
		return "OLLAMA_API_KEY"
	case "ollama-local":
		// Local instance; OLLAMA_BASE_URL controls reachability, no
		// key needed.
		return ""
	}
	return ""
}

// resolveConfigForDoctor mirrors the binary's auto-discovery logic.
// When path is non-empty, returns it as-is (caller passed --config).
// Otherwise walks the standard XDG paths and returns the first hit.
func resolveConfigForDoctor(path string) (resolved string, found bool) {
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
		return path, false
	}
	for _, p := range configSearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// configSearchPaths returns the in-order paths the binary checks
// when --config is left at the default. Public-ish so init / doctor
// share the same list.
func configSearchPaths() []string {
	paths := []string{"./loomcycle.yaml"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "loomcycle", "loomcycle.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "loomcycle", "loomcycle.yaml"))
	}
	return paths
}

// loadConfigForDoctor delegates to config.Load. Kept as a seam so
// tests with a stubbed config implementation can plug in (though v1
// just uses the real loader).
var loadConfigForDoctor = func(path string) (configForDoctor, error) {
	return loadConfigRealAdapter(path)
}

// configForDoctor is a narrow interface over the bits of config.Config
// that doctor actually reads. Keeping it narrow lets future doctor
// tests stub a fake without dragging in the full Config type.
type configForDoctor interface {
	ProviderPriorityList() []string
	// AgentProviderHints returns every provider name mentioned on any
	// agent's per-agent `providers:` list OR `provider:` pin. Operators
	// often leave `provider_priority` empty and pin per-agent; doctor
	// needs to know about those to check the right env vars.
	AgentProviderHints() []string
	// UserTierProviderHints returns every provider name on any
	// `user_tiers.*.provider_priority` overlay. Same reason as
	// AgentProviderHints — overlays can introduce providers the
	// top-level list doesn't carry.
	UserTierProviderHints() []string
	StorageBackend() string
	StoragePgDSN() string
	StorageDataDir() string
	ListenAddrValue() string
}

// providerListFromConfig collects every provider name referenced
// anywhere in the config — top-level priority, per-agent
// providers/pin, per-user-tier overlays. Sorted + de-duped for stable
// output. An operator running entirely off per-agent pins (empty
// top-level provider_priority) still gets per-provider key checks.
func providerListFromConfig(cfg configForDoctor) []string {
	seen := map[string]struct{}{}
	add := func(ps []string) {
		for _, p := range ps {
			if p != "" {
				seen[p] = struct{}{}
			}
		}
	}
	add(cfg.ProviderPriorityList())
	add(cfg.AgentProviderHints())
	add(cfg.UserTierProviderHints())
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// checkStorageBackend reports whether the configured backend is
// reachable enough for doctor's purpose:
//
//   - sqlite: data dir creatable + a probe file writable
//   - postgres: DSN non-empty (full Open() is too heavy for a v1
//     diagnostic; v0.11.2 can grow a real connectivity check)
func checkStorageBackend(cfg configForDoctor) (detail string, ok bool) {
	backend := cfg.StorageBackend()
	if backend == "" {
		backend = "sqlite"
	}
	switch backend {
	case "sqlite":
		dir := cfg.StorageDataDir()
		if dir == "" {
			dir = "./data"
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Sprintf("sqlite data dir %s not creatable: %v", dir, err), false
		}
		probe := filepath.Join(dir, ".loomcycle-doctor-probe")
		if err := os.WriteFile(probe, []byte("probe"), 0o644); err != nil {
			return fmt.Sprintf("sqlite data dir %s not writable: %v", dir, err), false
		}
		_ = os.Remove(probe)
		return fmt.Sprintf("sqlite at %s (writable)", dir), true
	case "postgres":
		dsn := cfg.StoragePgDSN()
		if dsn == "" {
			return "postgres backend selected but DSN empty (set storage.pg_dsn or LOOMCYCLE_PG_DSN)", false
		}
		return fmt.Sprintf("postgres at %s (DSN set; connectivity check deferred to v0.11.2)", maskDSN(dsn)), true
	}
	return fmt.Sprintf("unknown backend %q", backend), false
}

func configListenAddr(cfg configForDoctor) string {
	if v := cfg.ListenAddrValue(); v != "" {
		return v
	}
	return "127.0.0.1:8787"
}

// checkPortBindable tries to listen on the address briefly. The bound
// listener is closed immediately — we just want to know whether the
// port is available right now. A FAIL here usually means another
// loomcycle is already running or another process owns the port.
func checkPortBindable(addr string) error {
	d := &net.ListenConfig{}
	// 1s deadline keeps doctor fast even when the address is bad.
	// SO_REUSEADDR semantics differ per OS — a clean Close immediately
	// after binding is the portable way to check "could I have bound
	// here?" without racing the actual server.
	ctx, cancel := timeoutContext(1 * time.Second)
	defer cancel()
	l, err := d.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return l.Close()
}
