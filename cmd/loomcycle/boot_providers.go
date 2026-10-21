package main

import (
	"log"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
)

// buildProviders is boot phase 2: the provider set (behind its call
// scheduler) and the run-admission semaphore.
func (b *boot) buildProviders() {
	cfg := b.cfg

	// Everything built from cfg that obtains secrets at run time (an AWS
	// auth provider's credentials) registers them here; the holder points at
	// the server's redactor once that exists.
	secrets := &secretSink{}
	cfg.SecretSink = secrets.register
	pr, err := newProviderResolver(cfg)
	if err != nil {
		log.Fatalf("provider resolver: %v", err)
	}
	validateCodeAgents(cfg, pr)
	sem := concurrency.New(
		cfg.Concurrency.MaxConcurrentRuns,
		cfg.Concurrency.MaxQueueDepth,
		0, // no wait timeout: a run, and a direct request, waits until a slot frees
	).WithPerUserCap(cfg.Concurrency.MaxConcurrentRunsPerUser).
		WithQueuedPerUserCap(cfg.Concurrency.MaxQueuedRunsPerUser)
	if cfg.Concurrency.MaxConcurrentRunsPerUser > 0 {
		log.Printf("concurrency: per-user cap enabled — max_concurrent_runs_per_user=%d, max_queued_runs_per_user=%d (global cap=%d)",
			cfg.Concurrency.MaxConcurrentRunsPerUser, cfg.Concurrency.MaxQueuedRunsPerUser, cfg.Concurrency.MaxConcurrentRuns)
	}

	b.secrets = secrets
	b.pr = pr
	b.sem = sem
}
