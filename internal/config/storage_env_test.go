package config

import "testing"

// TestStorageEnv_PoolSizeOutOfInt32RangeIsIgnored: the pool sizes are int32.
// A value past that range is a typo, not a pool size; it is ignored like any
// other unparsable value, never wrapped into some other number (2^32+5 used
// to become a pool of 5).
func TestStorageEnv_PoolSizeOutOfInt32RangeIsIgnored(t *testing.T) {
	t.Setenv("LOOMCYCLE_PG_MAX_OPEN_CONNS", "4294967301") // 2^32 + 5
	t.Setenv("LOOMCYCLE_PG_MIN_IDLE_CONNS", "4294967298") // 2^32 + 2
	c, err := loadYAMLString(t, "storage:\n  pg_max_open_conns: 7\n  pg_min_idle_conns: 3\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Storage.PgMaxOpenConns != 7 {
		t.Errorf("PgMaxOpenConns = %d, want the yaml value 7", c.Storage.PgMaxOpenConns)
	}
	if c.Storage.PgMinIdleConns != 3 {
		t.Errorf("PgMinIdleConns = %d, want the yaml value 3", c.Storage.PgMinIdleConns)
	}
}

// TestStorageEnv_PoolSizeInRangeOverridesYAML keeps the ordinary override.
func TestStorageEnv_PoolSizeInRangeOverridesYAML(t *testing.T) {
	t.Setenv("LOOMCYCLE_PG_MAX_OPEN_CONNS", "64")
	t.Setenv("LOOMCYCLE_PG_MIN_IDLE_CONNS", "0")
	c, err := loadYAMLString(t, "storage:\n  pg_max_open_conns: 7\n  pg_min_idle_conns: 3\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Storage.PgMaxOpenConns != 64 || c.Storage.PgMinIdleConns != 0 {
		t.Errorf("pool = %d/%d, want 64/0 from the environment", c.Storage.PgMaxOpenConns, c.Storage.PgMinIdleConns)
	}
}
