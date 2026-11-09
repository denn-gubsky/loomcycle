package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/redact"
)

// TestLoadOTELHeadersFile_MasksTheValuesInLogs: collector auth read from the
// headers file is in no environment the log redactor was built from; once
// loaded, a log line that echoes it is masked, and the values come back for
// the server's at-rest redactor.
func TestLoadOTELHeadersFile_MasksTheValuesInLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers")
	if err := os.WriteFile(path, []byte("authorization=Bearer%20file-secret-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := redact.New(nil, false)
	cfg := lcotel.Config{Enabled: true, HeadersFile: path}
	values, err := loadOTELHeadersFile(&cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Headers["authorization"] != "Bearer file-secret-42" || len(values) != 1 {
		t.Fatalf("headers = %v, values = %v", cfg.Headers, values)
	}
	if got := r.String("dial failed with Bearer file-secret-42"); strings.Contains(got, "file-secret-42") {
		t.Errorf("log redactor left the file's value in: %q", got)
	}

	// Tracing off: the file is not read at all.
	off := lcotel.Config{HeadersFile: filepath.Join(t.TempDir(), "missing")}
	if v, err := loadOTELHeadersFile(&off, r); err != nil || v != nil {
		t.Errorf("tracing off: values=%v err=%v, want nothing read", v, err)
	}
}
