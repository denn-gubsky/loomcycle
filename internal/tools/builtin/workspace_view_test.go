package builtin

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func viewPNG(t *testing.T, w, h int) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// view returns a picture of the run's own workspace to the model as an
// image, with a short text naming it (RFC GW §5.8).
func TestWorkspaceView_ReturnsThePictureAsAnImage(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	pic := viewPNG(t, 20, 10)
	f.wsPut(t, "w", "charts/sales.png", pic)
	res := f.wsCall(t, f.ctx, `{"op":"view","name":"w","path":"charts/sales.png"}`)
	if res.IsError || len(res.Images) != 1 {
		t.Fatalf("view: %+v", res)
	}
	if !bytes.Equal(res.Images[0].Data, []byte(pic)) {
		t.Error("the picture's bytes changed on the way")
	}
	if !strings.Contains(res.Text, "charts/sales.png") {
		t.Errorf("text %q does not name the file", res.Text)
	}
}

// view is a read: the read grant is what it needs, and without it the call
// is refused before the workspace is reached.
func TestWorkspaceView_NeedsTheReadGrant(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	f.wsPut(t, "w", "a.png", viewPNG(t, 4, 4))
	noRead := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) {
		p.Workspace = tools.WorkspacePolicyValue{Write: true, Exec: true, Open: true}
	})
	if c := refusalClass(f.wsCall(t, noRead, `{"op":"view","name":"w","path":"a.png"}`)); c != "not_granted" {
		t.Errorf("view without read = %q, want not_granted", c)
	}
}

// A workspace shared with the run is viewed through the share, read access
// being enough; with no share, the name does not exist for the run.
func TestWorkspaceView_SharedNameObeysTheShare(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	f.wsPut(t, "w", "a.png", viewPNG(t, 4, 4))
	if c := refusalClass(f.wsCall(t, f.peer, `{"op":"view","name":"w@a_alice","path":"a.png"}`)); c != "not_found" {
		t.Errorf("view with no share = %q, want not_found", c)
	}
	f.ws.mustOK(t, f.ctx, `{"op":"share","name":"w","with":"a_peer","access":"read"}`)
	if res := f.wsCall(t, f.peer, `{"op":"view","name":"w@a_alice","path":"a.png"}`); res.IsError || len(res.Images) != 1 {
		t.Errorf("view through a read share: %+v", res)
	}
}

// A file larger than a picture may be is refused without its bytes being
// held; a missing file is the worker's own not-found.
func TestWorkspaceView_RefusesAnOversizeOrMissingFile(t *testing.T) {
	f := newShareFixture(t, tools.WorkspacePolicyValue{Read: true})
	f.wsPut(t, "w", "big.png", strings.Repeat("x", workspaceViewMaxBytes+1))
	if res := f.wsCall(t, f.ctx, `{"op":"view","name":"w","path":"big.png"}`); !res.IsError || len(res.Images) != 0 || !strings.Contains(res.Text, "too_large") {
		t.Errorf("view of an oversize file: %+v", res)
	}
	if res := f.wsCall(t, f.ctx, `{"op":"view","name":"w","path":"none.png"}`); !res.IsError || len(res.Images) != 0 {
		t.Errorf("view of a missing file: %+v", res)
	}
}
