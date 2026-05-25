package anthropic_oauth_dev

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTokenStore_RoundTrip pins the basic Save→Load cycle including
// the chmod 0600 enforcement that's the v0.11.9 security boundary.
func TestTokenStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(filepath.Join(dir, "anthropic-oauth.json"))
	tok := NewToken("sk-ant-oat-x", "sk-ant-ort-y", "user:inference", 3600)
	if err := store.Save(tok); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Permissions on the on-disk file MUST be 0600. Anything wider is
	// a security regression — the operator's local file system is the
	// boundary for token confidentiality.
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.AccessToken != tok.AccessToken {
		t.Errorf("AccessToken roundtrip: %q vs %q", loaded.AccessToken, tok.AccessToken)
	}
	if loaded.RefreshToken != tok.RefreshToken {
		t.Errorf("RefreshToken roundtrip lost")
	}
	if !loaded.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("ExpiresAt roundtrip: %v vs %v", loaded.ExpiresAt, tok.ExpiresAt)
	}
}

// TestTokenStore_Atomicity verifies no half-written file is left when
// Save is interrupted — important because the refresh loop could race
// a concurrent CLI logout.
func TestTokenStore_NoTempfileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(filepath.Join(dir, "anthropic-oauth.json"))
	tok := NewToken("a", "b", "scope", 60)
	if err := store.Save(tok); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "anthropic-oauth.json" {
			t.Errorf("unexpected file in dir: %s (tempfile leaked)", e.Name())
		}
	}
}

// TestTokenStore_Delete_Idempotent: logout-after-logout must not error.
func TestTokenStore_Delete_Idempotent(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(filepath.Join(dir, "x.json"))
	if err := store.Delete(); err != nil {
		t.Errorf("Delete on missing file should be a no-op, got: %v", err)
	}
	_ = store.Save(NewToken("a", "b", "s", 60))
	if err := store.Delete(); err != nil {
		t.Errorf("Delete on existing file: %v", err)
	}
	if err := store.Delete(); err != nil {
		t.Errorf("Delete after Delete: %v", err)
	}
}

// TestTokenStore_LoadMissing returns fs.ErrNotExist so callers can
// distinguish "not logged in" from "corrupt store."
func TestTokenStore_LoadMissing(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(filepath.Join(dir, "absent.json"))
	_, err := store.Load()
	if err == nil {
		t.Fatal("expected ErrNotExist")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load missing file should return ErrNotExist, got %v", err)
	}
}

// TestTokenStore_VerifyPermissions_FlagsWidenedMode warns operators
// whose token file leaked permissions.
func TestTokenStore_VerifyPermissions_FlagsWidenedMode(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — chmod behaviour differs")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "leaky.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := NewTokenStore(path)
	if err := store.VerifyPermissions(); err == nil {
		t.Error("VerifyPermissions should reject 0644 file")
	}
}

// TestToken_NeedsRefresh: 4-minute-until-expiry → NeedsRefresh; 6-min
// → does not. Pins the 5-minute slack the background refresher uses.
func TestToken_NeedsRefresh(t *testing.T) {
	now := time.Now()
	tt := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"4min", now.Add(4 * time.Minute), true},
		{"6min", now.Add(6 * time.Minute), false},
		{"past", now.Add(-1 * time.Minute), true},
	}
	for _, c := range tt {
		t.Run(c.name, func(t *testing.T) {
			tok := Token{ExpiresAt: c.expiresAt}
			if got := tok.NeedsRefresh(); got != c.want {
				t.Errorf("NeedsRefresh = %v, want %v", got, c.want)
			}
		})
	}
}
