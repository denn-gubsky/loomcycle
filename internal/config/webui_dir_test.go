package config

import (
	"strings"
	"testing"
)

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

// The bundle URL and its digest are set together or not at all, the URL is
// https with a host and no user info, the digest is 64 lowercase hex, and a
// URL with LOOMCYCLE_WEBUI_DIR is ambiguous. Each is refused at load, before
// anything is fetched, and the error never quotes the URL: its query string
// may carry a token.
func TestWebUIBundleURL_EnvValidation(t *testing.T) {
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const secret = "sig=s3cr3t"
	for _, c := range []struct {
		name, url, sha, dir string
		wantErr             string // "" = accepted
	}{
		{"neither", "", "", "", ""},
		{"both", "https://example.com/ui.tar.gz?" + secret, sum, "", ""},
		{"url only", "https://example.com/ui.tar.gz?" + secret, "", "", "must be set together"},
		{"digest only", "", sum, "", "must be set together"},
		{"with dir", "https://example.com/ui.tar.gz", sum, "/srv/webui", "LOOMCYCLE_WEBUI_DIR"},
		{"http", "http://example.com/ui.tar.gz?" + secret, sum, "", "https"},
		{"ftp", "ftp://example.com/ui.tar.gz", sum, "", "https"},
		{"no host", "https:///ui.tar.gz", sum, "", "host"},
		{"user info", "https://user:" + secret + "@example.com/ui.tar.gz", sum, "", "user info"},
		{"unparseable", "https://exa mple.com/%zz?" + secret, sum, "", "LOOMCYCLE_WEBUI_BUNDLE_URL"},
		{"upper-case digest", "https://example.com/ui.tar.gz", strings.ToUpper(sum), "", "64 lowercase hex"},
		{"short digest", "https://example.com/ui.tar.gz", sum[:63], "", "64 lowercase hex"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LOOMCYCLE_WEBUI_BUNDLE_URL", c.url)
			t.Setenv("LOOMCYCLE_WEBUI_BUNDLE_SHA256", c.sha)
			t.Setenv("LOOMCYCLE_WEBUI_DIR", c.dir)
			cfg, err := loadYAMLString(t, "channels: {}\n")
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if cfg.Env.WebUIBundleURL != c.url || cfg.Env.WebUIBundleSHA256 != c.sha {
					t.Fatalf("read (%q, %q), want (%q, %q)",
						cfg.Env.WebUIBundleURL, cfg.Env.WebUIBundleSHA256, c.url, c.sha)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted, want an error mentioning %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not mention %q", err, c.wantErr)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "example.com") {
				t.Errorf("error quotes the URL: %q", err)
			}
		})
	}
}
