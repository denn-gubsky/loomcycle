//go:build windows

package anthropic_oauth_dev

// acquireFileLock is a no-op on Windows. Cross-process advisory file locking is
// not implemented here: the anthropic-oauth-dev provider is a dev-only
// convenience whose primary platforms are Linux/macOS, and reload-before-refresh
// (in refreshLocked) still mitigates the rotation race. Returns a no-op release
// so the caller path is identical across platforms.
func acquireFileLock(_ string) (func(), error) {
	return func() {}, nil
}
