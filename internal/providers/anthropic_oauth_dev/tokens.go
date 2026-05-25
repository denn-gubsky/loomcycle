package anthropic_oauth_dev

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Token is one OAuth token-set persisted to disk. The shape mirrors
// Pi's structure so a future migration from Pi's token store is a
// rename, not a re-marshal.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	ObtainedAt   time.Time `json:"obtained_at"`
	Scope        string    `json:"scope"`
	ClientID     string    `json:"client_id"`
}

// IsExpired reports whether the access token has passed its expiry. The
// expiry already carries a 5-minute slack from issuance (set in
// NewToken below), so callers don't need to add their own buffer.
func (t Token) IsExpired() bool {
	return time.Now().After(t.ExpiresAt)
}

// NeedsRefresh reports whether the access token is within the 5-min
// slack window before expiry. The background refresh goroutine uses
// this to decide when to rotate proactively.
func (t Token) NeedsRefresh() bool {
	return time.Until(t.ExpiresAt) < 5*time.Minute
}

// NewToken builds a Token from a fresh OAuth response, computing
// ExpiresAt with the 5-min slack already applied. expiresIn is the
// `expires_in` field from the OAuth response (in seconds).
func NewToken(accessToken, refreshToken, scope string, expiresIn int) Token {
	now := time.Now().UTC()
	return Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		Scope:        scope,
		ClientID:     ClaudeCodeClientID,
		ObtainedAt:   now,
		// 5-minute slack: treat the token as "expired" 5 minutes before
		// Anthropic actually expires it. The background refresh runs
		// before this point, so in-flight requests never use a token
		// that's about to expire.
		ExpiresAt: now.Add(time.Duration(expiresIn) * time.Second).Add(-5 * time.Minute),
	}
}

// TokenStore persists tokens to a file path with chmod 0600 enforcement.
// Safe for concurrent use — a sync.RWMutex serialises read/write.
//
// The file lives at TokenStorePath() (~/.config/loomcycle/anthropic-oauth.json
// on Linux/macOS, %AppData%\loomcycle\anthropic-oauth.json on Windows).
// Filesystem permissions are the security boundary; operators with
// stronger requirements use API-key Anthropic instead per RFC §out of
// scope.
type TokenStore struct {
	path string
	mu   sync.RWMutex
}

// NewTokenStore constructs a TokenStore at the given path. Use
// DefaultTokenStorePath() for the canonical operator location.
func NewTokenStore(path string) *TokenStore {
	return &TokenStore{path: path}
}

// DefaultTokenStorePath returns the canonical operator path:
// $XDG_CONFIG_HOME/loomcycle/anthropic-oauth.json (Linux),
// ~/Library/Application Support/loomcycle/anthropic-oauth.json (macOS),
// %AppData%\loomcycle\anthropic-oauth.json (Windows).
//
// Returns the empty string + an error when os.UserConfigDir() fails —
// the caller decides how to surface it (CLI subcommands print a clear
// error pointing at the env-var override).
func DefaultTokenStorePath() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("os.UserConfigDir: %w", err)
	}
	return filepath.Join(cfgDir, "loomcycle", "anthropic-oauth.json"), nil
}

// Path returns the file path the store was constructed with. Exposed
// for the `status` CLI subcommand which surfaces the location to the
// operator.
func (s *TokenStore) Path() string { return s.path }

// Load reads and returns the token. Returns os.ErrNotExist when the
// file doesn't exist — callers branch on errors.Is(err, os.ErrNotExist)
// to distinguish "not logged in" from "corrupt token store."
func (s *TokenStore) Load() (Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return Token{}, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return t, nil
}

// Save writes the token atomically (write-to-tempfile + rename) and
// enforces chmod 0600 on the result. Atomic write means a crash mid-
// save leaves either the old token or no file at all — never a
// half-written one.
//
// Parent directories are created with chmod 0700 if missing (the
// loomcycle config dir might not exist on a fresh install).
func (s *TokenStore) Save(t Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(s.path), err)
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	// Tempfile + rename for atomicity. Same directory so the rename is
	// atomic on POSIX (cross-device-rename would copy + delete).
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".anthropic-oauth-*.tmp")
	if err != nil {
		return fmt.Errorf("create tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails after this point.
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("rename tempfile: %w", err)
	}
	// Defensive re-chmod on the final path — some filesystems carry
	// permissions across rename, others don't; explicit re-set is
	// belt-and-suspenders against silent permission drift.
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("chmod final: %w", err)
	}
	return nil
}

// Delete removes the token file. Returns nil when the file is already
// absent (idempotent — matches the `logout` CLI subcommand's "always
// safe to call" expectation).
func (s *TokenStore) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("delete %s: %w", s.path, err)
}

// VerifyPermissions reports a non-nil error when the on-disk file's
// permissions are wider than 0600. Used by the `status` CLI subcommand
// to warn operators whose token file leaked permissions (e.g. a
// chmod-755 misconfiguration of $HOME).
func (s *TokenStore) VerifyPermissions() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf("token file %s has permissions %o (want 0600); run `chmod 600 %s`", s.path, mode, s.path)
	}
	return nil
}
