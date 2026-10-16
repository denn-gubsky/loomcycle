package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A static volume whose root is the dynamic root, or above it, does not reach
// the dynamic and ephemeral volumes inside it. Read-only was enforced by the
// volume's NAME only, so a Write through the wider static binding landed in a
// read-only ephemeral volume, and Read/Glob/Grep through it reached other
// runs' and tenants' volumes. The same static binding still serves its own
// files, so a config that nests the dynamic root keeps working. Every path
// that builds a binding is checked: the agent's own policy (named and
// implicit default), spawn narrowing, and a resumed sub-run's parent.
func TestStaticVolume_DoesNotReachTheDynamicRoot(t *testing.T) {
	ctx := context.Background()
	named := func(srv *Server) tools.VolumePolicyValue {
		return srv.volumePolicyForAgent(ctx, config.AgentDef{Volumes: []string{"default", "pool"}})
	}
	policies := map[string]func(*Server) tools.VolumePolicyValue{
		"named": named,
		"implicit default": func(srv *Server) tools.VolumePolicyValue {
			return srv.volumePolicyForAgent(ctx, config.AgentDef{})
		},
		"spawn narrowing": func(srv *Server) tools.VolumePolicyValue {
			return srv.childVolumePolicy(ctx, named(srv), config.AgentDef{Volumes: []string{"default", "pool"}})
		},
		"resumed parent": func(srv *Server) tools.VolumePolicyValue {
			return srv.recordedParentVolumes(ctx, volumeCeilingRecord{Active: true, Bindings: []volumeCeilingBinding{
				{Name: "default", Default: true}, {Name: "pool"},
			}})
		},
	}
	call := func(t tools.Tool, c context.Context, args map[string]any) tools.Result {
		in, _ := json.Marshal(args)
		res, err := t.Execute(c, in)
		if err != nil {
			return tools.Result{Text: err.Error(), IsError: true}
		}
		return res
	}
	for label, policyOf := range policies {
		// A fresh tree per label: one label's write must not hide another's.
		top, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dyn := filepath.Join(top, "dynamic")
		eph := filepath.Join(dyn, "_ephemeral", "run-1", "work")
		if err := os.MkdirAll(eph, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(eph, "secret.txt"), []byte("needle in another run\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(top, "notes.txt"), []byte("needle of my own\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		srv := &Server{cfgHolder: config.NewHolder(&config.Config{Volumes: map[string]config.Volume{
			"default": {Path: top, Mode: "rw", Default: true},
			"pool":    {Path: dyn, Mode: "rw", DynamicRoot: true},
		}})}
		pol := policyOf(srv)
		c := tools.WithVolumePolicy(ctx, pol)
		reach := []struct {
			tool tools.Tool
			args map[string]any
		}{
			{&builtin.Write{}, map[string]any{"volume": "default", "path": "dynamic/_ephemeral/run-1/work/x.txt", "content": "x"}},
			{&builtin.Read{}, map[string]any{"volume": "default", "path": "dynamic/_ephemeral/run-1/work/secret.txt"}},
			{&builtin.Read{}, map[string]any{"volume": "default", "path": "dynamic/_ephemeral/run-1/work/secret.txt", "offset": 1}},
			{&builtin.Edit{}, map[string]any{"volume": "default", "path": "dynamic/_ephemeral/run-1/work/secret.txt", "old_string": "run", "new_string": "walk"}},
		}
		if label != "implicit default" {
			reach = append(reach,
				struct {
					tool tools.Tool
					args map[string]any
				}{&builtin.Write{}, map[string]any{"volume": "pool", "path": "_ephemeral/run-1/work/x.txt", "content": "x"}},
				struct {
					tool tools.Tool
					args map[string]any
				}{&builtin.Read{}, map[string]any{"volume": "pool", "path": "_ephemeral/run-1/work/secret.txt"}},
			)
		}
		for _, r := range reach {
			if res := call(r.tool, c, r.args); !res.IsError || strings.Contains(res.Text, "needle") {
				t.Errorf("%s: %s %v reached the dynamic root: %q", label, r.tool.Name(), r.args, res.Text)
			}
		}
		if _, err := os.Stat(filepath.Join(eph, "x.txt")); err == nil {
			t.Errorf("%s: a write landed in the ephemeral volume", label)
		}
		if b, _ := os.ReadFile(filepath.Join(eph, "secret.txt")); string(b) != "needle in another run\n" {
			t.Errorf("%s: the ephemeral volume's file was changed: %q", label, b)
		}

		// Listing and searching the static volume skip the dynamic root but
		// still find the volume's own file, so the check is not vacuous.
		g := call(&builtin.Glob{}, c, map[string]any{"volume": "default", "pattern": "**/*.txt"})
		if g.IsError || strings.Contains(g.Text, "secret") || !strings.Contains(g.Text, "notes.txt") {
			t.Errorf("%s: Glob = %q (error=%v), want notes.txt and not the dynamic root", label, g.Text, g.IsError)
		}
		gr := call(&builtin.Grep{}, c, map[string]any{"volume": "default", "pattern": "needle", "output_mode": "content"})
		if gr.IsError || strings.Contains(gr.Text, "another") || !strings.Contains(gr.Text, "of my own") {
			t.Errorf("%s: Grep = %q (error=%v), want notes.txt and not the dynamic root", label, gr.Text, gr.IsError)
		}
		if res := call(&builtin.Grep{}, c, map[string]any{"volume": "default", "pattern": "needle", "path": "dynamic"}); !res.IsError {
			t.Errorf("%s: Grep with path under the dynamic root = %q, want a refusal", label, res.Text)
		}

		// Bashbox sees an empty directory there: nothing read, nothing kept.
		// Its own files are still read and written.
		box := &builtin.Bashbox{Enabled: true}
		res := call(box, c, map[string]any{"volume": "default", "command": "cat notes.txt; cat dynamic/_ephemeral/run-1/work/secret.txt; echo x > dynamic/_ephemeral/run-1/work/x.txt; echo ok > mine2.txt"})
		if strings.Contains(res.Text, "another") || !strings.Contains(res.Text, "of my own") {
			t.Errorf("%s: Bashbox = %q, want notes.txt and not the dynamic root", label, res.Text)
		}
		if _, err := os.Stat(filepath.Join(eph, "x.txt")); err == nil {
			t.Errorf("%s: a Bashbox write landed in the ephemeral volume", label)
		}
		if _, err := os.Stat(filepath.Join(top, "mine2.txt")); err != nil {
			t.Errorf("%s: a Bashbox write to the static volume's own files was not kept: %v", label, err)
		}
		if label != "implicit default" {
			if res := call(box, c, map[string]any{"volume": "pool", "command": "cat _ephemeral/run-1/work/secret.txt"}); !res.IsError || strings.Contains(res.Text, "needle") {
				t.Errorf("%s: Bashbox in the dynamic root itself = %q, want a refusal", label, res.Text)
			}
		}

		// The static volume still serves its own files.
		if res := call(&builtin.Write{}, c, map[string]any{"volume": "default", "path": "mine.txt", "content": "ok"}); res.IsError {
			t.Errorf("%s: a write to the static volume's own files was refused: %s", label, res.Text)
		}
	}
}
