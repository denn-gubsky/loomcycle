package config

import (
	"runtime"
	"strings"
	"testing"
)

// LOOMCYCLE_EXTRACT_CONCURRENCY bounds concurrent file extractions: unset,
// zero or negative takes GOMAXPROCS, a positive value is kept, and a value
// that does not parse refuses the load rather than falling back to a bound the
// operator did not choose.
func TestLoad_ExtractConcurrencyDefaultsToGOMAXPROCSAndRefusesGarbage(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	for _, c := range []struct {
		env  string
		want int
	}{{"", procs}, {"0", procs}, {"-3", procs}, {"5", 5}, {" 7 ", 7}} {
		t.Setenv("LOOMCYCLE_EXTRACT_CONCURRENCY", c.env)
		cfg, err := loadYAMLString(t, gitRemoteYAMLHead)
		if err != nil {
			t.Fatalf("%q: %v", c.env, err)
		}
		if cfg.Env.ExtractConcurrency != c.want {
			t.Errorf("LOOMCYCLE_EXTRACT_CONCURRENCY=%q: got %d, want %d", c.env, cfg.Env.ExtractConcurrency, c.want)
		}
	}
	for _, bad := range []string{"four", "2.5", "3x"} {
		t.Setenv("LOOMCYCLE_EXTRACT_CONCURRENCY", bad)
		if _, err := loadYAMLString(t, gitRemoteYAMLHead); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_EXTRACT_CONCURRENCY") {
			t.Errorf("LOOMCYCLE_EXTRACT_CONCURRENCY=%q: err = %v, want a load error naming the variable", bad, err)
		}
	}
}
