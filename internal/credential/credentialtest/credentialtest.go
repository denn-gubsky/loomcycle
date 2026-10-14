// Package credentialtest puts a stored credential into states the credential
// engine refuses to create, for tests outside internal/credential that
// exercise how a consumer treats them.
package credentialtest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Expire sets the expires_at of the stored credential id to at — usually a
// moment already past, which PutInline refuses. The sealed value is left as
// it is: expires_at is not part of what it is sealed with, so the row still
// opens, and only the expiry rule refuses it.
func Expire(t testing.TB, st store.Store, id credential.Identity, at time.Time) {
	t.Helper()
	ctx := context.Background()
	row, err := st.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		t.Fatalf("credentialtest.Expire: read %q: %v", id.Name, err)
	}
	row.ExpiresAt = &at
	if _, err := st.CredentialDefPut(ctx, row); err != nil {
		t.Fatalf("credentialtest.Expire: write %q: %v", id.Name, err)
	}
}
