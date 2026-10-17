package credential

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

var allScopes = Scopes{Agent: true, User: true, Tenant: true}

func tenantKey(name string) Identity { return Identity{TenantID: "t", Scope: "tenant", Name: name} }

func mustPutKey(t *testing.T, e *Engine, id Identity, keyType string, material string) Meta {
	t.Helper()
	m, err := e.PutCryptoKey(context.Background(), id, keyType, []byte(material), nil)
	if err != nil {
		t.Fatalf("PutCryptoKey(%s/%s/%s): %v", id.Scope, id.ScopeID, id.Name, err)
	}
	return m
}

func mustRotate(t *testing.T, e *Engine, id Identity, material string) Meta {
	t.Helper()
	m, err := e.RotateCryptoKey(context.Background(), id, []byte(material))
	if err != nil {
		t.Fatalf("RotateCryptoKey: %v", err)
	}
	return m
}

// tamperDef rewrites the stored definition of id behind the engine's back,
// as a database writer could.
func tamperDef(t *testing.T, e *Engine, id Identity, edit func(m map[string]any)) {
	t.Helper()
	ctx := context.Background()
	row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(row.Definition, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	if row.Definition, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CredentialDefPut(ctx, row); err != nil {
		t.Fatal(err)
	}
}

func versionNumbers(vs []KeyVersion) (nums []int, retired []int) {
	for _, v := range vs {
		nums = append(nums, v.Version)
		if v.Retired {
			retired = append(retired, v.Version)
		}
	}
	return nums, retired
}

func TestCryptoKey_LifecycleAndRefusals(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := tenantKey("signer")

	m := mustPutKey(t, e, id, "ed25519", "material-1")
	if m.Definition != nil {
		t.Fatal("PutCryptoKey returned the sealed definition")
	}
	if m.Usage != UsageCrypto || m.KeyType != "ed25519" || m.CurrentVersion != 1 || m.MinVersion != 1 || len(m.Versions) != 1 || m.Versions[0].CreatedAt.IsZero() {
		t.Fatalf("meta after put = %+v", m)
	}

	// Create-only: a second create, and an ordinary write over it, are refused.
	if _, err := e.PutCryptoKey(ctx, id, "ed25519", []byte("x"), nil); !errors.Is(err, ErrKeyExists) {
		t.Errorf("second PutCryptoKey err = %v, want ErrKeyExists", err)
	}
	if _, err := e.PutInline(ctx, id, "plain", nil, nil, nil); !errors.Is(err, ErrKeyExists) {
		t.Errorf("PutInline over a crypto key err = %v, want ErrKeyExists", err)
	}
	if _, err := e.PutInline(ctx, tenantKey("plain"), "v", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutCryptoKey(ctx, tenantKey("plain"), "ed25519", []byte("x"), nil); !errors.Is(err, ErrKeyExists) {
		t.Errorf("PutCryptoKey over an ordinary credential err = %v, want ErrKeyExists", err)
	}

	// Input checks.
	for _, tc := range []struct {
		name     string
		id       Identity
		keyType  string
		material string
	}{
		{"empty key_type", tenantKey("a"), "", "m"},
		{"uppercase key_type", tenantKey("a"), "Ed25519", "m"},
		{"long key_type", tenantKey("a"), strings.Repeat("a", 33), "m"},
		{"empty material", tenantKey("a"), "ed25519", ""},
		{"bad name", tenantKey("a b"), "ed25519", "m"},
		{"user scope without id", Identity{TenantID: "t", Scope: "user", Name: "a"}, "ed25519", "m"},
		{"tenant scope with id", Identity{TenantID: "t", Scope: "tenant", ScopeID: "x", Name: "a"}, "ed25519", "m"},
		{"unknown scope", Identity{TenantID: "t", Scope: "global", Name: "a"}, "ed25519", "m"},
	} {
		if _, err := e.PutCryptoKey(ctx, tc.id, tc.keyType, []byte(tc.material), nil); err == nil {
			t.Errorf("%s: PutCryptoKey accepted it", tc.name)
		}
	}
	if _, err := e.RotateCryptoKey(ctx, id, nil); err == nil {
		t.Error("RotateCryptoKey accepted empty material")
	}
	past := time.Now().Add(-time.Hour)
	if _, err := e.PutCryptoKey(ctx, tenantKey("stale"), "ed25519", []byte("m"), &past); err == nil {
		t.Error("PutCryptoKey accepted an expiry in the past")
	}

	disabled := newEngine(t, "")
	if _, err := disabled.PutCryptoKey(ctx, id, "ed25519", []byte("m"), nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("PutCryptoKey with no KEK err = %v, want ErrNoKey", err)
	}
	if _, err := disabled.RotateCryptoKey(ctx, id, []byte("m")); !errors.Is(err, ErrNoKey) {
		t.Errorf("RotateCryptoKey with no KEK err = %v, want ErrNoKey", err)
	}

	// Rotation appends and moves current.
	mustRotate(t, e, id, "material-2")
	m = mustRotate(t, e, id, "material-3")
	if nums, _ := versionNumbers(m.Versions); m.CurrentVersion != 3 || fmt.Sprint(nums) != "[1 2 3]" {
		t.Fatalf("after two rotations current=%d versions=%v", m.CurrentVersion, nums)
	}
	if _, err := e.RotateCryptoKey(ctx, tenantKey("plain"), []byte("m")); !errors.Is(err, ErrNotCryptoKey) {
		t.Errorf("RotateCryptoKey of an ordinary credential err = %v, want ErrNotCryptoKey", err)
	}
	var nf *store.ErrNotFound
	if _, err := e.RotateCryptoKey(ctx, tenantKey("absent"), []byte("m")); !errors.As(err, &nf) {
		t.Errorf("RotateCryptoKey of an absent key err = %v, want not-found", err)
	}

	// min_version: raise only, never above current.
	// current is 3: 5 (current+2) is beyond retire-all.
	for _, bad := range []int{0, 5} {
		if _, err := e.SetCryptoMinVersion(ctx, id, bad); !errors.Is(err, ErrBadVersion) {
			t.Errorf("SetCryptoMinVersion(%d) err = %v, want ErrBadVersion", bad, err)
		}
	}
	m, err := e.SetCryptoMinVersion(ctx, id, 2)
	if _, retired := versionNumbers(m.Versions); err != nil || m.MinVersion != 2 || fmt.Sprint(retired) != "[1]" {
		t.Fatalf("SetCryptoMinVersion(2) = min %d retired %v, %v", m.MinVersion, retired, err)
	}
	if _, err := e.SetCryptoMinVersion(ctx, id, 1); !errors.Is(err, ErrBadVersion) {
		t.Errorf("lowering min_version err = %v, want ErrBadVersion", err)
	}
	if m, err := e.SetCryptoMinVersion(ctx, id, 2); err != nil || m.MinVersion != 2 {
		t.Errorf("SetCryptoMinVersion to the same value = %d, %v; want a no-op", m.MinVersion, err)
	}
	if _, err := e.SetCryptoMinVersion(ctx, tenantKey("plain"), 1); !errors.Is(err, ErrNotCryptoKey) {
		t.Errorf("SetCryptoMinVersion of an ordinary credential err = %v", err)
	}

	// Only a retired version can be deleted.
	for _, v := range []int{2, 3} {
		if _, err := e.DeleteCryptoKeyVersion(ctx, id, v); !errors.Is(err, ErrKeyInUse) {
			t.Errorf("DeleteCryptoKeyVersion(%d) err = %v, want ErrKeyInUse", v, err)
		}
	}
	if _, err := e.DeleteCryptoKeyVersion(ctx, id, 9); !errors.Is(err, ErrBadVersion) {
		t.Errorf("DeleteCryptoKeyVersion(9) err = %v, want ErrBadVersion", err)
	}
	m, err = e.DeleteCryptoKeyVersion(ctx, id, 1)
	if nums, _ := versionNumbers(m.Versions); err != nil || fmt.Sprint(nums) != "[2 3]" {
		t.Fatalf("DeleteCryptoKeyVersion(1) = %v, %v", nums, err)
	}
	if _, err := e.DeleteCryptoKeyVersion(ctx, id, 1); !errors.Is(err, ErrBadVersion) {
		t.Errorf("deleting a deleted version err = %v, want ErrBadVersion", err)
	}
	if _, err := e.DeleteCryptoKeyVersion(ctx, tenantKey("plain"), 1); !errors.Is(err, ErrNotCryptoKey) {
		t.Errorf("DeleteCryptoKeyVersion of an ordinary credential err = %v", err)
	}

	// The whole key cannot be deleted while a version is unretired.
	if found, err := e.Delete(ctx, id); !errors.Is(err, ErrKeyInUse) || !found {
		t.Errorf("Delete of a live crypto key = %v, %v; want found and ErrKeyInUse", found, err)
	}
	if _, err := e.Get(ctx, id); err != nil {
		t.Fatalf("the refused Delete removed the key: %v", err)
	}
	// Ordinary deletes are unchanged.
	if found, err := e.Delete(ctx, tenantKey("plain")); err != nil || !found {
		t.Errorf("Delete of an ordinary credential = %v, %v", found, err)
	}
	if found, err := e.Delete(ctx, tenantKey("absent")); err != nil || found {
		t.Errorf("Delete of an absent credential = %v, %v", found, err)
	}
}

func TestCryptoKey_ListAndGetReportVersionsWithoutMaterial(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	mustPutKey(t, e, tenantKey("k"), "hmac-sha256", "top-secret-material")
	mustRotate(t, e, tenantKey("k"), "top-secret-material-2")
	list, err := e.List(ctx, "t", "tenant", "")
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	m := list[0]
	if m.Definition != nil || m.Usage != UsageCrypto || m.KeyType != "hmac-sha256" || m.CurrentVersion != 2 || len(m.Versions) != 2 {
		t.Fatalf("List meta = %+v", m)
	}
	if strings.Contains(fmt.Sprintf("%+v", m), "top-secret") {
		t.Fatal("metadata carries key material")
	}
}

func TestResolveCryptoKey_ReturnsOnlyUnretiredVersions(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := tenantKey("k")
	mustPutKey(t, e, id, "aes-256-gcm", "m1")
	mustRotate(t, e, id, "m2")
	mustRotate(t, e, id, "m3")
	if _, err := e.SetCryptoMinVersion(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	k, found, err := e.ResolveCryptoKey(ctx, "t", "", "", "k", allScopes)
	if err != nil || !found {
		t.Fatalf("ResolveCryptoKey = %v, %v", found, err)
	}
	if k.Name != "k" || k.Scope != "tenant" || k.KeyType != "aes-256-gcm" || k.Current != 3 || k.MinVersion != 2 {
		t.Errorf("key = %+v", k)
	}
	if len(k.Versions) != 2 || string(k.Versions[2]) != "m2" || string(k.Versions[3]) != "m3" {
		t.Errorf("versions = %d entries, want only 2 and 3", len(k.Versions))
	}
	if _, found, err := e.ResolveCryptoKey(ctx, "other", "", "", "k", allScopes); found || err != nil {
		t.Errorf("another tenant resolved it: %v, %v", found, err)
	}
}

func TestResolveCryptoKey_SkipsScopesNotAllowed(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "agent", ScopeID: "bot", Name: "only-agent"}, "ed25519", "agent-only")
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "agent", ScopeID: "bot", Name: "both"}, "ed25519", "agent-material")
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "both"}, "ed25519", "user-material")
	mustPutKey(t, e, tenantKey("both"), "ed25519", "tenant-material")

	// A name resolves only to the one allowed scope holding a crypto key of it;
	// held as a key in more than one, it is refused unless the caller narrows
	// to one scope. No precedence: a key never stands in for another scope's.
	for _, tc := range []struct {
		name      string
		key       string
		allowed   Scopes
		wantFound bool
		want      string
		wantScope string
		ambiguous bool
	}{
		{"only in a skipped agent scope", "only-agent", Scopes{User: true, Tenant: true}, false, "", "", false},
		{"agent allowed", "only-agent", Scopes{Agent: true}, true, "agent-only", "agent", false},
		{"user and tenant both hold it", "both", Scopes{User: true, Tenant: true}, false, "", "", true},
		{"pinned to tenant", "both", Scopes{Tenant: true}, true, "tenant-material", "tenant", false},
		{"agent and user both hold it", "both", Scopes{Agent: true, User: true}, false, "", "", true},
		{"pinned to agent", "both", Scopes{Agent: true}, true, "agent-material", "agent", false},
		{"pinned to user", "both", Scopes{User: true}, true, "user-material", "user", false},
		{"all allowed, every scope holds it", "both", allScopes, false, "", "", true},
		{"none allowed", "both", Scopes{}, false, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, found, err := e.ResolveCryptoKey(ctx, "t", "bot", "alice", tc.key, tc.allowed)
			if tc.ambiguous {
				if found || !errors.Is(err, ErrAmbiguousKey) || k.Versions != nil {
					t.Fatalf("found=%v err=%v; want ErrAmbiguousKey and no key", found, err)
				}
				return
			}
			if err != nil || found != tc.wantFound {
				t.Fatalf("found=%v err=%v; want found=%v", found, err, tc.wantFound)
			}
			if found && (string(k.Versions[1]) != tc.want || k.Scope != tc.wantScope) {
				t.Errorf("got %q from %s, want %q from %s", k.Versions[1], k.Scope, tc.want, tc.wantScope)
			}
		})
	}
}

