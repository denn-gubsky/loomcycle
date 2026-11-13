package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// badUnrelatedEnv sets what the server refuses to start with and neither
// command needs: a runtime setting that does not parse, a retired gate kept
// off, the retired config-files variable, a setting that moved to a yaml key
// the yaml does not name, a dead-link mode that is not one, and an
// automigrate value that is neither 1 nor 0 (read as 0 by both).
func badUnrelatedEnv(t *testing.T) []string {
	t.Helper()
	t.Setenv("LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", "zz-soon")
	t.Setenv("LOOMCYCLE_SCHEDULER_ENABLED", "0")
	t.Setenv("LOOMCYCLE_CONFIG_FILES", "/zz-nowhere.yaml")
	t.Setenv("LOOMCYCLE_METRICS_ENABLED", "1")
	t.Setenv("LOOMCYCLE_DEADLINK_GC", "zz-disabled")
	t.Setenv("LOOMCYCLE_PG_AUTOMIGRATE", "zz-false")
	return []string{"LOOMCYCLE_AUTH_CACHE_TTL_SECONDS", "LOOMCYCLE_SCHEDULER_ENABLED", "LOOMCYCLE_CONFIG_FILES",
		"LOOMCYCLE_METRICS_ENABLED", "LOOMCYCLE_DEADLINK_GC", "LOOMCYCLE_PG_AUTOMIGRATE"}
}

func wantWarnedNotValue(t *testing.T, stderr string, names []string) {
	t.Helper()
	for _, name := range names {
		if !strings.Contains(stderr, "loomcycle: warning: ") || !strings.Contains(stderr, name) {
			t.Errorf("stderr does not warn about %s:\n%s", name, stderr)
		}
	}
	if strings.Contains(stderr, "zz-") {
		t.Errorf("stderr carries a value:\n%s", stderr)
	}
}

// tokens rewrap is the tool for tokens that stopped logging in. An
// environment the server refuses, in variables it never reads, must not take
// the tool away as well: it warns and does its work.
func TestTokensRewrap_RunsUnderAnEnvironmentTheServerRefuses(t *testing.T) {
	_, cfgPath := tokensTestEnv(t, testSecretKey(), "")
	names := badUnrelatedEnv(t)
	var out, errb bytes.Buffer
	if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 0 {
		t.Fatalf("rc = %d, stderr: %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "1 unpeppered") {
		t.Errorf("the review did not run:\n%s", out.String())
	}
	wantWarnedNotValue(t, errb.String(), names)
}

func TestAuditVerify_RunsUnderAnEnvironmentTheServerRefuses(t *testing.T) {
	cfgPath, _ := auditCmdEnv(t)
	names := badUnrelatedEnv(t)
	for _, chain := range [][]string{{"--tenant", "acme"}, {"--operator"}, {"--all"}} {
		code, out, errOut := runAuditForTest(append([]string{"verify", "--config", cfgPath}, chain...)...)
		if code != 0 || !strings.Contains(out, "ok") {
			t.Fatalf("verify %v: exit %d\n%s%s", chain, code, out, errOut)
		}
		wantWarnedNotValue(t, errOut, names)
	}
}

// Which store is opened is not relaxed. A retired variable that named
// another backend still stops both tools, and so does the refusal to come up
// on Postgres beside an existing SQLite store.
func TestOfflineTools_StillStopOnAStoreRefusal(t *testing.T) {
	t.Run("the retired backend variable", func(t *testing.T) {
		_, cfgPath := tokensTestEnv(t, testSecretKey(), "")
		badUnrelatedEnv(t)
		t.Setenv("LOOMCYCLE_STORAGE_BACKEND", "postgres")
		var out, errb bytes.Buffer
		if rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb); rc != 2 || !strings.Contains(errb.String(), "LOOMCYCLE_STORAGE_BACKEND") {
			t.Errorf("tokens rewrap: rc = %d, stderr: %s", rc, errb.String())
		}
		cfgPath, _ = auditCmdEnv(t)
		t.Setenv("LOOMCYCLE_STORAGE_BACKEND", "postgres")
		if code, _, errOut := runAuditForTest("verify", "--config", cfgPath, "--all"); code != 2 || !strings.Contains(errOut, "LOOMCYCLE_STORAGE_BACKEND") {
			t.Errorf("audit verify: exit %d, stderr: %s", code, errOut)
		}
	})
	t.Run("an inferred Postgres beside an existing SQLite store", func(t *testing.T) {
		_, cfgPath := tokensTestEnv(t, testSecretKey(), "")
		badUnrelatedEnv(t)
		// No backend named, and a DSN in the environment: Postgres is inferred.
		if err := os.WriteFile(cfgPath, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LOOMCYCLE_PG_DSN", "postgres://u@127.0.0.1:1/x?connect_timeout=2")
		var out, errb bytes.Buffer
		rc := runTokens([]string{"rewrap", "--config", cfgPath}, &out, &errb)
		if rc == 0 || !strings.Contains(errb.String(), "existing SQLite store") {
			t.Errorf("tokens rewrap: rc = %d, stderr: %s", rc, errb.String())
		}
		if strings.Contains(errb.String(), "127.0.0.1:1") {
			t.Errorf("stderr carries the DSN:\n%s", errb.String())
		}
	})
}
