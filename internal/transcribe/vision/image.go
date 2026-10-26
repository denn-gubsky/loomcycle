package vision

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
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

// MaxDecodeBytes bounds the estimated PEAK memory of decoding one image to
// scale it: the decoder's buffers, judged from the header alone (see
// decodeCost), not the file size and not only the output pixels. A pixel
// bound alone is not enough: a 16-bit RGBA PNG holds 8 bytes a pixel, so a
// header claiming 64 MP would decode to half a gigabyte. 64 MiB is
// 4096 × 4096 at 4 bytes: a 300 dpi A4 scan (8.7 MP) or a phone photo (12 MP)
// as a baseline 8-bit JPEG or a plain 8-bit PNG fits with room to spare.
//
// A 16-bit-per-channel image is held to the same budget rather than given one
// of its own, so a 12 MP 16-bit photo (96 MiB) is refused with advice to save
// it at 8 bits: the extra depth carries nothing a model reads text from, and
// a separate allowance would double what one call may hold.
//
// The bound is on the decode; the scaled copy it is drawn into is at most
// MaxImageEdge² × 4 bytes (under 10 MiB) on top. One decode runs at a time per
// engine (Engine.decodeSlot), so concurrent calls do not add up.
const MaxDecodeBytes = 4096 * 4096 * 4

// ImageTooLargeError refuses an image whose estimated decode would pass
// MaxDecodeBytes, judged from its header before anything is decoded.
type ImageTooLargeError struct {
	Width, Height int
	// BytesPerPixel is the estimated peak per pixel (decodeCost).
	BytesPerPixel int
	Limit         int64
	// advice is how to send a version that fits.
	advice string
}

func (e *ImageTooLargeError) Error() string {
	return fmt.Sprintf("the image is %d×%d pixels at an estimated %d bytes a pixel to decode (%d MiB), over the %d MiB limit for decoding an image to scale it; %s",
		e.Width, e.Height, e.BytesPerPixel, int64(e.Width)*int64(e.Height)*int64(e.BytesPerPixel)>>20, e.Limit>>20, e.advice)
}

// bytesPerPixel is what the decoder's output image takes per pixel for
// colour model m. An unknown model is taken at the most any takes.
func bytesPerPixel(m color.Model) int {
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	switch m {
	case color.GrayModel, color.AlphaModel:
		return 1
	case color.Gray16Model, color.Alpha16Model:
		return 2
	case color.RGBAModel, color.NRGBAModel, color.YCbCrModel, color.NYCbCrAModel, color.CMYKModel:
		return 4
	}
	return 8
}

// decodeCost estimates the peak bytes per pixel of decoding data, from the
// header only, and says how to send a smaller decode:
//
//   - PNG: the output image (bytesPerPixel), twice over when interlaced
//     (Adam7 keeps a buffer per pass beside the output); an IHDR not where
//     image/png reads it is taken at the worst case, 2 × 8;
//   - JPEG: see jpegCost;
//   - any other format (GIF, WebP, BMP): 8 bytes a pixel, the most any
//     output takes, since their decoders' buffers are not characterised.
func decodeCost(data []byte, format string, cfg image.Config) (int, string) {
	out := bytesPerPixel(cfg.ColorModel)
	switch format {
	case "png":
		interlaced, ok := pngInterlaced(data)
		switch {
		case !ok:
			return 2 * 8, "send a smaller image"
		case interlaced:
			return 2 * out, "it is interlaced: save it without interlacing, or smaller"
		case out > 4:
			return out, "it is stored at 16 bits per channel: save it at 8 bits per channel"
		}
		return out, "send a smaller image"
	case "jpeg":
		return jpegCost(data, cfg)
	}
	return 8, "send a smaller image, or a PNG or JPEG"
}

// jpegWorstCase is the most a JPEG decode holds per pixel in image/jpeg: a
// progressive 4-component (CMYK/YCCK) file keeps 64 int32 coefficients per
// 8×8 block for each of its 4 components (16 bytes a pixel) beside planes
// converted into a second, CMYK, image (8).
const jpegWorstCase = 8 + 4*4

