package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
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
	sink, desc, err := openAuditSink(context.Background(), b.cfg, b.storeIface)
	if err != nil {
		fatal("audit: the audit trail cannot be opened", "err", err)
	}
	if b.auditRetentionKeys, err = auditRetentionKeys(b.cfg); err != nil {
		fatal("audit: the audit retention cannot run", "err", err)
	}
	b.auditSink = sink
	if sink == nil {
		slog.Info("audit: no audit sink (single-operator posture; set LOOMCYCLE_AUDIT_STORE=1 or LOOMCYCLE_AUDIT_LOG_PATH to record)")
		return
	}
	slog.Info("audit: recording", "sinks", desc)
}

// auditOpenTimeout bounds the boot probe of the store-backed stream.
const auditOpenTimeout = 30 * time.Second

// openAuditSink builds the audit sink cfg asks for. It returns nil (and no
// error) when nothing is configured and the posture does not require the
// store-backed stream; desc names the sinks opened.
//
// A missing LOOMCYCLE_SECRET_KEY where the stream is on is an error, not a
// fallback to an unsigned chain: the password and device gates refuse the
// same way, and an unsigned chain would promise an integrity any database
// writer could undo.
func openAuditSink(ctx context.Context, cfg *config.Config, st store.Store) (audit.Sink, []string, error) {
	var sinks []audit.Sink
	var desc []string
	if cfg.AuditStoreEnabled() {
		why := "LOOMCYCLE_AUDIT_STORE=1"
		if cfg.AuditStoreRequired() {
			why = "a sign-in gate, auth.mode: strict or a non-compat LOOMCYCLE_ACL is configured"
		}
		if st == nil {
			return nil, nil, fmt.Errorf("the store-backed audit stream is on (%s) but there is no store", why)
		}
		cur, err := credential.DeriveAuditChainKey(cfg.Env.SecretKey)
		if err != nil {
			return nil, nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY: %w", err)
		}
		if cur == nil {
			return nil, nil, fmt.Errorf("the store-backed audit stream is on (%s) and signs its chain with a key "+
				"derived from LOOMCYCLE_SECRET_KEY, which is not set", why)
		}
		prev, err := credential.DeriveAuditChainKey(cfg.Env.SecretKeyPrevious)
		if err != nil {
			return nil, nil, fmt.Errorf("LOOMCYCLE_SECRET_KEY_PREVIOUS: %w", err)
		}
		// Reading the chain list proves the tables exist and the store
		// answers, so a store that cannot hold the stream stops the boot
		// rather than failing the first sign-in's record.
		pctx, cancel := context.WithTimeout(ctx, auditOpenTimeout)
		_, err = st.AuditTenants(pctx)
		cancel()
		if err != nil {
			return nil, nil, fmt.Errorf("the store-backed audit stream is on (%s) but the store cannot serve it: %w", why, err)
		}
		ss, err := audit.NewStoreSink(st, audit.ChainKeys{Current: cur, Previous: prev})
		if err != nil {
			return nil, nil, err
		}
		sinks = append(sinks, ss)
		desc = append(desc, "store")
	}
	if cfg.Env.AuditLogPath != "" {
		fs, err := audit.NewFileSink(cfg.Env.AuditLogPath)
		if err != nil {
			return nil, nil, err
		}
		sinks = append(sinks, fs)
		desc = append(desc, "file:"+cfg.Env.AuditLogPath)
	}
	return audit.Combine(sinks...), desc, nil
}

// auditRetentionKeys are the keys the retention sweeper's audit family
// verifies a chain with and signs its new anchor with, or nil when
// LOOMCYCLE_AUDIT_RETENTION_DAYS keeps every row. A retention age with no
// LOOMCYCLE_SECRET_KEY is an error: pruning without signing the anchor would
// leave a chain that no longer verifies.
func auditRetentionKeys(cfg *config.Config) (*audit.ChainKeys, error) {
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
	return &audit.ChainKeys{Current: cur, Previous: prev}, nil
}
