package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDynamicRootWritable_LogsRemedyOnlyWhenRootRefusesWrites(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so a read-only root cannot be made")
	}
	logged := func(root string) string {
		var buf bytes.Buffer
		checkDynamicRootWritable(slog.New(slog.NewTextHandler(&buf, nil)), root)
		return buf.String()
	}

	writable := t.TempDir()
	if out := logged(writable); out != "" {
		t.Errorf("writable root logged %q; want nothing", out)
	}
	if left, _ := os.ReadDir(writable); len(left) != 0 {
		t.Errorf("the probe left %d entries in the root", len(left))
	}

	readOnly := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	out := logged(readOnly)
	for _, want := range []string{
		"level=ERROR",
		readOnly,
		fmt.Sprintf("chown -R %d:%d", os.Geteuid(), os.Getegid()),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("read-only root logged %q; want it to contain %q", out, want)
		}
	}
}
