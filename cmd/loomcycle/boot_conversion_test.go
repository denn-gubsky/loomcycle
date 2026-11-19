package main

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/conversion/wire"
)

// A worker endpoint on the network refuses to start without a token that
// cannot be guessed; local workers alone get one made for this boot.
func TestConversionToken_ARemoteEndpointNeedsAStrongToken(t *testing.T) {
	remote := config.ConversionConfig{Mode: config.ConversionRemote, Listen: "0.0.0.0:7788"}
	for _, tok := range []string{"", "short-token"} {
		t.Setenv("LOOMCYCLE_WORKER_TOKEN", tok)
		if _, err := conversionToken(remote); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_WORKER_TOKEN") {
			t.Errorf("token %q: err = %v, want a refusal naming LOOMCYCLE_WORKER_TOKEN", tok, err)
		}
	}
	strong := strings.Repeat("ab", 20)
	t.Setenv("LOOMCYCLE_WORKER_TOKEN", strong)
	if got, err := conversionToken(remote); err != nil || got != strong {
		t.Errorf("a strong token: %q, %v", got, err)
	}
	// Local alone ignores the operator's token: its children are told a
	// fresh one.
	got, err := conversionToken(config.ConversionConfig{Mode: config.ConversionLocal})
	if err != nil || got == strong || len(got) < 32 {
		t.Errorf("local token = %q, %v; want a fresh one", got, err)
	}
}

// The core's DjVu helpers, the page rasterizer among them (RFC GV P4), are
// what its local workers are given; every DjVu label counts as configured,
// since a worker with DjVuLibre installed has them by default.
func TestConversionHelpers_CarryTheDjVuRasterizer(t *testing.T) {
	cfg := &config.Config{Transcription: config.TranscriptionConfig{OCR: &config.OCRConfig{DjVu: &config.DjVuHelperConfig{
		Text:      &config.HelperCommand{Command: "djvutxt --detail=page --page={pages} {in}"},
		Rasterize: &config.HelperCommand{Command: "ddjvu -format=tiff -page={page} -size={size} {in} {out}"},
	}}}}
	h := conversionHelpers(cfg)
	if h.djvuRasterize != "ddjvu -format=tiff -page={page} -size={size} {in} {out}" || h.djvuText == "" {
		t.Errorf("helpers = %+v", h)
	}
	if c := h.configured(); !c[wire.LabelDjVu] || !c[wire.LabelDjVuRender] {
		t.Errorf("configured = %v", c)
	}
}
