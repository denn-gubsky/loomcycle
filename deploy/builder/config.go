package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the sidecar's operator-set policy, all from the environment. Every
// per-call value an agent supplies is clamped to the ceilings here — the model
// can ask for less isolation headroom but never more than the operator allows.
type Config struct {
	ListenAddr string // SANDBOX_LISTEN_ADDR (default :9000)

	// AuthToken is the shared bearer every MCP request must present
	// (Authorization: Bearer <token>). Required unless AllowAnon — a sandbox
	// that executes code must never run as an open, unauthenticated service.
	AuthToken string // SANDBOX_AUTH_TOKEN
	AllowAnon bool   // SANDBOX_ALLOW_ANON=1 (local dev only; logs a warning)

	// Engine.
	PodmanBin string // SANDBOX_PODMAN_BIN (default "podman")
	Image     string // SANDBOX_IMAGE — the toolchain image sessions run
	Runtime   string // SANDBOX_RUNTIME → --runtime (runc|runsc|kata; "" = engine default)
	CtrUser   string // SANDBOX_CONTAINER_USER → --user (default "1000:1000"; never root)

	// Network. Sessions are --network none by default; an agent may request
	// network:"egress" only when the operator opts in.
	AllowEgress bool // SANDBOX_ALLOW_EGRESS=1

	// Resource ceilings (per session container) + defaults when the caller
	// omits a value.
	DefTmpfsMB int64 // SANDBOX_DEFAULT_TMPFS_MB (default 512)
	MaxTmpfsMB int64 // SANDBOX_MAX_TMPFS_MB (default 2048)
	DefCPUs    float64
	MaxCPUs    float64 // SANDBOX_MAX_CPUS (default 2)
	DefMemMB   int64
	MaxMemMB   int64 // SANDBOX_MAX_MEM_MB (default 2048)
	DefPids    int64
	MaxPids    int64 // SANDBOX_MAX_PIDS (default 512)

	// Exec bounds — mirror the Bash/Bashbox tool guards so behaviour is
	// consistent across loomcycle's shell surfaces.
	DefTimeout  time.Duration // 30s
	MaxTimeout  time.Duration // 5m
	MaxOutBytes int64         // 1 MiB

	// Session lifecycle.
	SessionIdleTTL time.Duration // SANDBOX_SESSION_IDLE_TTL (default 15m)
	SessionMaxTTL  time.Duration // SANDBOX_SESSION_MAX_TTL (default 1h)
	GCInterval     time.Duration // SANDBOX_GC_INTERVAL (default 1m)
	MaxSessions    int           // SANDBOX_MAX_SESSIONS (default 32; global in P1)
}

// LoadConfig reads the environment into a Config, applying defaults and
// validating the security-critical invariants.
func LoadConfig() (*Config, error) {
	c := &Config{
		ListenAddr:     envStr("SANDBOX_LISTEN_ADDR", ":9000"),
		AuthToken:      os.Getenv("SANDBOX_AUTH_TOKEN"),
		AllowAnon:      os.Getenv("SANDBOX_ALLOW_ANON") == "1",
		PodmanBin:      envStr("SANDBOX_PODMAN_BIN", "podman"),
		Image:          os.Getenv("SANDBOX_IMAGE"),
		Runtime:        os.Getenv("SANDBOX_RUNTIME"),
		CtrUser:        envStr("SANDBOX_CONTAINER_USER", "1000:1000"),
		AllowEgress:    os.Getenv("SANDBOX_ALLOW_EGRESS") == "1",
		DefTmpfsMB:     envInt("SANDBOX_DEFAULT_TMPFS_MB", 512),
		MaxTmpfsMB:     envInt("SANDBOX_MAX_TMPFS_MB", 2048),
		MaxCPUs:        envFloat("SANDBOX_MAX_CPUS", 2),
		MaxMemMB:       envInt("SANDBOX_MAX_MEM_MB", 2048),
		MaxPids:        envInt("SANDBOX_MAX_PIDS", 512),
		DefTimeout:     30 * time.Second,
		MaxTimeout:     5 * time.Minute,
		MaxOutBytes:    envInt("SANDBOX_MAX_OUTPUT_BYTES", 1<<20),
		SessionIdleTTL: envDur("SANDBOX_SESSION_IDLE_TTL", 15*time.Minute),
		SessionMaxTTL:  envDur("SANDBOX_SESSION_MAX_TTL", time.Hour),
		GCInterval:     envDur("SANDBOX_GC_INTERVAL", time.Minute),
		MaxSessions:    int(envInt("SANDBOX_MAX_SESSIONS", 32)),
	}
	// Per-session defaults are the ceilings unless separately narrowed — a
	// caller asking for less is honoured, more is clamped (see clampOpen).
	c.DefCPUs = c.MaxCPUs
	c.DefMemMB = c.MaxMemMB
	c.DefPids = c.MaxPids

	if c.Image == "" {
		return nil, fmt.Errorf("SANDBOX_IMAGE is required (the toolchain image sessions run; build the reference one from deploy/builder/session/Dockerfile)")
	}
	if c.AuthToken == "" && !c.AllowAnon {
		return nil, fmt.Errorf("SANDBOX_AUTH_TOKEN is required (a code-execution sandbox must not run unauthenticated); set SANDBOX_ALLOW_ANON=1 ONLY for local dev")
	}
	if c.Runtime != "" && c.Runtime != "runc" && c.Runtime != "runsc" && c.Runtime != "crun" && c.Runtime != "kata" && c.Runtime != "kata-runtime" {
		return nil, fmt.Errorf("SANDBOX_RUNTIME %q not recognised (use runc|crun|runsc|kata)", c.Runtime)
	}
	return c, nil
}

func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envDur(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
