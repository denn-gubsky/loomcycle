package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/logging"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// setupLogging installs the process-wide slog logger (format + level from
// LOOMCYCLE_LOG_FORMAT / LOOMCYCLE_LOG_LEVEL) and, through it, the stdlib log
// package. It runs before subcommand dispatch so every code path logs in the
// chosen format, and before anything could log a secret: the exact values of
// secret-named env vars present at startup are masked in every line.
//
// Assumption: secrets resolved later at run time (a $cred: value, a tenant's
// provider-key override) are registered on the HTTP server's own redactor,
// not this one, so they are NOT scrubbed from log lines yet.
//
// It returns the log redactor, so the boot can add secrets it reads later
// that are never in the environment (the collector headers file).
func setupLogging() *redact.Redactor {
	r := redact.New(secretEnvForLogs(os.Environ()), false)
	logging.Setup(logging.Options{
		Format:   os.Getenv(logging.EnvFormat),
		Level:    os.Getenv(logging.EnvLevel),
		Redactor: r,
		Identity: logIdentity,
	})
	return r
}

// secretEnvForLogs maps each secret-named env var (config.IsSecretEnvName)
// to its value. Tier-B pattern heuristics are deliberately left off for logs:
// they are tuned for tool I/O and would rewrite benign log text.
func secretEnvForLogs(environ []string) map[string]string {
	return config.SecretEnvValues(environ)
}

// logIdentity reads the run identity the runtime already carries on ctx.
func logIdentity(ctx context.Context) logging.Identity {
	id := tools.RunIdentity(ctx)
	return logging.Identity{
		RunID:  tools.RunID(ctx),
		Tenant: id.TenantID,
		Agent:  tools.AgentName(ctx),
		User:   id.UserID,
	}
}

// fatal logs msg at ERROR and exits 1, as log.Fatalf did: deferred calls do
// not run. It goes through the slog default, so the line comes out in the
// configured format with the configured redaction.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
