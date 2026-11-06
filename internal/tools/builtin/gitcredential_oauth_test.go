package builtin

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A git network op's credential is "$cred:<name>" or "$ghapp:<name>". An
// $oauth: reference is refused before any lookup: an OAuth access token is
// for the server it was granted for, not for a git remote.
func TestGitCredential_OAuthRefIsRefused(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t"})
	for _, resolve := range []GitCredentialFunc{
		GitCredentialResolver(engine, nil, nil),
		GitPushCredentialResolver(engine, nil, nil),
	} {
		_, _, err := resolve(ctx, "$oauth:docs", "https://git.example.com/acme/app")
		if !errors.Is(err, ErrGitCredentialUnresolvable) {
			t.Errorf("err = %v, want ErrGitCredentialUnresolvable", err)
		}
	}
}
