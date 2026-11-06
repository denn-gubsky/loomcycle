package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auditchain"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// auditCmdEnv isolates the command from the host as tokensTestEnv does, and
// writes a chain for tenant acme (three events) through the store-backed
// sink under the key derived from LOOMCYCLE_SECRET_KEY. It returns the
// --config path and the database path.
func auditCmdEnv(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"LOOMCYCLE_PRESETS", "LOOMCYCLE_CONFIG_DIR", "LOOMCYCLE_CONFIG_FILES",
		"LOOMCYCLE_STORAGE_BACKEND", "LOOMCYCLE_PG_DSN", "LOOMCYCLE_REPLICA_ID",
		"LOOMCYCLE_SECRET_KEY_PREVIOUS", "XDG_CONFIG_HOME", "LOOMCYCLE_ACL", "LOOMCYCLE_AUDIT_STORE",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", dir)
	t.Setenv("LOOMCYCLE_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("LOOMCYCLE_SECRET_KEY", testSecretKey())
	cfgPath := filepath.Join(dir, "loomcycle.yaml")
	if err := os.WriteFile(cfgPath, []byte("storage:\n  backend: sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "data", "loomcycle.db")
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, err := credential.DeriveAuditChainKey(testSecretKey())
	if err != nil {
		t.Fatal(err)
	}
	sink, err := auditchain.NewStoreSink(st, auditchain.ChainKeys{Current: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"alice", "bob", "carol"} {
		if err := sink.Record(audit.Event{Action: "auth_login", ActorTenant: "acme", ActorSubject: s}); err != nil {
			t.Fatal(err)
		}
	}
	return cfgPath, dbPath
}

func runAuditForTest(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runAudit(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestAuditVerifyCmd_ReportsTheFirstBreak(t *testing.T) {
	cfgPath, dbPath := auditCmdEnv(t)

	code, out, errOut := runAuditForTest("verify", "--config", cfgPath, "--tenant", "acme")
	if code != 0 || !strings.Contains(out, `tenant "acme": ok — 3 rows verified`) {
		t.Fatalf("an intact chain: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = runAuditForTest("verify", "--config", cfgPath, "--all", "--json")
	if code != 0 || strings.Count(out, `"ok":true`) != 2 {
		t.Fatalf("--all on intact chains: exit %d\n%s", code, out)
	}

	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE audit_events SET actor = 'mallory' WHERE tenant_id = 'acme' AND seq = 2`); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runAuditForTest("verify", "--config", cfgPath, "--tenant", "acme")
	if code != 1 || !strings.Contains(out, "BROKEN at row 2") {
		t.Fatalf("a tampered chain: exit %d\n%s", code, out)
	}
	code, out, _ = runAuditForTest("verify", "--config", cfgPath, "--all")
	if code != 1 || !strings.Contains(out, "operator chain: ok") || !strings.Contains(out, `tenant "acme": BROKEN`) {
		t.Fatalf("--all with one tampered chain: exit %d\n%s", code, out)
	}

	// Usage and configuration errors are exit 2.
	if code, _, _ := runAuditForTest("verify", "--config", cfgPath); code != 2 {
		t.Errorf("no chain named: exit %d, want 2", code)
	}
	if code, _, _ := runAuditForTest("verify", "--config", cfgPath, "--tenant", "acme", "--all"); code != 2 {
		t.Errorf("two chains named: exit %d, want 2", code)
	}
	t.Setenv("LOOMCYCLE_SECRET_KEY", "")
	if code, _, errOut := runAuditForTest("verify", "--config", cfgPath, "--tenant", "acme"); code != 2 || !strings.Contains(errOut, "LOOMCYCLE_SECRET_KEY") {
		t.Errorf("no secret key: exit %d (%s), want 2 naming LOOMCYCLE_SECRET_KEY", code, errOut)
	}
}
