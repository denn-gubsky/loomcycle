package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// tokensTestEnv isolates the command from the host's environment and config,
// points it at a sqlite database in a scratch data dir holding one
// unpeppered token row, and returns that store (left open, as a running
// server would hold it) and the --config path.
func tokensTestEnv(t *testing.T, secretKey, explicitPepper string) (store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"LOOMCYCLE_PRESETS", "LOOMCYCLE_CONFIG_DIR", "LOOMCYCLE_CONFIG_FILES",
		"LOOMCYCLE_STORAGE_BACKEND", "LOOMCYCLE_PG_DSN", "LOOMCYCLE_REPLICA_ID",
		"LOOMCYCLE_SECRET_KEY_PREVIOUS", "XDG_CONFIG_HOME",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", dir)
	t.Setenv("LOOMCYCLE_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("LOOMCYCLE_SECRET_KEY", secretKey)
	t.Setenv("LOOMCYCLE_OPERATOR_TOKEN_PEPPER", explicitPepper)
	cfgPath := filepath.Join(dir, "loomcycle.yaml")
	if err := os.WriteFile(cfgPath, []byte("storage:\n  backend: sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := storesqlite.Open(filepath.Join(dir, "data", "loomcycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_00000000000000a1", Name: "old", TenantID: "default", Subject: "old",
		TokenHash: auth.HashToken("", "lct_old"), AllowedScopes: []string{"runs:create"},
	}); err != nil {
		t.Fatal(err)
	}
	return st, cfgPath
}

func tokensStoredHash(t *testing.T, st store.Store) string {
	t.Helper()
	rows, err := st.OperatorTokenDefListByName(context.Background(), "old")
	if err != nil || len(rows) != 1 {
		t.Fatalf("read def_00000000000000a1: %d rows, %v", len(rows), err)
	}
	return rows[0].TokenHash
}

func testSecretKey() string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
}

// The command rewraps a named unpeppered row onto the derived pepper's
// wrapped form — the form a server's login recomputes from the token — while
// another handle (a running server) holds the database.
func TestTokensRewrap_RewrapsTheUnpepperedRowUnderTheServersPepper(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	var out, errb bytes.Buffer
	named := reviewFingerprint(t, review(t, cfgPath), defOld)
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", named}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}

	env := config.Env{SecretKey: testSecretKey()}
	h, err := env.TokenHasher()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tokensStoredHash(t, st), h.WrappedHash("lct_old"); got != want {
		t.Fatalf("stored %q, want the wrapped form %q", got, want)
	}
	for _, want := range []string{"1 unpeppered", "rewrapped 1 of 1 named row(s)", `"def_00000000000000a1"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// A second run has nothing left to do.
	out.Reset()
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("second run rc = %d", rc)
	}
	if !strings.Contains(out.String(), "nothing to rewrap") {
		t.Errorf("second run output:\n%s", out.String())
	}
}

// --dry-run checks the named rows and writes nothing.
func TestTokensRewrap_DryRunChangesNothing(t *testing.T) {
	st, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	before := tokensStoredHash(t, st)
	var out, errb bytes.Buffer
	named := reviewFingerprint(t, review(t, cfgPath), defOld)
	if rc := runTokens([]string{"rewrap", "--config", cfgPath, "--def-id", named, "--dry-run"}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if got := tokensStoredHash(t, st); got != before {
		t.Fatalf("a dry run rewrote the row to %q", got)
	}
	for _, want := range []string{"dry run: would rewrap 1 row(s)", `"def_00000000000000a1"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// With no derived pepper the command refuses, says why, and writes nothing.
func TestTokensRewrap_RefusesWithoutADerivedPepper(t *testing.T) {
	for name, tc := range map[string]struct{ key, explicit, why string }{
		"explicit pepper": {testSecretKey(), "explicit-pepper", "LOOMCYCLE_OPERATOR_TOKEN_PEPPER is set"},
		"no pepper":       {"", "", "LOOMCYCLE_SECRET_KEY is not set"},
	} {
		t.Run(name, func(t *testing.T) {
			st, cfgPath := tokensTestEnv(t, tc.key, tc.explicit)
			before := tokensStoredHash(t, st)
			var out, errb bytes.Buffer
			if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 2 {
				t.Fatalf("rc = %d, want 2; stderr: %s", rc, errb.String())
			}
			if !strings.Contains(errb.String(), tc.why) {
				t.Errorf("stderr does not say why:\n%s", errb.String())
			}
			if got := tokensStoredHash(t, st); got != before {
				t.Errorf("a refused run rewrote the row to %q", got)
			}
		})
	}
}

// Pointed at a data dir with no database, the command refuses rather than
// create an empty one and report nothing to do.
func TestTokensRewrap_RefusesAMissingSqliteDatabase(t *testing.T) {
	_, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	missing := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv("LOOMCYCLE_DATA_DIR", missing)
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 2 {
		t.Fatalf("rc = %d, want 2; stderr: %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "no database at") {
		t.Errorf("stderr does not name the missing database:\n%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(missing, "loomcycle.db")); !os.IsNotExist(err) {
		t.Errorf("the command created a database at the wrong path: %v", err)
	}
}

func TestTokens_UnknownVerbIsAUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rotate"}, &out, &errb); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if rc := runTokens(nil, &out, &errb); rc != 2 {
		t.Fatalf("no verb: rc = %d, want 2", rc)
	}
}
