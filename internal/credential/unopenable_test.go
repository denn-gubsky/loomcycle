package credential

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// sharedStore is one credential store, so engines with different keys can
// look at the same rows (a key rotated without _PREVIOUS, or removed).
func sharedStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func engineOver(t *testing.T, st store.Store, kek string) *Engine {
	t.Helper()
	sealer, err := NewSealer(kek, "")
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(st, sealer)
}

// seed stores "gh" (value "sk-sealed") at tenant scope in tenant "t", sealed
// under key A.
func seed(t *testing.T, st store.Store) {
	t.Helper()
	if _, err := engineOver(t, st, testKey(1)).PutInline(context.Background(), Identity{TenantID: "t", Scope: "tenant", Name: "gh"}, "sk-sealed", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// checkUnopenable asserts err is the held-but-unusable ErrUnopenable, names
// the credential, and carries neither the value nor anything stored.
func checkUnopenable(t *testing.T, op string, err error, def []byte) {
	t.Helper()
	if !errors.Is(err, ErrUnopenable) || !HeldButUnusable(err) {
		t.Errorf("%s: err = %v, want a held-but-unusable ErrUnopenable", op, err)
		return
	}
	msg := err.Error()
	if !strings.Contains(msg, `"gh"`) {
		t.Errorf("%s: %q does not name the credential", op, msg)
	}
	if strings.Contains(msg, "sk-sealed") {
		t.Errorf("%s: the error carries the value", op)
	}
	if len(def) > 0 && strings.Contains(msg, string(def)) {
		t.Errorf("%s: the error carries the stored definition", op)
	}
}

// A row sealed under one key and read under another (a KEK rotated without
// LOOMCYCLE_SECRET_KEY_PREVIOUS, or simply wrong) is held but unusable on
// every path that opens a value — never a miss.
func TestEngine_AValueAnotherKeySealedIsUnopenableNotMissing(t *testing.T) {
	st := sharedStore(t)
	seed(t, st)
	row, err := st.CredentialDefGet(context.Background(), "t", "tenant", "", "gh")
	if err != nil {
		t.Fatal(err)
	}
	e := engineOver(t, st, testKey(2))
	ctx := netguard.WithPinnable(context.Background())

	_, found, err := e.Resolve(ctx, "t", "", "", "gh")
	checkUnopenable(t, "Resolve", err, row.Definition)
	if found {
		t.Error("Resolve reported found for a value it could not open")
	}
	_, _, err = e.ResolveFor(ctx, "t", "", "", "gh", "https://api.example.com")
	checkUnopenable(t, "ResolveFor", err, row.Definition)
	out, _, err := e.Substitute(ctx, "t", "", "", "Bearer $cred:gh", "https://api.example.com", nil)
	checkUnopenable(t, "Substitute", err, row.Definition)
	if out != "" {
		t.Errorf("Substitute produced %q for a value it could not open", out)
	}
	_, _, err = e.ResolveToCompare(ctx, "t", "gh")
	checkUnopenable(t, "ResolveToCompare", err, row.Definition)
}

// With no key configured at all, a stored row is held but unusable on the
// metadata-only checks too — HasKey must not answer "no key" and let the
// operator's stand in — while a name with no row is still a plain miss.
func TestEngine_NoKeyWithAStoredRowIsUnopenableAndNoRowIsAMiss(t *testing.T) {
	st := sharedStore(t)
	seed(t, st)
	e := engineOver(t, st, "")
	ctx := context.Background()

	has, err := e.HasKey(ctx, "t", "", "", "gh")
	checkUnopenable(t, "HasKey", err, nil)
	if has {
		t.Error("HasKey reported a usable key with no KEK")
	}
	found, usable, err := e.HasKeyFor(ctx, "t", "", "", "gh", "https://api.example.com")
	checkUnopenable(t, "HasKeyFor", err, nil)
	if !found || usable {
		t.Errorf("HasKeyFor = found %v usable %v; want found, not usable", found, usable)
	}
	ok, err := e.HasToCompare(ctx, "t", "gh")
	checkUnopenable(t, "HasToCompare", err, nil)
	if ok {
		t.Error("HasToCompare reported a usable secret with no KEK")
	}
	_, _, err = e.Resolve(ctx, "t", "", "", "gh")
	checkUnopenable(t, "Resolve", err, nil)
	if !errors.Is(err, ErrNoKey) {
		t.Errorf("Resolve: %v should say no key is configured", err)
	}
	_, _, err = e.ResolveToCompare(ctx, "t", "gh")
	checkUnopenable(t, "ResolveToCompare", err, nil)

	// No row: a miss, exactly as before.
	if has, err := e.HasKey(ctx, "t", "", "", "absent"); has || err != nil {
		t.Errorf("HasKey(absent) = %v, %v; want a plain miss", has, err)
	}
	if _, found, err := e.Resolve(ctx, "t", "", "", "absent"); found || err != nil {
		t.Errorf("Resolve(absent) = found %v err %v; want a plain miss", found, err)
	}
	if _, found, err := e.ResolveToCompare(ctx, "t", "absent"); found || err != nil {
		t.Errorf("ResolveToCompare(absent) = found %v err %v; want a plain miss", found, err)
	}
}

// A corrupt seal and a backend this build cannot read are unopenable too,
// and a parse error's text (which could quote stored bytes) is not passed on.
func TestEngine_ACorruptOrUnreadableRowIsUnopenable(t *testing.T) {
	st := sharedStore(t)
	ctx := context.Background()
	for name, row := range map[string]credentialdef.Row{
		"garbled": {Backend: BackendInline, Definition: []byte(`{"value":"not-a-seal-zzqq"}`)},
		"vaulted": {Backend: BackendVault, Definition: []byte(`{"path":"secret/x"}`)},
	} {
		row.TenantID, row.Scope, row.Name = "t", "tenant", name
		if _, err := st.CredentialDefPut(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	e := engineOver(t, st, testKey(1))
	for _, name := range []string{"garbled", "vaulted"} {
		_, found, err := e.Resolve(ctx, "t", "", "", name)
		if !errors.Is(err, ErrUnopenable) || found {
			t.Errorf("Resolve(%s) = found %v err %v; want ErrUnopenable", name, found, err)
		}
		if err != nil && strings.Contains(err.Error(), "zzqq") {
			t.Errorf("Resolve(%s): the error quotes the stored definition: %v", name, err)
		}
	}
}
