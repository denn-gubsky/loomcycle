package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestAudit_IsOnByDefaultWithAGateConfigured: a gate, strict mode or a
// non-compat ACL turns the store-backed stream on with no audit setting; a
// single-operator deployment gets no sink unless it asks; and where the
// stream is on, a missing key or an unusable store is an error the boot
// stops on.
func TestAudit_IsOnByDefaultWithAGateConfigured(t *testing.T) {
	ctx := context.Background()
	key := testSecretKey()
	gate := []config.AuthGate{{Name: "paste", Kind: config.AuthGateKindToken}}
	open := func(t *testing.T) *sqlite.Store {
		t.Helper()
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	for name, cfg := range map[string]*config.Config{
		"a gate":          {ResolvedAuth: config.AuthPolicy{Gates: gate}},
		"strict mode":     {ResolvedAuth: config.AuthPolicy{Mode: config.AuthModeStrict}},
		"enforce ACL":     {Env: config.Env{ACLMode: "enforce"}},
		"shadow ACL":      {Env: config.Env{ACLMode: "shadow"}},
		"asked for (=1)":  {Env: config.Env{AuditStore: true}},
		"a gate and file": {ResolvedAuth: config.AuthPolicy{Gates: gate}, Env: config.Env{AuditLogPath: filepath.Join(t.TempDir(), "audit.jsonl")}},
	} {
		t.Run("on/"+name, func(t *testing.T) {
			cfg.Env.SecretKey = key
			st := open(t)
			sink, _, _, err := openAuditSink(ctx, cfg, st)
			if err != nil || sink == nil {
				t.Fatalf("openAuditSink = %v, %v; want the store-backed stream", sink, err)
			}
			if err := sink.Record(audit.Event{Action: "auth_login", ActorTenant: "acme", ActorSubject: "alice"}); err != nil {
				t.Fatal(err)
			}
			rows, err := st.AuditEventsList(ctx, "acme", 0, 0)
			if err != nil || len(rows) != 1 || rows[0].Action != "auth_login" {
				t.Fatalf("the chain holds %v, %v; want the recorded sign-in", rows, err)
			}
			if p := cfg.Env.AuditLogPath; p != "" {
				if b, err := os.ReadFile(p); err != nil || !strings.Contains(string(b), `"auth_login"`) {
					t.Errorf("the JSONL mirror holds %q, %v", b, err)
				}
			}
		})
	}

	t.Run("off/single operator", func(t *testing.T) {
		cfg := &config.Config{Env: config.Env{AuthToken: "legacy", ACLMode: "compat", SecretKey: key}}
		sink, _, _, err := openAuditSink(ctx, cfg, open(t))
		if err != nil || sink != nil {
			t.Fatalf("openAuditSink = %v, %v; want no sink", sink, err)
		}
	})
	t.Run("off/single operator keeps its file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.jsonl")
		cfg := &config.Config{Env: config.Env{AuditLogPath: path}}
		st := open(t)
		sink, _, desc, err := openAuditSink(ctx, cfg, st)
		if err != nil || sink == nil || len(desc) != 1 || desc[0] != "file:"+path {
			t.Fatalf("openAuditSink = %v, %v, %v; want the file only", sink, desc, err)
		}
		if err := sink.Record(audit.Event{Action: "auth_login"}); err != nil {
			t.Fatal(err)
		}
		if tenants, _ := st.AuditTenants(ctx); len(tenants) != 0 {
			t.Errorf("the store holds chains %q; want none", tenants)
		}
	})

	t.Run("refused/no secret key", func(t *testing.T) {
		cfg := &config.Config{ResolvedAuth: config.AuthPolicy{Gates: gate}}
		_, _, _, err := openAuditSink(ctx, cfg, open(t))
		if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_SECRET_KEY") {
			t.Fatalf("err = %v; want a refusal naming LOOMCYCLE_SECRET_KEY", err)
		}
	})
	t.Run("refused/the store cannot serve it", func(t *testing.T) {
		cfg := &config.Config{ResolvedAuth: config.AuthPolicy{Gates: gate}, Env: config.Env{SecretKey: key}}
		st := open(t)
		_ = st.Close()
		if _, _, _, err := openAuditSink(ctx, cfg, st); err == nil {
			t.Fatal("a closed store opened the stream; want a refusal")
		}
	})
	t.Run("refused/no store", func(t *testing.T) {
		cfg := &config.Config{ResolvedAuth: config.AuthPolicy{Gates: gate}, Env: config.Env{SecretKey: key}}
		if _, _, _, err := openAuditSink(ctx, cfg, nil); err == nil {
			t.Fatal("no store opened the stream; want a refusal")
		}
	})
}

