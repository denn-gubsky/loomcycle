package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// Every restore call site derives a restored dynamic volume under THIS host's
// dynamic root, runs the VolumeDef checks, and skips a name that is a static
// volume here — so a volume row planted with a host path of its choosing
// comes back pointing inside the target root, and nowhere else.
func TestRestore_EveryCallSiteDerivesVolumesUnderItsOwnRoot(t *testing.T) {
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	ctx := context.Background()
	plant := func(tenant, name, body string) {
		t.Helper()
		if _, err := src.VolumeDefCreate(ctx, store.VolumeDefRow{TenantID: tenant, Name: name, Definition: json.RawMessage(body)}); err != nil {
			t.Fatal(err)
		}
	}
	// Rows as a tampered source database could hold them: a path outside
	// any volume root, a static name, and a mode the tool refuses.
	plant("acme", "data", `{"path":"/etc","mode":"rw"}`)
	plant("acme", "repo", `{"path":"/srv/repo","mode":"rw"}`)
	plant("acme", "bad", `{"path":"/tmp/bad","mode":"rwx"}`)
	_, envelope, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, via := range []string{"connector", "http"} {
		t.Run(via, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			static, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := makeBaseConfig()
			cfg.Volumes = map[string]config.Volume{
				"pool": {Path: root, Mode: "rw", DynamicRoot: true},
				"repo": {Path: static, Mode: "ro"},
			}
			srv, _ := makeServer(t, completingProvider(), cfg)
			restored, warnings := restoreWarnings(t, srv, via, envelope)
			has := func(parts ...string) bool {
				for _, w := range warnings {
					all := true
					for _, p := range parts {
						all = all && strings.Contains(w, p)
					}
					if all {
						return true
					}
				}
				return false
			}
			if restored["volume_defs"] != 1 || restored["volume_dirs_created"] != 1 {
				t.Errorf("restored = %v; want only acme/data, with its directory created", restored)
			}
			want := filepath.Join(root, "acme", "data")
			spec, ok := lookup.VolumeDef(ctx, cfg, srv.store, "acme", "data")
			if !ok || spec.Path != want || spec.Mode != "rw" {
				t.Errorf("acme/data resolves (ok=%v) to %+v, want %s rw", ok, spec, want)
			}
			if info, err := os.Stat(want); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Errorf("no 0700 directory at %s: %v", want, err)
			}
			if spec, ok := lookup.VolumeDef(ctx, cfg, srv.store, "acme", "repo"); !ok || spec.Source != "static" {
				t.Errorf("repo resolves (ok=%v) to %+v, want the static volume", ok, spec)
			}
			if _, err := srv.store.VolumeDefGetByName(ctx, "acme", "repo"); err == nil {
				t.Error("a dynamic row was written under a static volume's name")
			}
			if _, err := srv.store.VolumeDefGetByName(ctx, "acme", "bad"); err == nil {
				t.Error("a volume with a refused mode was written")
			}
			if !has("volume_def acme/repo", "static volume") || !has("volume_def acme/bad", "invalid mode") {
				t.Errorf("missing skip warnings: %v", warnings)
			}
			for _, w := range warnings {
				if strings.Contains(w, root) || strings.Contains(w, "/etc") {
					t.Errorf("a warning names a host path: %s", w)
				}
			}
		})
	}
}
