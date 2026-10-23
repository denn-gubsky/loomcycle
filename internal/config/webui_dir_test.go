package config

import "testing"

// LOOMCYCLE_WEBUI_DIR is read verbatim apart from surrounding space; unset
// leaves it empty, which serves the embedded Web UI.
func TestWebUIDir_Env(t *testing.T) {
	for _, c := range []struct{ env, want string }{
		{"", ""},
		{"/srv/loomcycle/webui", "/srv/loomcycle/webui"},
		{"  /srv/webui \n", "/srv/webui"},
	} {
		t.Setenv("LOOMCYCLE_WEBUI_DIR", c.env)
		cfg, err := loadYAMLString(t, "channels: {}\n")
		if err != nil {
			t.Fatalf("%q: %v", c.env, err)
		}
		if cfg.Env.WebUIDir != c.want {
			t.Errorf("%q: WebUIDir = %q, want %q", c.env, cfg.Env.WebUIDir, c.want)
		}
	}
}