// jpegCost is a JPEG's decode estimate. The component count is the
// decoder's own (DecodeConfig's colour model). Whether the file is
// progressive is not trusted to a lenient read: the cheap, baseline estimate
// holds only when a strict walk proves exactly one SOF0/SOF1 frame before the
// first scan. image/jpeg skips junk bytes and "FF 00" that a strict walk
// stops at, so anything that walk cannot read is budgeted at the worst case
// rather than at a guess a crafted file could steer.
func jpegCost(data []byte, cfg image.Config) (int, string) {
	comps, out := 0, 0
	switch cfg.ColorModel {
	case color.GrayModel:
		comps, out = 1, 1
	case color.YCbCrModel:
		comps, out = 3, 4
	case color.CMYKModel:
		// Decoded to planes, then converted into a CMYK image.
		comps, out = 4, 8
	default:
		return jpegWorstCase, "send a smaller image"
	}
	progressive, ok := jpegFrameStrict(data)
	switch {
	case !ok:
		return jpegWorstCase, "send a smaller image, saved as a plain baseline JPEG"
	case progressive:
		return out + 4*comps, "it is a progressive JPEG: save it as a baseline JPEG, or smaller"
	}
	return out, "send a smaller image"
}

// pngInterlaced reads the IHDR interlace byte where image/png reads it: the
// first chunk, after the signature (8) and the chunk's length and type (8),
// 12 bytes into its data. ok is false when it is not there or not 0 or 1.
func pngInterlaced(data []byte) (interlaced, ok bool) {
	if len(data) <= 28 || string(data[12:16]) != "IHDR" || data[28] > 1 {
		return false, false
	}
	return data[28] == 1, true
}

// jpegFrameStrict walks the marker segments from SOI, each starting exactly
// "FF xx" with xx a marker, up to the first scan, and reports whether the one
// frame header found is progressive. ok is false for anything else: a byte
// out of place, a fill byte, "FF 00", a segment running past the end, no
// frame header, or more than one.
func jpegFrameStrict(data []byte) (progressive, ok bool) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return false, false
	}
	frames := 0
	i := 2
	for {
		if i+4 > len(data) || data[i] != 0xFF {
			return false, false
		}
		m := data[i+1]
		switch {
		case m == 0xDA: // start of scan: the header is complete
			if frames != 1 {
				return false, false
			}
			return progressive, true
		case m == 0x00, m == 0xFF, m == 0x01, m == 0xD8, m == 0xD9, m >= 0xD0 && m <= 0xD7:
			// Fill, stuffing, standalone markers and a second SOI or an EOI
			// have no place between SOI and the first scan here.
			return false, false
		}
		n := int(data[i+2])<<8 | int(data[i+3])
		if n < 2 || i+2+n > len(data) {
			return false, false
		}
		// SOFn is 0xC0–0xCF except DHT (C4), JPG (C8) and DAC (CC).
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			frames++
			progressive = m != 0xC0 && m != 0xC1
		}
		i += 2 + n
	}
}

// sendable are the types every vision driver takes (the run input's image
// whitelist).
var sendable = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// prepareImage returns the image as it is sent: unchanged when the model
// takes its type and it fits MaxImageEdge, otherwise decoded, scaled to fit
// and encoded again (JPEG stays JPEG; everything else becomes PNG).
func (e *Engine) prepareImage(ctx context.Context, data []byte, mediaType string) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("the image could not be read: %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", fmt.Errorf("the image header says %d×%d pixels", cfg.Width, cfg.Height)
	}
	// Sent as it is: never decoded, so its size costs nothing here.
	if sendable[mediaType] && max(cfg.Width, cfg.Height) <= MaxImageEdge {
		return data, mediaType, nil
	}
	bpp, advice := decodeCost(data, format, cfg)
	if int64(cfg.Width)*int64(cfg.Height)*int64(bpp) > MaxDecodeBytes {
		return nil, "", &ImageTooLargeError{Width: cfg.Width, Height: cfg.Height, BytesPerPixel: bpp, Limit: MaxDecodeBytes, advice: advice}
	}
	// One decode at a time per engine, so concurrent OCR calls cannot add
	// their peaks up: MaxDecodeBytes is then what decoding holds at most.
	select {
	case e.decodeSlot <- struct{}{}:
		defer func() { <-e.decodeSlot }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	src, _, err := e.decode(bytes.NewReader(data))
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
