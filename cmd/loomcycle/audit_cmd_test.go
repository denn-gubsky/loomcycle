package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

// writeExport writes acme's chain as GET /v1/_audit streams it.
func writeExport(t *testing.T, dbPath string, edit func(lines []map[string]any)) string {
	t.Helper()
	st, err := storesqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	head, err := st.AuditHeadGet(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.AuditEventsList(ctx, "acme", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, r := range rows {
		lines = append(lines, map[string]any{"tenant": r.TenantID, "seq": r.Seq, "ts_ns": r.TS.UnixNano(),
			"actor": r.Actor, "action": r.Action, "subject": r.Subject, "payload": r.Payload,
			"prev_hash": r.PrevHash, "hash": r.Hash, "mac": r.MAC})
	}
	lines = append(lines, map[string]any{"trailer": true, "tenant": head.TenantID, "head_seq": head.Seq,
		"head_hash": head.Hash, "head_mac": head.MAC})
	if edit != nil {
		edit(lines)
	}
	var b bytes.Buffer
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "acme.jsonl")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --export checks a file offline, with the signatures when the key is set
// and saying it did not when it is not.
func TestAuditVerifyCmd_VerifiesAnExportOffline(t *testing.T) {
	_, dbPath := auditCmdEnv(t)
	good := writeExport(t, dbPath, nil)
	code, out, errOut := runAuditForTest("verify", "--export", good)
	if code != 0 || !strings.Contains(out, `export of tenant "acme": ok — 3 rows`) || !strings.Contains(out, "signatures checked") {
		t.Fatalf("an intact export with the key: exit %d\n%s%s", code, out, errOut)
	}
	bad := writeExport(t, dbPath, func(l []map[string]any) { l[1]["actor"] = "mallory" })
	if code, out, _ := runAuditForTest("verify", "--export", bad); code != 1 || !strings.Contains(out, "BROKEN at row 2") {
		t.Fatalf("a tampered export: exit %d\n%s", code, out)
	}
	if code, _, _ := runAuditForTest("verify", "--export", good, "--tenant", "acme"); code != 2 {
		t.Errorf("--export with --tenant: exit %d, want 2", code)
	}
	t.Setenv("LOOMCYCLE_SECRET_KEY", "")
	if code, out, _ := runAuditForTest("verify", "--export", good); code != 0 || !strings.Contains(out, "hash links only") {
		t.Errorf("an export with no key: exit %d\n%s; want ok, saying the signatures were not checked", code, out)
	}
}
