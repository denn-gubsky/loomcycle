package config

import (
	"strings"
	"testing"
)

// memory.fulltext.config is spliced into DDL by the postgres store, so the load
// refuses anything that is not a configuration name — at boot, with the key.
func TestFullTextConfig_LoadRefusesANonName(t *testing.T) {
	for _, tc := range []struct {
		value, wantErr string
	}{
		{"russian", ""},
		{"public.ukrainian", ""},
		{"", ""},
		{"english'); DROP TABLE memory; --", "memory.fulltext.config"},
		{"two words", "memory.fulltext.config"},
	} {
		cfg, err := loadYAMLString(t, "memory:\n  fulltext:\n    config: \""+tc.value+"\"\n    unaccent: true\n")
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%q: unexpected error %v", tc.value, err)
				continue
			}
			if cfg.Memory.FullText.Config != tc.value || !cfg.Memory.FullText.Unaccent {
				t.Errorf("%q: block not decoded: %+v", tc.value, cfg.Memory.FullText)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: want error containing %q, got %v", tc.value, tc.wantErr, err)
		}
	}
}
