package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auditchain"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// openAudit opens the deployment's audit sink before anything that records
// to it is wired: the store-backed chain when the posture requires it or
// LOOMCYCLE_AUDIT_STORE=1 asks for it, the JSONL file when
// LOOMCYCLE_AUDIT_LOG_PATH is set, both when both. A sink that is required
// and cannot open stops the boot (RFC FZ D7).
func (b *boot) openAudit() {
	sink, stream, desc, err := openAuditSink(context.Background(), b.cfg, b.storeIface)
	if err != nil {
		fatal("audit: the audit trail cannot be opened", "err", err)
	}
	if stream != nil {
		// Observations (refusals, denials, lookups) go through one queue and
		// one writer; drain it at shutdown, before the store closes.
		stream.StartAsync(auditchain.DefaultQueueCapacity)
		b.addCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), auditDrainTimeout)
			defer cancel()
			if err := stream.Close(ctx); err != nil {
				pending, dropped := stream.Backlog()
				slog.Warn("audit: the observation queue did not drain before shutdown; these never reached the chain",
					"pending", pending, "dropped", dropped, "err", err)
			}
		})
	}
	if b.auditRetentionKeys, err = auditRetentionKeys(b.cfg); err != nil {
		fatal("audit: the audit retention cannot run", "err", err)
	}
	b.auditSink = sink
	b.auditStream = stream
	if sink == nil {
		slog.Info("audit: no audit sink (single-operator posture; set LOOMCYCLE_AUDIT_STORE=1 or LOOMCYCLE_AUDIT_LOG_PATH to record)")
		return
	}
	slog.Info("audit: recording", "sinks", desc)
}

// auditOpenTimeout bounds the boot probe of the store-backed stream.
const auditOpenTimeout = 30 * time.Second

// auditDrainTimeout bounds the shutdown's wait for queued observations. It
// comes after the listeners drain and the pause, so it is kept short; the
// deployment docs recommend a 30 s stop grace for the whole sequence.
const auditDrainTimeout = 5 * time.Second

// openAuditSink builds the audit sink cfg asks for. It returns nil (and no
// error) when nothing is configured and the posture does not require the
// store-backed stream; desc names the sinks opened.
//
// A missing LOOMCYCLE_SECRET_KEY where the stream is on is an error, not a
// fallback to an unsigned chain: the password and device gates refuse the
// same way, and an unsigned chain would promise an integrity any database
// writer could undo.
func openAuditSink(ctx context.Context, cfg *config.Config, st store.Store) (audit.Sink, *auditchain.StoreSink, []string, error) {
	var sinks []audit.Sink
	var stream *auditchain.StoreSink
	var desc []string
	stored, err := storedPrincipals(ctx, cfg, st)
	if err != nil {
		return nil, nil, nil, err
	}
	required := cfg.AuditRequiredReason(stored)
	if required != "" && cfg.Env.AuditStoreOff {
		return nil, nil, nil, fmt.Errorf("LOOMCYCLE_AUDIT_STORE=0: the store-backed audit stream cannot be turned off while %s", required)
	}
	if cfg.Env.AuditStore || required != "" {
		why := "LOOMCYCLE_AUDIT_STORE=1"
		if required != "" {
			why = required
		}
		if st == nil {
			return nil, nil, nil, fmt.Errorf("the store-backed audit stream is on (%s) but there is no store", why)
		}
		cur, err := credential.DeriveAuditChainKey(cfg.Env.SecretKey)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY: %w", err)
		}
		if cur == nil {
			return nil, nil, nil, fmt.Errorf("the store-backed audit stream is on (%s) and signs its chain with a key "+
				"derived from LOOMCYCLE_SECRET_KEY, which is not set", why)
		}
		prev, err := credential.DeriveAuditChainKey(cfg.Env.SecretKeyPrevious)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY_PREVIOUS: %w", err)
		}
		// Reading the chain list proves the tables exist and the store
		// answers, so a store that cannot hold the stream stops the boot
		// rather than failing the first sign-in's record.
		pctx, cancel := context.WithTimeout(ctx, auditOpenTimeout)
		_, err = st.AuditTenants(pctx)
		cancel()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("the store-backed audit stream is on (%s) but the store cannot serve it: %w", why, err)
		}
		ss, err := auditchain.NewStoreSink(st, auditchain.ChainKeys{Current: cur, Previous: prev})
		if err != nil {
			return nil, nil, nil, err
		}
		stream = ss
		sinks = append(sinks, ss)
		desc = append(desc, "store")
	}
	if cfg.Env.AuditLogPath != "" {
		fs, err := audit.NewFileSink(cfg.Env.AuditLogPath)
		if err != nil {
			return nil, nil, nil, err
		}
		sinks = append(sinks, fs)
		desc = append(desc, "file:"+cfg.Env.AuditLogPath)
		if stream != nil {
			// A drop summary the store refuses at shutdown lands in the file.
			stream.SetFallback(fs)
		}
	}
	return audit.Combine(sinks...), stream, desc, nil
}

// storedPrincipals counts the live operator tokens in the store, when they
// could decide the posture: config alone does not already require the
// stream. Each live token name is one principal besides the configured
// credentials (config.AuditRequiredReason); a store that cannot answer stops
// the boot rather than guess a single-operator posture.
//
// The posture is decided here, at boot: a single-operator deployment that
// mints its first token later records nothing to the store until it
// restarts, and that restart then turns the stream on (or refuses without
// LOOMCYCLE_SECRET_KEY).
func storedPrincipals(ctx context.Context, cfg *config.Config, st store.Store) (int, error) {
	if st == nil || cfg.AuditStoreRequired() {
		return 0, nil
	}
	pctx, cancel := context.WithTimeout(ctx, auditOpenTimeout)
	defer cancel()
	names, err := st.OperatorTokenDefListNames(pctx)
	if err != nil {
		return 0, fmt.Errorf("audit: counting the operator tokens that decide the audit posture: %w", err)
	}
	n := 0
	for _, nm := range names {
		if nm.HasCurrent {
			n++
		}
	}
	return n, nil
}

// auditRetentionKeys are the keys the retention sweeper's audit family
// verifies a chain with and signs its new anchor with, or nil when
// LOOMCYCLE_AUDIT_RETENTION_DAYS keeps every row. A retention age with no
// LOOMCYCLE_SECRET_KEY is an error: pruning without signing the anchor would
// leave a chain that no longer verifies.
func auditRetentionKeys(cfg *config.Config) (*auditchain.ChainKeys, error) {
	if cfg.Env.AuditRetention <= 0 {
		return nil, nil
	}
	cur, err := credential.DeriveAuditChainKey(cfg.Env.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY: %w", err)
	}
	if cur == nil {
		return nil, fmt.Errorf("LOOMCYCLE_AUDIT_RETENTION_DAYS prunes the audit chain behind a signed anchor and needs LOOMCYCLE_SECRET_KEY to sign it")
	}
	prev, err := credential.DeriveAuditChainKey(cfg.Env.SecretKeyPrevious)
	if err != nil {
		return nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY_PREVIOUS: %w", err)
	}
	return &auditchain.ChainKeys{Current: cur, Previous: prev}, nil
}