// A live operator token in the store is a principal besides the legacy
// token: such a deployment is multi-principal, so the stream is required
// (on with a key, a refused boot without one or with LOOMCYCLE_AUDIT_STORE=0).
// A retired token does not count.
func TestAudit_AStoredOperatorTokenMakesTheStreamRequired(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T, retired bool) *sqlite.Store {
		t.Helper()
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		row := store.OperatorTokenDefRow{DefID: "def_00000000000000a1", Name: "ops", TenantID: "acme", Subject: "ops",
			TokenHash: auth.HashToken("", "lct_ops"), AllowedScopes: []string{"runs:create"}}
		if retired {
			row.RetiredAt = time.Now().Add(-time.Hour)
		}
		if _, err := s.OperatorTokenDefCreate(ctx, row); err != nil {
			t.Fatal(err)
		}
		return s
	}
	legacy := func(env config.Env) *config.Config {
		env.AuthToken = "legacy"
		return &config.Config{Env: env}
	}

	sink, stream, _, err := openAuditSink(ctx, legacy(config.Env{SecretKey: testSecretKey()}), open(t, false))
	if err != nil || sink == nil || stream == nil {
		t.Fatalf("legacy token + a live operator token, with a key: %v, %v, %v; want the store-backed stream", sink, stream, err)
	}
	_, _, _, err = openAuditSink(ctx, legacy(config.Env{}), open(t, false))
	if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_SECRET_KEY") || !strings.Contains(err.Error(), "more than one principal") {
		t.Fatalf("no key: err = %v; want a refusal naming LOOMCYCLE_SECRET_KEY and the principals", err)
	}
	_, _, _, err = openAuditSink(ctx, legacy(config.Env{SecretKey: testSecretKey(), AuditStoreOff: true}), open(t, false))
	if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_AUDIT_STORE=0") {
		t.Fatalf("LOOMCYCLE_AUDIT_STORE=0: err = %v; want a refusal", err)
	}
	sink, _, _, err = openAuditSink(ctx, legacy(config.Env{}), open(t, true))
	if err != nil || sink != nil {
		t.Fatalf("legacy token + a retired operator token: %v, %v; want no sink (single operator)", sink, err)
	}
}

const auditGateBootYAML = openModeBootYAML + `auth:
  gates:
    paste: { kind: token }
`

// The real binary: a gate with no LOOMCYCLE_SECRET_KEY does not start, the
// same gate with a key does and records to the store, and
// LOOMCYCLE_AUDIT_STORE=0 cannot switch the stream off.
func TestBoot_AuditStreamWithAGate(t *testing.T) {
	b := newOpenModeBoot(t)
	if err := os.WriteFile(b.cfgPath, []byte(auditGateBootYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"LOOMCYCLE_AUTH_TOKEN": "legacy-test-bearer"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}

	res := b.boot(t, "127.0.0.1", with())
	if res.served || res.exitErr == nil || !strings.Contains(res.log, "LOOMCYCLE_SECRET_KEY") {
		t.Errorf("a gate with no secret key: served=%v exit=%v; want a refused boot naming LOOMCYCLE_SECRET_KEY\n%s",
			res.served, res.exitErr, res.log)
	}
	res = b.boot(t, "127.0.0.1", with("LOOMCYCLE_SECRET_KEY", testSecretKey(), "LOOMCYCLE_AUDIT_STORE", "0"))
	if res.served || !strings.Contains(res.log, "LOOMCYCLE_AUDIT_STORE=0") {
		t.Errorf("LOOMCYCLE_AUDIT_STORE=0 with a gate: served=%v; want a refused boot\n%s", res.served, res.log)
	}
	res = b.boot(t, "127.0.0.1", with("LOOMCYCLE_SECRET_KEY", testSecretKey()))
	if !res.served || !strings.Contains(res.log, "audit: recording") {
		t.Errorf("a gate with a secret key: served=%v; want a served boot recording to the store\n%s", res.served, res.log)
	}
}

// A single-operator deployment (the legacy token, no gate, compat ACL) boots
// as before, with no secret key and no audit sink.
func TestBoot_SingleOperatorNeedsNoAuditStream(t *testing.T) {
	b := newOpenModeBoot(t)
	res := b.boot(t, "127.0.0.1", map[string]string{"LOOMCYCLE_AUTH_TOKEN": "legacy-test-bearer"})
	if !res.served || !strings.Contains(res.log, "no audit sink") {
		t.Errorf("single operator: served=%v; want a served boot with no audit sink\n%s", res.served, res.log)
	}
}

// An audit retention age needs the key that signs the anchor a prune leaves;
// none keeps every row and needs nothing.
func TestAuditRetentionKeys_RetentionNeedsTheKey(t *testing.T) {
	if seal, err := auditRetentionKeys(&config.Config{}); seal != nil || err != nil {
		t.Errorf("no retention: %v, %v; want nothing", seal, err)
	}
	cfg := &config.Config{Env: config.Env{AuditRetention: 24 * time.Hour}}
	if _, err := auditRetentionKeys(cfg); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_SECRET_KEY") {
		t.Errorf("retention with no key: %v; want an error naming LOOMCYCLE_SECRET_KEY", err)
	}
	cfg.Env.SecretKey = testSecretKey()
	if seal, err := auditRetentionKeys(cfg); seal == nil || err != nil {
		t.Errorf("retention with a key: %v, %v; want a signer", seal, err)
	}
}
