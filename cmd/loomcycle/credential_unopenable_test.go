package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// sealedUnder stores names in tenant "t" sealed under key byte a, and
// returns a binding over the same store whose engine has key byte b (0 = no
// key configured at all) — a KEK rotated without _PREVIOUS, or removed.
func sealedUnder(t *testing.T, a, b byte, names ...string) credentialBinding {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	put := engineWithKey(t, st, a)
	for _, name := range names {
		if _, err := put.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: "tenant", Name: name}, name+"-value", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	return credentialBinding{engine: engineWithKey(t, st, b), register: func(string) { t.Error("an unopenable value was registered") }}
}

func engineWithKey(t *testing.T, st store.Store, k byte) *credential.Engine {
	t.Helper()
	kek := ""
	if k != 0 {
		kek = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{k}, 32))
	}
	sealer, err := credential.NewSealer(kek, "")
	if err != nil {
		t.Fatal(err)
	}
	return credential.NewEngine(st, sealer)
}

// A tenant key that exists but cannot be opened — wrong KEK, or no KEK at
// all — fails the provider call; the operator's key is never used in its
// place, whether or not the run may use operator keys. With no KEK and no
// stored row, the operator key is used exactly as before.
func TestCredentialBinding_AnUnopenableProviderKeyFailsInsteadOfTheOperatorKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  byte
	}{{"wrong KEK", 2}, {"no KEK", 0}} {
		b := sealedUnder(t, 1, tc.key, "ANTHROPIC_API_KEY")
		for _, allowed := range []bool{true, false} {
			ctx := providers.WithOperatorKeyAllowed(providers.WithCredentialResolver(runCtx(), b.providerKey), allowed)
			key, source, _, err := providers.ResolveKeyOrOperator(ctx, "ANTHROPIC_API_KEY", "sk-operator")
			if !errors.Is(err, providers.ErrCredentialUnusable) || !errors.Is(err, credential.ErrUnopenable) {
				t.Errorf("%s, operator key allowed=%v: err = %v, want ErrCredentialUnusable wrapping ErrUnopenable", tc.name, allowed, err)
			}
			if key != "" || source != "" {
				t.Errorf("%s, operator key allowed=%v: key=%q source=%q; want no key at all", tc.name, allowed, key, source)
			}
		}
		if b.keyable(context.Background(), "t", "", "u", "ANTHROPIC_API_KEY") {
			t.Errorf("%s: an unopenable key was reported keyable", tc.name)
		}
	}

	b := sealedUnder(t, 1, 0) // no KEK, nothing stored
	ctx := providers.WithCredentialResolver(runCtx(), b.providerKey)
	if key, source, _, err := providers.ResolveKeyOrOperator(ctx, "ANTHROPIC_API_KEY", "sk-operator"); err != nil || key != "sk-operator" || source != "operator" {
		t.Errorf("no KEK, no row: key=%q source=%q err=%v; want the operator key, unchanged", key, source, err)
	}
}

// Through the real Bash tool: a tenant GITHUB_TOKEN sealed under another key
// leaves the variable unset in the child — the operator's allowlisted host
// GITHUB_TOKEN is removed, not passed through — and the output says why.
func TestCredentialBinding_AnUnopenableEnvVarRemovesTheHostVar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	t.Setenv("GITHUB_TOKEN", "operator-host-token")
	b := sealedUnder(t, 1, 2, "GITHUB_TOKEN")
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bash := &builtin.Bash{
		Enabled:         true,
		AllowedExtraEnv: []string{"GITHUB_TOKEN"},
		AllowedCreds:    []string{"GITHUB_TOKEN"},
		CredResolve:     b.envVar("bash"),
	}
	ctx := tools.UpdateRunPolicy(runCtx(), func(rp *tools.RunPolicy) {
		rp.Volumes = tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "default", Root: cwd, Default: true}}}
	})
	body, _ := json.Marshal(map[string]string{"command": "env"})
	res, err := bash.Execute(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "GITHUB_TOKEN=") {
		t.Errorf("the child saw a GITHUB_TOKEN although the tenant's could not be opened: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[GITHUB_TOKEN is not set:") || !strings.Contains(res.Text, "cannot be opened") {
		t.Errorf("the output does not say why the variable is unset: %q", res.Text)
	}
}