func TestResolveCryptoKey_RefusesWithoutFallingThrough(t *testing.T) {
	ctx := context.Background()
	// One engine; each case uses its own name: the case's row at TENANT scope
	// (the one a name resolves to) over a good user key of the same name that
	// must not stand in when the tenant's cannot be used.
	e := newEngine(t, testKey(1))

	for i, tc := range []struct {
		name  string
		setup func(t *testing.T, e *Engine, userID Identity)
		want  error // nil: the tenant row is skipped and the user's key used
	}{
		{"expired", func(t *testing.T, e *Engine, userID Identity) {
			mustPutKey(t, e, userID, "ed25519", "m")
			row, _ := e.store.CredentialDefGet(ctx, userID.TenantID, userID.Scope, userID.ScopeID, userID.Name)
			past := time.Now().Add(-time.Minute)
			row.ExpiresAt = &past
			if _, err := e.store.CredentialDefPut(ctx, row); err != nil {
				t.Fatal(err)
			}
		}, ErrExpired},
		{"ordinary credential", func(t *testing.T, e *Engine, userID Identity) {
			// Not a key: skipped, so writing one takes no key from anyone.
			if _, err := e.PutInline(ctx, userID, "plain", nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"key_type edited", func(t *testing.T, e *Engine, userID Identity) {
			mustPutKey(t, e, userID, "ed25519", "m")
			tamperDef(t, e, userID, func(m map[string]any) { m["key_type"] = "aes-256-gcm" })
		}, ErrUnopenable},
		{"usage edited", func(t *testing.T, e *Engine, userID Identity) {
			mustPutKey(t, e, userID, "ed25519", "m")
			tamperDef(t, e, userID, func(m map[string]any) { m["usage"] = "signing" })
		}, ErrUnopenable},
		{"usage removed", func(t *testing.T, e *Engine, userID Identity) {
			mustPutKey(t, e, userID, "ed25519", "m")
			tamperDef(t, e, userID, func(m map[string]any) { delete(m, "usage") })
		}, ErrUnopenable},
		{"version numbers swapped", func(t *testing.T, e *Engine, userID Identity) {
			mustPutKey(t, e, userID, "ed25519", "m1")
			mustRotate(t, e, userID, "m2")
			tamperDef(t, e, userID, func(m map[string]any) {
				vs := m["versions"].([]any)
				a, b := vs[0].(map[string]any), vs[1].(map[string]any)
				a["value"], b["value"] = b["value"], a["value"]
			})
		}, ErrUnopenable},
		{"version renumbered", func(t *testing.T, e *Engine, userID Identity) {
			mustPutKey(t, e, userID, "ed25519", "m1")
			tamperDef(t, e, userID, func(m map[string]any) {
				m["versions"].([]any)[0].(map[string]any)["version"] = 2
				m["current"] = 2
			})
		}, ErrUnopenable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("k%d", i)
			mustPutKey(t, e, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: name}, "ed25519", "user-key")
			tc.setup(t, e, tenantKey(name))
			k, found, err := e.ResolveCryptoKey(ctx, "t", "", "alice", name, allScopes)
			if tc.want == nil {
				if err != nil || !found || string(k.Versions[1]) != "user-key" {
					t.Fatalf("ResolveCryptoKey = found %v key %+v err %v; want the user's key", found, k, err)
				}
				return
			}
			// Both scopes hold it as a key: refused, never the user's.
			if !errors.Is(err, ErrAmbiguousKey) || found || k.Versions != nil {
				t.Fatalf("all scopes: found %v key %+v err %v; want ErrAmbiguousKey and no key", found, k, err)
			}
			// Pinned to tenant: the tenant row's own reason, never the user's key.
			k, found, err = e.ResolveCryptoKey(ctx, "t", "", "alice", name, Scopes{Tenant: true})
			if !errors.Is(err, tc.want) || found || k.Versions != nil {
				t.Fatalf("tenant: found %v key %+v err %v; want %v and no key", found, k, err, tc.want)
			}
			if !HeldButUnusable(err) {
				t.Errorf("err %v is not held-but-unusable", err)
			}
		})
	}
}

