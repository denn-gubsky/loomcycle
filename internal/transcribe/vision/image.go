package vision

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // decoders for DecodeConfig / Decode
	"image/jpeg"
	"image/png"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxImageEdge is the longest side, in pixels, an image is sent at. Claude
// scales anything larger down to about this on its side and bills by the
// pixels it keeps, and a local vision model's token count grows with the
// pixels too, so a larger image costs upload and context for nothing. A page
// rendered at 150 dpi (A4: 1240×1754) is only slightly reduced.
const MaxImageEdge = 1568

// maxDecodePixels refuses to decode an image whose header claims more pixels
// than this: a few bytes of PNG can declare a picture that would take
// gigabytes to hold.
const maxDecodePixels = 64 << 20

// sendable are the types every vision driver takes (the run input's image
// whitelist).
var sendable = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// prepareImage returns the image as it is sent: unchanged when the model
// takes its type and it fits MaxImageEdge, otherwise decoded, scaled to fit
// and encoded again (JPEG stays JPEG; everything else becomes PNG).
func prepareImage(data []byte, mediaType string) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("the image could not be read: %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, "", fmt.Errorf("the image is %d×%d pixels, over the %d-pixel limit", cfg.Width, cfg.Height, maxDecodePixels)
	}
	if sendable[mediaType] && max(cfg.Width, cfg.Height) <= MaxImageEdge {
		return data, mediaType, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("the image could not be decoded: %v", err)
	}
	w, h := cfg.Width, cfg.Height
	if long := max(w, h); long > MaxImageEdge {
		w, h = max(w*MaxImageEdge/long, 1), max(h*MaxImageEdge/long, 1)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var b bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&b, dst, &jpeg.Options{Quality: 90}); err != nil {
			return nil, "", err
		}
		return b.Bytes(), "image/jpeg", nil
	}
	if err := png.Encode(&b, dst); err != nil {
		return nil, "", err
	}
	return b.Bytes(), "image/png", nil
}
