package stdio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSpawn_ChildDoesNotInheritLoomcyclesOTELSettings: a stdio server gets
// none of loomcycle's own OTEL_* variables (an instrumented server would
// export to loomcycle's collector with its credentials), keeps the rest of the
// environment, and keeps an OTEL_* entry its definition sets itself.
func TestSpawn_ChildDoesNotInheritLoomcyclesOTELSettings(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=collector-secret")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("STDIO_ENV_PROBE_KEPT", "kept")
	out := filepath.Join(t.TempDir(), "env")
	c, err := Spawn(Config{
		Command: "/bin/sh",
		Args:    []string{"-c", `env > "$ENV_OUT"; cat >/dev/null`},
		Env:     []string{"ENV_OUT=" + out, "OTEL_SERVICE_NAME=my-server"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var env string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, _ := os.ReadFile(out); strings.Contains(string(b), "ENV_OUT=") {
			env = string(b)
			break
		}
	}
	if env == "" {
		t.Fatal("the child never wrote its environment")
	}
	for _, leaked := range []string{"collector-secret", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		if strings.Contains(env, leaked) {
			t.Errorf("child environment carries %q", leaked)
		}
	}
	for _, want := range []string{"STDIO_ENV_PROBE_KEPT=kept", "OTEL_SERVICE_NAME=my-server"} {
		if !strings.Contains(env, want) {
			t.Errorf("child environment lacks %q", want)
		}
	}
}
