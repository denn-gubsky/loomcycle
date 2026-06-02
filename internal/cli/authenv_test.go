package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeConfigPath returns a loomcycle.yaml path inside dir (LoadAuthEnv reads
// auth.env from the config file's directory).
func fakeConfigPath(dir string) string { return filepath.Join(dir, "loomcycle.yaml") }

func TestLoadAuthEnv_AbsentIsNotAnError(t *testing.T) {
	_, n, err := LoadAuthEnv(fakeConfigPath(t.TempDir()))
	if err != nil {
		t.Fatalf("absent auth.env should not error: %v", err)
	}
	if n != 0 {
		t.Errorf("absent auth.env set %d vars, want 0", n)
	}
}

func TestLoadAuthEnv_SetsUnsetVarsAndParsesShape(t *testing.T) {
	dir := t.TempDir()
	// export prefix, quotes, a comment, and a blank line must all parse.
	body := "# comment\n\nexport LOOMCYCLE_AUTH_TOKEN=abc123\nLOOMCYCLE_FOO=\"quoted val\"\n"
	if err := os.WriteFile(filepath.Join(dir, authEnvFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("LOOMCYCLE_AUTH_TOKEN")
	os.Unsetenv("LOOMCYCLE_FOO")
	t.Cleanup(func() { os.Unsetenv("LOOMCYCLE_AUTH_TOKEN"); os.Unsetenv("LOOMCYCLE_FOO") })

	_, n, err := LoadAuthEnv(fakeConfigPath(dir))
	if err != nil {
		t.Fatalf("LoadAuthEnv: %v", err)
	}
	if n != 2 {
		t.Errorf("set %d vars, want 2", n)
	}
	if got := os.Getenv("LOOMCYCLE_AUTH_TOKEN"); got != "abc123" {
		t.Errorf("LOOMCYCLE_AUTH_TOKEN = %q, want abc123", got)
	}
	if got := os.Getenv("LOOMCYCLE_FOO"); got != "quoted val" {
		t.Errorf("LOOMCYCLE_FOO = %q, want 'quoted val' (quotes stripped)", got)
	}
}

// The security-load-bearing guarantee: a real shell export always wins over
// the file, so auth.env can never silently shadow an explicit token.
func TestLoadAuthEnv_RealEnvWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, authEnvFileName),
		[]byte("LOOMCYCLE_AUTH_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMCYCLE_AUTH_TOKEN", "from-shell")

	_, n, err := LoadAuthEnv(fakeConfigPath(dir))
	if err != nil {
		t.Fatalf("LoadAuthEnv: %v", err)
	}
	if n != 0 {
		t.Errorf("set %d vars, want 0 (the var was already set)", n)
	}
	if got := os.Getenv("LOOMCYCLE_AUTH_TOKEN"); got != "from-shell" {
		t.Errorf("LOOMCYCLE_AUTH_TOKEN = %q, want from-shell (shell export must win)", got)
	}
}
