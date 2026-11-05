package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// runAudit is `loomcycle audit <verb>`. Like `tokens`, it opens the store the
// way the server does (the same config layering and openStore) and derives
// the chain key from the same LOOMCYCLE_SECRET_KEY, so it reads the chains
// the server writes and checks them with the key that signed them.
//
// Exit codes: 0 every chain verifies, 1 a chain does not verify or the store
// could not be read, 2 a usage or configuration error.
func runAudit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(stderr, auditVerifyUsage)
		return 2
	}
	return runAuditVerify(args[1:], stdout, stderr)
}

const auditVerifyUsage = "Usage: loomcycle audit verify (--tenant <tenant> | --operator | --all) [--config <yaml>]... [--preset <name>]... [--json]"

func runAuditVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfgPaths, presets []string
	fs.Func("config", "path to config YAML, as the server is given it (repeatable; default: the server's auto-discovery)", func(v string) error {
		cfgPaths = append(cfgPaths, v)
		return nil
	})
	fs.Func("preset", "embedded preset/bundle, as the server is given it (repeatable; default: LOOMCYCLE_PRESETS)", func(v string) error {
		presets = append(presets, v)
		return nil
	})
	var tenant string
	tenantSet := false
	fs.Func("tenant", "verify this tenant's chain (and the operator chain that records it)", func(v string) error {
		tenant, tenantSet = v, true
		return nil
	})
	operator := fs.Bool("operator", false, "verify the operator chain (events that belong to no tenant)")
	all := fs.Bool("all", false, "verify the operator chain and every tenant chain")
	asJSON := fs.Bool("json", false, "print one JSON report per chain")
	fs.Usage = func() {
		fmt.Fprintln(stderr, auditVerifyUsage)
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Walks the store-backed audit chain from its retention anchor to its head and")
		fmt.Fprintln(stderr, "reports the first row that does not verify: changed, missing, out of order, or")
		fmt.Fprintln(stderr, "not signed with the deployment's key. Run it with the server's own config and")
		fmt.Fprintln(stderr, "environment (LOOMCYCLE_SECRET_KEY). Read-only; safe while servers are running.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "loomcycle: error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	picked := 0
	for _, on := range []bool{tenantSet, *operator, *all} {
		if on {
			picked++
		}
	}
	if picked != 1 || (tenantSet && tenant == "") {
		fmt.Fprintln(stderr, "loomcycle: error: name one of --tenant <tenant>, --operator or --all")
		return 2
	}

	layers, err := assembleConfigLayers(presets, cfgPaths, false)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: %v\n", err)
		return 2
	}
	cfg, err := config.LoadLayers(layers...)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: config: %v\n", err)
		return 2
	}
	cur, err := credential.DeriveAuditChainKey(cfg.Env.SecretKey)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: LOOMCYCLE_SECRET_KEY: %v\n", err)
		return 2
	}
	if cur == nil {
		fmt.Fprintln(stderr, "loomcycle: error: LOOMCYCLE_SECRET_KEY is not set: the chain is signed with a key derived from it; run with the server's environment")
		return 2
	}
	prev, err := credential.DeriveAuditChainKey(cfg.Env.SecretKeyPrevious)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: LOOMCYCLE_SECRET_KEY_PREVIOUS: %v\n", err)
		return 2
	}
	// A sqlite path that does not exist is not the server's database;
	// opening it would create an empty one that verifies trivially.
	if cfg.Storage.Backend == "sqlite" || cfg.Storage.Backend == "" {
		dbPath := filepath.Join(cfg.Env.DataDir, "loomcycle.db")
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "loomcycle: error: no database at %s: run with the server's LOOMCYCLE_DATA_DIR and config\n", dbPath)
			return 2
		}
	}
	st, closeStore, err := openStore(cfg, false)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: store: %v\n", err)
		return 1
	}
	defer closeStore()

	ctx := context.Background()
	keys := audit.ChainKeys{Current: cur, Previous: prev}
	var reports []audit.VerifyReport
	switch {
	case *all:
		tenants, err := st.AuditTenants(ctx)
		if err == nil {
			reports, err = audit.VerifyAll(ctx, st, keys, tenants)
		}
		if err != nil {
			fmt.Fprintf(stderr, "loomcycle: error: reading the audit chains: %v\n", err)
			return 1
		}
	default:
		rep, err := audit.Verify(ctx, st, keys, tenant)
		if err != nil {
			fmt.Fprintf(stderr, "loomcycle: error: reading the audit chain: %v\n", err)
			return 1
		}
		reports = []audit.VerifyReport{rep}
	}
	code := 0
	for _, rep := range reports {
		if !rep.OK {
			code = 1
		}
		if *asJSON {
			b, _ := json.Marshal(rep)
			fmt.Fprintln(stdout, string(b))
			continue
		}
		name := fmt.Sprintf("tenant %q", rep.Tenant)
		if rep.Tenant == "" {
			name = "operator chain"
		}
		if rep.OK {
			fmt.Fprintf(stdout, "%s: ok — %d rows verified (anchor %d, head %d)\n", name, rep.Rows, rep.AnchorSeq, rep.HeadSeq)
		} else {
			// Reason is built by audit.Verify from row numbers and fixed
			// text; no stored field is echoed into it.
			fmt.Fprintf(stdout, "%s: BROKEN at row %d — %s\n", name, rep.BreakSeq, rep.Reason)
		}
	}
	return code
}
