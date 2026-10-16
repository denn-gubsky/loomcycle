package grpc

import (
	"encoding/base64"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
)

// TestSegmentsFromProto_ImageBytesToBase64: a proto image content block carries
// raw bytes; segmentsFromProto maps them onto the loop's base64-string Data
// field (uniform with the HTTP wire) and passes media_type through. Non-image
// blocks keep empty image fields.
func TestSegmentsFromProto_ImageBytesToBase64(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a} // arbitrary bytes (PNG magic-ish)
	segs := []*loomcyclepb.PromptSegment{{
		Role: "user",
		Content: []*loomcyclepb.PromptContentBlock{
			{Type: "trusted-text", Text: "describe"},
			{Type: "image", MediaType: "image/png", Data: raw},
		},
	}}

	out := segmentsFromProto(segs)
	if len(out) != 1 || len(out[0].Content) != 2 {
		t.Fatalf("unexpected shape: %+v", out)
	}

	text := out[0].Content[0]
	if text.Type != "trusted-text" || text.MediaType != "" || text.Data != "" {
		t.Errorf("text block should carry no image fields: %+v", text)
	}

	img := out[0].Content[1]
	if img.Type != "image" {
		t.Fatalf("second block type = %q, want image", img.Type)
	}
	if img.MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", img.MediaType)
	}
	if want := base64.StdEncoding.EncodeToString(raw); img.Data != want {
		t.Errorf("Data = %q, want base64 %q", img.Data, want)
	}
}

// An untrusted block's kind crosses gRPC as on the HTTP wire, so a gRPC
// caller can send code to be fenced as written rather than escaped.
func TestSegmentsFromProto_CarriesTheUntrustedKind(t *testing.T) {
	out := segmentsFromProto([]*loomcyclepb.PromptSegment{{
		Role: "user",
		Content: []*loomcyclepb.PromptContentBlock{
			{Type: "untrusted-block", Kind: "code", Text: "a<b"},
			{Type: "untrusted-block", Text: "no kind"},
		},
	}})
	if got := out[0].Content[0]; got.Kind != "code" || got.Text != "a<b" {
		t.Errorf("first block = %+v, want kind code", got)
	}
	if got := out[0].Content[1].Kind; got != "" {
		t.Errorf("second block kind = %q, want empty", got)
	}
}
