package credential

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestExternalCryptoKey_ResolvesToItsReferenceAndRefusesMaterialOps(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := tenantKey("orders")
	ref := []byte(`{"kind":"vault","address":"https://vault.example","key":"orders","token_cred":"$cred:vt"}`)
	m, err := e.PutExternalCryptoKey(ctx, id, "aes-256-gcm", ExternalKey{Kind: "vault", Ref: ref}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Usage != UsageCrypto || m.KeyBackend != "vault" || m.KeyType != "aes-256-gcm" {
		t.Errorf("meta = %+v", m)
	}
	k, found, err := e.ResolveCryptoKey(ctx, "t", "", "", "orders", allScopes)
	if err != nil || !found {
		t.Fatalf("resolve = %v, %v", found, err)
	}
	if k.External == nil || k.External.Kind != "vault" || !bytes.Equal(k.External.Ref, ref) || len(k.Versions) != 0 {
		t.Errorf("resolved = %+v", k)
	}
	// Never a secret, like any crypto key.
	if _, _, err := e.Resolve(ctx, "t", "", "", "orders"); !errors.Is(err, ErrCryptoKey) {
		t.Errorf("Resolve = %v, want ErrCryptoKey", err)
	}
	if _, err := e.RotateCryptoKey(ctx, id, []byte("m")); !errors.Is(err, ErrExternalKey) {
		t.Errorf("rotate = %v, want ErrExternalKey", err)
	}
	if _, err := e.SetCryptoMinVersion(ctx, id, 2); !errors.Is(err, ErrExternalKey) {
		t.Errorf("set_min_version = %v, want ErrExternalKey", err)
	}
	if _, err := e.DeleteCryptoKeyVersion(ctx, id, 1); !errors.Is(err, ErrExternalKey) {
		t.Errorf("delete_version = %v, want ErrExternalKey", err)
	}
	if _, err := e.ExportCryptoKey(ctx, id, "a long enough passphrase"); !errors.Is(err, ErrExternalKey) {
		t.Errorf("export = %v, want ErrExternalKey", err)
	}
	// Delete removes the reference with nothing to retire first.
	if found, err := e.Delete(ctx, id); err != nil || !found {
		t.Errorf("delete = %v, %v", found, err)
	}
}

// The reference is sealed with usage, key_type and backend: a database writer
// who repoints any of them gets a key that does not open, not one that sends
// its calls somewhere else or runs another algorithm.
func TestExternalCryptoKey_AnEditedRowDoesNotOpen(t *testing.T) {
	for name, edit := range map[string]func(m map[string]any){
		"backend":  func(m map[string]any) { m["backend"] = "aws_kms" },
		"key_type": func(m map[string]any) { m["key_type"] = "hmac-sha256" },
		"versions": func(m map[string]any) { m["versions"] = []any{map[string]any{"version": 1}} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEngine(t, testKey(1))
			ctx := context.Background()
			id := tenantKey("orders")
			if _, err := e.PutExternalCryptoKey(ctx, id, "aes-256-gcm", ExternalKey{Kind: "vault", Ref: []byte(`{"kind":"vault"}`)}, nil); err != nil {
				t.Fatal(err)
			}
			tamperDef(t, e, id, edit)
			if _, _, err := e.ResolveCryptoKey(ctx, "t", "", "", "orders", allScopes); !errors.Is(err, ErrUnopenable) {
				t.Errorf("resolve after editing %s = %v, want ErrUnopenable", name, err)
			}
		})
	}
}

func TestResolveKMSSecret_ReadsOnlyTheNamedBucket(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	user := Identity{TenantID: "t", Scope: "user", ScopeID: "u1", Name: "tok"}
	if _, err := e.PutInline(ctx, user, "user-token", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, err := e.ResolveKMSSecret(ctx, Identity{TenantID: "t", Scope: "tenant", Name: "tok"}); found || err != nil {
		t.Errorf("tenant bucket = %v, %v; want absent (only the user holds it)", found, err)
	}
	if v, found, err := e.ResolveKMSSecret(ctx, user); !found || err != nil || v != "user-token" {
		t.Errorf("user bucket = %q, %v, %v", v, found, err)
	}
	restricted := []string{"https://vault.example"}
	if _, err := e.PutInline(ctx, tenantKey("r"), "x", nil, &restricted, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ResolveKMSSecret(ctx, tenantKey("r")); !errors.Is(err, ErrRestricted) {
		t.Errorf("restricted = %v, want ErrRestricted", err)
	}
}
