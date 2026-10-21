package config

import "testing"

// LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES tunes the per-call cap on Bashbox's
// in-memory writes. Unset, zero, negative or malformed keeps the 64 MiB
// default: the cap is a memory floor, so a typo must not lift it. A value
// too small for gbash's own setup writes is raised to 1 MiB.
func TestBashboxOverlayMaxBytes_DefaultsAndOverride(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", 64 << 20},
		{"0", 64 << 20},
		{"-5", 64 << 20},
		{"64MiB", 64 << 20},
		{"8388608", 8 << 20},
		{"1000", 1 << 20},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_BASHBOX_OVERLAY_MAX_BYTES", tc.value)
			cfg, err := loadYAMLString(t, "providers: {}\n")
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Env.BashboxOverlayMaxBytes; got != tc.want {
				t.Errorf("BashboxOverlayMaxBytes = %d, want %d", got, tc.want)
			}
		})
	}
}
