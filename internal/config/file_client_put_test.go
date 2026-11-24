package config

import (
	"strings"
	"testing"
)

// LOOMCYCLE_FILE_CLIENT_PUT_MAX_BYTES caps one file a run's owner puts into
// its inbox (RFC HE F1): 20 MiB by default, and never above the per-file cap
// LOOMCYCLE_FILE_MAX_BYTES, since a person's upload is a kept file like any
// other.
func TestFileClientPutMaxBytes_DefaultsAndClampsToFileMax(t *testing.T) {
	for _, tc := range []struct {
		name, put, fileMax string
		want               int64
	}{
		{"unset", "", "", 20 << 20},
		{"lowered", "1048576", "", 1 << 20},
		{"raised under the file cap", "33554432", "", 32 << 20},
		{"above the file cap", "104857600", "", 64 << 20},
		{"file cap lowered under the default", "", "10485760", 10 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_FILE_CLIENT_PUT_MAX_BYTES", tc.put)
			t.Setenv("LOOMCYCLE_FILE_MAX_BYTES", tc.fileMax)
			cfg, err := loadYAMLString(t, "providers: {}\n")
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Env.FileClientPutMaxBytes; got != tc.want {
				t.Errorf("FileClientPutMaxBytes = %d, want %d", got, tc.want)
			}
		})
	}
}

// A value that does not parse is refused rather than read as the default: a
// cap meant to be lowered must not keep more than the operator allowed.
func TestFileClientPutMaxBytes_BadValueRefusesTheBoot(t *testing.T) {
	t.Setenv("LOOMCYCLE_FILE_CLIENT_PUT_MAX_BYTES", "20MiB")
	_, err := loadYAMLString(t, "providers: {}\n")
	if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_FILE_CLIENT_PUT_MAX_BYTES") {
		t.Fatalf("err = %v, want it to name LOOMCYCLE_FILE_CLIENT_PUT_MAX_BYTES", err)
	}
}