// A crypto key never resolves on a path that sends or compares a value, and
// does not let a same-named ordinary credential further down stand in.
func TestCryptoKey_NeverResolvesAsASecret(t *testing.T) {
	ctx := netguard.WithPinnable(context.Background())
	const target = "https://api.example.com/v1"
	e := newEngine(t, testKey(1))
	if _, err := e.PutInline(ctx, tenantKey("k"), "ordinary-secret", nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		id          Identity
		agent, user string
	}{
		{"user scope over a tenant credential", Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "k"}, "", "alice"},
		{"agent scope over a tenant credential", Identity{TenantID: "t", Scope: "agent", ScopeID: "bot", Name: "k"}, "bot", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustPutKey(t, e, tc.id, "hmac-sha256", "key-material")
			defer func() {
				// Retire every version, then delete the key.
				if _, err := e.SetCryptoMinVersion(ctx, tc.id, 2); err != nil {
					t.Fatal(err)
				}
				if found, err := e.Delete(ctx, tc.id); err != nil || !found {
					t.Fatalf("Delete after retire-all = %v, %v", found, err)
				}
			}()
			check := func(op string, err error, leaked bool) {
				t.Helper()
				if !errors.Is(err, ErrCryptoKey) || !HeldButUnusable(err) {
					t.Errorf("%s: err = %v, want a held-but-unusable ErrCryptoKey", op, err)
				}
				if leaked {
					t.Errorf("%s: handed out a value", op)
				}
				if err != nil && strings.Contains(err.Error(), "key-material") {
					t.Errorf("%s: the error carries the material", op)
				}
			}
			res, found, err := e.Resolve(ctx, "t", tc.agent, tc.user, "k")
			check("Resolve", err, found || res.Value != "")
			res, found, err = e.ResolveFor(ctx, "t", tc.agent, tc.user, "k", target)
			check("ResolveFor", err, found || res.Value != "")
			out, unresolved, err := e.Substitute(ctx, "t", tc.agent, tc.user, "Bearer $cred:k", target, func(string) { t.Error("Substitute registered a value") })
			check("Substitute", err, out != "" || unresolved != nil)
			has, err := e.HasKey(ctx, "t", tc.agent, tc.user, "k")
			check("HasKey", err, has)
			found, usable, err := e.HasKeyFor(ctx, "t", tc.agent, tc.user, "k", target)
			check("HasKeyFor", err, usable)
			if !found {
				t.Error("HasKeyFor: a held crypto key reported as not found")
			}
		})
	}

	// The compare reads look at the tenant scope only.
	mustPutKey(t, e, tenantKey("cmp"), "hmac-sha256", "key-material")
	if v, found, err := e.ResolveToCompare(ctx, "t", "cmp"); !errors.Is(err, ErrCryptoKey) || found || v != "" {
		t.Errorf("ResolveToCompare = %q, %v, %v; want ErrCryptoKey", v, found, err)
	}
	if found, err := e.HasToCompare(ctx, "t", "cmp"); !errors.Is(err, ErrCryptoKey) || found {
		t.Errorf("HasToCompare = %v, %v; want ErrCryptoKey", found, err)
	}
	// The ordinary tenant credential still resolves for a run with no
	// identity that reaches the crypto keys.
	if res, found, err := e.Resolve(ctx, "t", "", "bob", "k"); err != nil || !found || res.Value != "ordinary-secret" {
		t.Errorf("Resolve for bob = %q, %v, %v", res.Value, found, err)
	}
}

