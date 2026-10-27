package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrate_LegacyTokenReadsFrozenAuthority is the sqlite twin of postgres
// 0116: a token row written before operator_token_defs.authority existed must
// read back frozen ("") after the upgrade, never inherit — an existing token
// keeps the ceiling it was minted with.
//
// Builds the pre-column table on disk (drops the column the fresh open
// added), writes a row the way a pre-upgrade deployment would have, then
// reopens so the ALTER runs against that row.
func TestMigrate_LegacyTokenReadsFrozenAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_token_authority.db")
	ctx := context.Background()
	{
		s, err := Open(path)
		if err != nil {
			t.Fatalf("initial open: %v", err)
		}
		for _, stmt := range []string{
			`ALTER TABLE operator_token_defs DROP COLUMN authority`,
			`INSERT INTO operator_token_defs (def_id, name, tenant_id, subject, token_hash, allowed_scopes, created_at)
			 VALUES ('def_old', 'u-alice', 'acme', 'alice', 'h-old', '["substrate:user"]', 1)`,
		} {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				_ = s.Close()
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (upgrade): %v", err)
	}
	defer s.Close()
	row, err := s.OperatorTokenDefGetByTokenHash(ctx, "h-old")
	if err != nil {
		t.Fatalf("lookup after upgrade: %v", err)
	}
	if row.Authority != "" {
		t.Errorf("pre-upgrade token authority = %q, want frozen (\"\")", row.Authority)
	}
}
