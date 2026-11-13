package config

import (
	"os"
	"strings"
	"testing"
)

// One variable says whether the dead-link collection runs and whether it
// deletes. Unset it is on, which is what the two variables it replaced gave
// when neither was set; a value outside the three fails the load by name.
func TestDeadLinkGC_ModeIsOffDryRunOrOnAndDefaultsToOn(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		unset       bool
		want        string
		wantErr     bool
	}{
		{name: "unset", unset: true, want: DeadLinkGCOn},
		{name: "empty", value: "", want: DeadLinkGCOn},
		{name: "on", value: "on", want: DeadLinkGCOn},
		{name: "dry-run", value: "dry-run", want: DeadLinkGCDryRun},
		{name: "off", value: "off", want: DeadLinkGCOff},
		{name: "padded", value: " off ", want: DeadLinkGCOff},
		{name: "another case", value: "OFF", wantErr: true},
		{name: "a boolean", value: "1", wantErr: true},
		{name: "a zero", value: "0", wantErr: true},
		{name: "a near miss", value: "dryrun", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_DEADLINK_GC", tc.value)
			if tc.unset {
				if err := os.Unsetenv("LOOMCYCLE_DEADLINK_GC"); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := loadYAMLString(t, "defaults: {provider: mock}\n")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_DEADLINK_GC") {
					t.Fatalf("err = %v, want one naming LOOMCYCLE_DEADLINK_GC", err)
				}
				for _, mode := range []string{"off", "dry-run", "on"} {
					if !strings.Contains(err.Error(), mode) {
						t.Errorf("error does not list %q: %v", mode, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Env.DeadLinkGC != tc.want {
				t.Errorf("DeadLinkGC = %q, want %q", cfg.Env.DeadLinkGC, tc.want)
			}
		})
	}
}