// A version's seal moved into an ordinary credential's value field does not
// open: the crypto binding is part of what it was sealed with.
func TestCryptoKey_SealDoesNotOpenAsAnOrdinaryCredential(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := tenantKey("k")
	mustPutKey(t, e, id, "hmac-sha256", "key-material")
	tamperDef(t, e, id, func(m map[string]any) {
		v := m["versions"].([]any)[0].(map[string]any)["value"]
		for k := range m {
			delete(m, k)
		}
		m["value"] = v
	})
	res, found, err := e.Resolve(ctx, "t", "", "", "k")
	if !errors.Is(err, ErrUnopenable) || found || res.Value != "" {
		t.Fatalf("Resolve of a disguised crypto seal = %q, %v, %v; want ErrUnopenable", res.Value, found, err)
	}
}

func TestPutInline_OrdinaryDefinitionCarriesNoCryptoFields(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	m, err := e.PutInline(ctx, tenantKey("plain"), "v", nil, urls("https://api.example.com"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Usage != "" || m.KeyType != "" || m.Versions != nil || m.CurrentVersion != 0 || m.MinVersion != 0 {
		t.Errorf("ordinary meta carries crypto fields: %+v", m)
	}
	row, err := e.store.CredentialDefGet(ctx, "t", "tenant", "", "plain")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row.Definition, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["value"] == nil || fields["allowed_urls"] == nil {
		t.Errorf("ordinary definition fields = %v, want exactly value and allowed_urls", fields)
	}
	if isCryptoDef(row.Definition) {
		t.Error("an ordinary definition is classified as a crypto key")
	}
}

func TestResolveKeyMaterial(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutInline(ctx, tenantKey("seed"), "JBSWY3DPEHPK3PXP", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(ctx, tenantKey("dup"), "tenant-value", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "dup"}, "hmac-sha256", "key-material")
	if _, err := e.PutInline(ctx, tenantKey("scoped"), "v", nil, urls("https://api.example.com"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "mine"}, "user-value", nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	res, found, err := e.ResolveKeyMaterial(ctx, "t", "", "alice", "seed", allScopes)
	if err != nil || !found || res.Value != "JBSWY3DPEHPK3PXP" || res.Scope != "tenant" {
		t.Fatalf("ordinary = %+v, %v, %v", res, found, err)
	}
	// Key material resolves as $cred: does (the narrowest scope first): the
	// user's crypto key "dup" is found, refused, and never falls through to
	// the tenant's ordinary value.
	if res, found, err := e.ResolveKeyMaterial(ctx, "t", "", "alice", "dup", allScopes); !errors.Is(err, ErrUseKeyName) || found || res.Value != "" {
		t.Errorf("crypto key as material = %q, %v, %v; want ErrUseKeyName without falling through", res.Value, found, err)
	}
	if _, found, err := e.ResolveKeyMaterial(ctx, "t", "", "alice", "scoped", allScopes); !errors.Is(err, ErrRestricted) || found {
		t.Errorf("restricted = %v, %v; want ErrRestricted", found, err)
	}
	if _, found, err := e.ResolveKeyMaterial(ctx, "t", "", "alice", "mine", Scopes{Tenant: true}); err != nil || found {
		t.Errorf("only in a skipped user scope = %v, %v; want not found", found, err)
	}
	if res, found, err := e.ResolveKeyMaterial(ctx, "t", "", "alice", "dup", Scopes{Tenant: true}); err != nil || !found || res.Value != "tenant-value" {
		t.Errorf("user skipped = %q, %v, %v; want the tenant's", res.Value, found, err)
	}

	row, _ := e.store.CredentialDefGet(ctx, "t", "tenant", "", "seed")
	past := time.Now().Add(-time.Minute)
	row.ExpiresAt = &past
	if _, err := e.store.CredentialDefPut(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, found, err := e.ResolveKeyMaterial(ctx, "t", "", "", "seed", allScopes); !errors.Is(err, ErrExpired) || found {
		t.Errorf("expired = %v, %v; want ErrExpired", found, err)
	}
}

func TestCryptoKeyTransfer_RoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newEngine(t, testKey(1))
	id := Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "k"}
	expires := time.Now().Add(240 * time.Hour).UTC().Truncate(time.Second)
	if _, err := src.PutCryptoKey(ctx, id, "aes-256-gcm", []byte("m1"), &expires); err != nil {
		t.Fatal(err)
	}
	mustRotate(t, src, id, "m2")
	mustRotate(t, src, id, "m3")
	if _, err := src.SetCryptoMinVersion(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	want, _ := src.Get(ctx, id)
	const pass = "correct horse battery staple"

	if _, err := src.ExportCryptoKey(ctx, id, "short-pass"); err == nil {
		t.Error("ExportCryptoKey accepted a passphrase under 16 characters")
	}
	if _, err := src.PutInline(ctx, tenantKey("plain"), "v", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ExportCryptoKey(ctx, tenantKey("plain"), pass); !errors.Is(err, ErrNotCryptoKey) {
		t.Errorf("exporting an ordinary credential err = %v, want ErrNotCryptoKey", err)
	}
	blob, err := src.ExportCryptoKey(ctx, id, pass)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(blob, base64.StdEncoding.EncodeToString([]byte("m1"))) || strings.Contains(blob, "m2") {
		t.Fatal("the blob carries material in the clear")
	}

	dst := newEngine(t, testKey(2)) // another deployment, another KEK
	if _, err := dst.ImportCryptoKey(ctx, id, blob, "wrong horse battery staple"); !errors.Is(err, ErrKeyTransfer) {
		t.Errorf("wrong passphrase err = %v, want ErrKeyTransfer", err)
	}
	if _, err := dst.ImportCryptoKey(ctx, id, tamperBlob(t, blob), pass); !errors.Is(err, ErrKeyTransfer) {
		t.Errorf("tampered blob err = %v, want ErrKeyTransfer", err)
	}
	if _, err := dst.ImportCryptoKey(ctx, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "other"}, blob, pass); err == nil {
		t.Error("import under another name was accepted")
	}
	if _, err := newEngine(t, "").ImportCryptoKey(ctx, id, blob, pass); !errors.Is(err, ErrNoKey) {
		t.Errorf("import with no KEK err = %v, want ErrNoKey", err)
	}

	got, err := dst.ImportCryptoKey(ctx, id, blob, pass)
	if err != nil {
		t.Fatalf("ImportCryptoKey: %v", err)
	}
	if got.KeyType != want.KeyType || got.CurrentVersion != 3 || got.MinVersion != 2 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("imported meta = %+v, want %+v", got, want)
	}
	if len(got.Versions) != 3 {
		t.Fatalf("imported versions = %+v", got.Versions)
	}
	for i := range got.Versions {
		if got.Versions[i].Version != want.Versions[i].Version || got.Versions[i].Retired != want.Versions[i].Retired ||
			!got.Versions[i].CreatedAt.Equal(want.Versions[i].CreatedAt) {
			t.Errorf("version %d = %+v, want %+v", i, got.Versions[i], want.Versions[i])
		}
	}
	k, found, err := dst.ResolveCryptoKey(ctx, "t", "", "alice", "k", allScopes)
	if err != nil || !found || string(k.Versions[2]) != "m2" || string(k.Versions[3]) != "m3" || len(k.Versions) != 2 {
		t.Fatalf("resolved import = %d versions, %v, %v", len(k.Versions), found, err)
	}
	// The retired version travelled too.
	_, d, err := dst.getCryptoRow(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := dst.openVersion(id, d, d.Versions[0]); err != nil || string(pt) != "m1" {
		t.Errorf("retired version 1 on the target = %q, %v", pt, err)
	}

	// Import never overwrites: a crypto key or an ordinary credential there.
	if _, err := dst.ImportCryptoKey(ctx, id, blob, pass); !errors.Is(err, ErrKeyExists) {
		t.Errorf("import over a crypto key err = %v, want ErrKeyExists", err)
	}
	plainID := Identity{TenantID: "t", Scope: "user", ScopeID: "bob", Name: "k"}
	if _, err := dst.PutInline(ctx, plainID, "v", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ImportCryptoKey(ctx, plainID, blob, pass); !errors.Is(err, ErrKeyExists) {
		t.Errorf("import over an ordinary credential err = %v, want ErrKeyExists", err)
	}
}

// tamperBlob flips one byte of the wrapped ciphertext and re-encodes.
func tamperBlob(t *testing.T, blob string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	ct, err := base64.StdEncoding.DecodeString(env["ciphertext"].(string))
	if err != nil {
		t.Fatal(err)
	}
	ct[len(ct)/2] ^= 1
	env["ciphertext"] = base64.StdEncoding.EncodeToString(ct)
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(out)
}

// A version sealed under the previous KEK still opens after a KEK rotation,
// and versions under both keys resolve together.
func TestCryptoKey_OpensUnderThePreviousKEK(t *testing.T) {
	ctx := context.Background()
	st := sharedStore(t)
	id := tenantKey("k")
	old := engineOver(t, st, testKey(1))
	mustPutKey(t, old, id, "ed25519", "m1")

	sealer, err := NewSealer(testKey(2), testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	rotated := NewEngine(st, sealer)
	mustRotate(t, rotated, id, "m2")
	k, found, err := rotated.ResolveCryptoKey(ctx, "t", "", "", "k", allScopes)
	if err != nil || !found || string(k.Versions[1]) != "m1" || string(k.Versions[2]) != "m2" {
		t.Fatalf("after KEK rotation = %d versions, %v, %v", len(k.Versions), found, err)
	}
	// Without the previous KEK, version 1 is unopenable and the key refuses.
	if _, _, err := engineOver(t, st, testKey(2)).ResolveCryptoKey(ctx, "t", "", "", "k", allScopes); !errors.Is(err, ErrUnopenable) {
		t.Errorf("without _PREVIOUS err = %v, want ErrUnopenable", err)
	}
}

// Concurrent rotations in one process are all recorded.
func TestRotateCryptoKey_ConcurrentRotationsAreNotLost(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := tenantKey("k")
	mustPutKey(t, e, id, "aes-256-gcm", "m0")
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.RotateCryptoKey(ctx, id, []byte(fmt.Sprintf("m%d", i+1))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	k, _, err := e.ResolveCryptoKey(ctx, "t", "", "", "k", allScopes)
	if err != nil || k.Current != n+1 || len(k.Versions) != n+1 {
		t.Fatalf("after %d concurrent rotations current=%d versions=%d err=%v", n, k.Current, len(k.Versions), err)
	}
	seen := map[string]bool{}
	for _, m := range k.Versions {
		seen[string(m)] = true
	}
	if len(seen) != n+1 || !bytes.Equal(k.Versions[1], []byte("m0")) {
		t.Errorf("materials = %v", seen)
	}
}

// min_version may go to current+1, retiring every version: the key then
// resolves with no usable version, may be deleted, and a rotation gives it a
// usable one again. Beyond current+1 is refused.
func TestCryptoKey_RetireAll(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	retired, rotated := tenantKey("retired"), tenantKey("rotated")
	for _, id := range []Identity{retired, rotated} {
		mustPutKey(t, e, id, "aes-256-gcm", "m1")
		mustRotate(t, e, id, "m2")
	}

	if _, err := e.SetCryptoMinVersion(ctx, retired, 4); !errors.Is(err, ErrBadVersion) {
		t.Errorf("min_version current+2 err = %v, want ErrBadVersion", err)
	}
	if found, err := e.Delete(ctx, retired); !errors.Is(err, ErrKeyInUse) || !found {
		t.Errorf("Delete before retire-all = %v, %v; want ErrKeyInUse", found, err)
	}
	m, err := e.SetCryptoMinVersion(ctx, retired, 3)
	if _, ret := versionNumbers(m.Versions); err != nil || m.MinVersion != 3 || fmt.Sprint(ret) != "[1 2]" {
		t.Fatalf("retire-all = min %d retired %v, %v", m.MinVersion, ret, err)
	}
	k, found, err := e.ResolveCryptoKey(ctx, "t", "", "", "retired", allScopes)
	if err != nil || !found || k.Versions == nil || len(k.Versions) != 0 || k.Current != 2 || k.MinVersion != 3 {
		t.Fatalf("resolve after retire-all = %+v, %v, %v; want found with no versions", k, found, err)
	}
	// Retired versions can be deleted one by one, and the key as a whole.
	if _, err := e.DeleteCryptoKeyVersion(ctx, retired, 2); err != nil {
		t.Fatalf("DeleteCryptoKeyVersion(2) after retire-all: %v", err)
	}
	if found, err := e.Delete(ctx, retired); err != nil || !found {
		t.Fatalf("Delete after retire-all = %v, %v", found, err)
	}
	if _, err := e.Get(ctx, retired); err == nil {
		t.Fatal("the key is still stored after Delete")
	}

	// Rotation after retire-all gives a usable version 3.
	if _, err := e.SetCryptoMinVersion(ctx, rotated, 3); err != nil {
		t.Fatal(err)
	}
	m = mustRotate(t, e, rotated, "m3")
	if m.CurrentVersion != 3 || m.MinVersion != 3 {
		t.Fatalf("rotate after retire-all meta = current %d min %d", m.CurrentVersion, m.MinVersion)
	}
	k, found, err = e.ResolveCryptoKey(ctx, "t", "", "", "rotated", allScopes)
	if err != nil || !found || len(k.Versions) != 1 || string(k.Versions[3]) != "m3" {
		t.Fatalf("resolve after rotate = %d versions, %v, %v; want only version 3", len(k.Versions), found, err)
	}
	if found, err := e.Delete(ctx, rotated); !errors.Is(err, ErrKeyInUse) || !found {
		t.Errorf("Delete with a usable version again = %v, %v; want ErrKeyInUse", found, err)
	}

	// A key with every version retired AND deleted has nothing left to
	// authenticate its key_type, so it is not rotated.
	if _, err := e.SetCryptoMinVersion(ctx, rotated, 4); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{1, 2, 3} {
		if _, err := e.DeleteCryptoKeyVersion(ctx, rotated, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.RotateCryptoKey(ctx, rotated, []byte("m4")); !errors.Is(err, ErrBadVersion) {
		t.Errorf("rotate with no stored version err = %v, want ErrBadVersion", err)
	}
	if k, found, err := e.ResolveCryptoKey(ctx, "t", "", "", "rotated", allScopes); err != nil || !found || len(k.Versions) != 0 {
		t.Errorf("resolve of an emptied key = %d versions, %v, %v", len(k.Versions), found, err)
	}
}

// The crypto binding cannot be confused with a URL restriction.
func TestCryptoBound_DiffersFromURLBinding(t *testing.T) {
	c := cryptoBound(UsageCrypto, "ed25519", 1)
	u := boundURLs(&[]string{"https://a"})
	empty := boundURLs(&[]string{})
	if bytes.HasPrefix(c, []byte("urls\x1e")) || bytes.HasPrefix(u, []byte("crypto\x1e")) || bytes.Equal(c, empty) {
		t.Errorf("bindings collide: %q %q %q", c, u, empty)
	}
	if bytes.Equal(cryptoBound(UsageCrypto, "ed25519", 1), cryptoBound(UsageCrypto, "ed25519", 11)) {
		t.Error("versions 1 and 11 bind the same")
	}
}

// No scope's key stands in for another's under the same name. A user who
// stores a key under the tenant's name, or under the agent's own (shared)
// key's name, has it used neither in that key's place nor at all while both
// exist: the name is refused unless the caller names the scope. Pinned, the
// caller gets exactly the scope it named. An ordinary credential written
// under the name takes no key from anyone.
func TestResolveCryptoKey_NoScopesKeyStandsInForAnothers(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, testKey(1))
	mustPutKey(t, e, tenantKey("signing"), "ed25519", "tenant-signing-key")
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "user", ScopeID: "mallory", Name: "signing"}, "ed25519", "mallorys-own-key")
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "agent", ScopeID: "bot", Name: "approvals"}, "ed25519", "agents-own-key")
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "user", ScopeID: "mallory", Name: "approvals"}, "ed25519", "mallorys-approvals")

	for _, c := range []struct{ agent, user, name string }{
		{"", "mallory", "signing"},      // a user's key under the tenant key's name
		{"bot", "mallory", "approvals"}, // a user's key under the agent's own key's name
	} {
		k, found, err := e.ResolveCryptoKey(ctx, "t", c.agent, c.user, c.name, allScopes)
		if found || !errors.Is(err, ErrAmbiguousKey) || k.Versions != nil {
			t.Errorf("%s for agent=%q user=%q: %q (found %v err %v); want ErrAmbiguousKey and no key",
				c.name, c.agent, c.user, k.Versions[1], found, err)
		}
	}
	// Pinned: exactly the named scope.
	if k, _, err := e.ResolveCryptoKey(ctx, "t", "", "mallory", "signing", Scopes{Tenant: true}); err != nil || string(k.Versions[1]) != "tenant-signing-key" {
		t.Errorf("pinned to tenant: %q %v", k.Versions[1], err)
	}
	if k, _, err := e.ResolveCryptoKey(ctx, "t", "bot", "mallory", "approvals", Scopes{Agent: true}); err != nil || string(k.Versions[1]) != "agents-own-key" {
		t.Errorf("pinned to agent: %q %v", k.Versions[1], err)
	}
	// Someone who holds no key of the name gets the one key there is.
	if k, found, err := e.ResolveCryptoKey(ctx, "t", "", "alice", "signing", allScopes); err != nil || !found || string(k.Versions[1]) != "tenant-signing-key" {
		t.Errorf("alice: %+v %v %v; want the tenant key", k, found, err)
	}

	// A user's own key with no tenant key of the name is theirs; an ordinary
	// tenant credential written under that name later does not take it away.
	mustPutKey(t, e, Identity{TenantID: "t", Scope: "user", ScopeID: "alice", Name: "mine"}, "ed25519", "alices-key")
	if _, err := e.PutInline(ctx, tenantKey("mine"), "an ordinary value", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	k, found, err := e.ResolveCryptoKey(ctx, "t", "", "alice", "mine", allScopes)
	if err != nil || !found || string(k.Versions[1]) != "alices-key" {
		t.Errorf("alice's own key after an ordinary tenant credential of the name: %+v %v %v", k, found, err)
	}
}

func TestRotateCryptoKey_CapsUsableVersions(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, testKey(1))
	id := tenantKey("busy")
	mustPutKey(t, e, id, "aes-256-gcm", "v1")
	for i := 2; i <= MaxUsableKeyVersions; i++ {
		mustRotate(t, e, id, fmt.Sprintf("v%d", i))
	}
	if _, err := e.RotateCryptoKey(ctx, id, []byte("one-too-many")); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("rotation past %d usable versions: %v, want ErrBadVersion", MaxUsableKeyVersions, err)
	}
	if _, err := e.SetCryptoMinVersion(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RotateCryptoKey(ctx, id, []byte("after-retiring-one")); err != nil {
		t.Errorf("rotation after retiring a version: %v", err)
	}
}
