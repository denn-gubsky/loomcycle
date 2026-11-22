package loop

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// visionGate is whether the run's current model can see the pictures a tool
// returns (RFC GW §5.8), taken when the turn's tools are dispatched: the
// provider and model only change at a model call (a fallback), never while
// tools run.
type visionGate struct {
	sees            bool
	provider, model string
}

func visionGateFor(opts *RunOptions) visionGate {
	g := visionGate{model: opts.Model}
	if opts.Provider != nil {
		g.provider = opts.Provider.ID()
		g.sees = providers.SeesImages(opts.Provider, opts.Model)
	}
	return g
}

// admit refuses a result's pictures to a model that cannot see them, before
// the call they would ride in: the call fails saying so, and the run goes on.
// The same gate as a user's image (RFC AT), but here the model asked for the
// picture, so the model is told rather than the run ended.
func (g visionGate) admit(res tools.Result) tools.Result {
	if len(res.Images) == 0 || g.sees {
		return res
	}
	text := fmt.Sprintf("the tool returned %d picture(s), but model %q on provider %q cannot see images, so they were not sent", len(res.Images), g.model, g.provider)
	if res.Text != "" {
		text += "\n\n" + res.Text
	}
	return tools.Result{Text: text, IsError: true, Error: res.Error, Count: res.Count}
}

// pictureNotes is one line per picture, added to the result's text. A reader
// of the tool_result event on any transport sees what the model was shown
// without the bytes, and a model whose provider carries the picture in a
// separate message can tell which call it answers.
func pictureNotes(images []tools.Image) string {
	if len(images) == 0 {
		return ""
	}
	var b strings.Builder
	for i, im := range images {
		fmt.Fprintf(&b, "\n[picture %d of %d: %s, %d×%d px, %s]", i+1, len(images), im.MediaType, im.Width, im.Height, sizeLabel(len(im.Data)))
	}
	return b.String()
}

func sizeLabel(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+512)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// imageParts is the pictures as the provider-facing blocks carry them.
func imageParts(images []tools.Image) []providers.ImagePart {
	if len(images) == 0 {
		return nil
	}
	out := make([]providers.ImagePart, len(images))
	for i, im := range images {
		out[i] = providers.ImagePart{MediaType: im.MediaType, Data: base64.StdEncoding.EncodeToString(im.Data)}
	}
	return out
}
