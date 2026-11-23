package tools

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // header readers for image.DecodeConfig
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Image is one picture a tool returns for the model to see (RFC GW §5.8).
// Width and Height are filled in by the dispatcher from the picture's header.
type Image struct {
	MediaType string
	Data      []byte
	Width     int
	Height    int
}

const (
	// MaxResultImages is the most pictures one call may return.
	MaxResultImages = 4
	// MaxImageEdge is the longest side a picture is sent at; a larger one is
	// scaled. It is vision.MaxImageEdge, the OCR path's bound (a test holds
	// the two equal): Claude scales anything larger down to about this and
	// bills by the pixels it keeps, so more costs context for nothing.
	MaxImageEdge = 1568
	// MaxImageBytes bounds one picture as sent: 5,000,000 bytes in base64,
	// Anthropic's limit for one image. It also bounds what the transcript
	// keeps for a call (MaxResultImages of these).
	MaxImageBytes = 3_750_000
)

// ImageReencoder decodes a picture and encodes it again, scaled to fit
// MaxImageEdge, refusing one whose decode would be too large from its header.
// The server passes its own, which runs in the conversion workers when there
// are any, so a hostile picture is decoded outside the core.
type ImageReencoder interface {
	ReencodeImage(ctx context.Context, data []byte) ([]byte, string, error)
}

// SetImageReencoder sets how a picture over MaxImageEdge is scaled.
func (d *Dispatcher) SetImageReencoder(r ImageReencoder) { d.images = r }

// headerTypes maps image.DecodeConfig's format names onto the media types
// every vision driver takes (the RFC AT whitelist).
var headerTypes = map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}

// checkImages checks a result's pictures: how many, what each is (read from
// its header, never from what the tool declared), its size, scaled to fit
// when it is over the edge. A picture that cannot be sent fails the call,
// saying why, with the tool's own text after it: a picture the model asked
// for and does not get must not look like a success.
func (d *Dispatcher) checkImages(ctx context.Context, res Result) Result {
	if len(res.Images) == 0 {
		return res
	}
	if len(res.Images) > MaxResultImages {
		return refusePictures(res, fmt.Sprintf("the tool returned %d pictures; a call may return at most %d", len(res.Images), MaxResultImages))
	}
	out := make([]Image, len(res.Images))
	for i, im := range res.Images {
		fitted, err := d.fitImage(ctx, im)
		if err != nil {
			return refusePictures(res, fmt.Sprintf("picture %d: %v", i+1, err))
		}
		out[i] = fitted
	}
	res.Images = out
	return res
}

func (d *Dispatcher) fitImage(ctx context.Context, im Image) (Image, error) {
	if len(im.Data) == 0 {
		return Image{}, fmt.Errorf("the picture is empty")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(im.Data))
	if err != nil {
		return Image{}, fmt.Errorf("the picture could not be read (PNG, JPEG, GIF or WebP): %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Image{}, fmt.Errorf("the picture's header says %d×%d pixels", cfg.Width, cfg.Height)
	}
	data, mediaType := im.Data, headerTypes[format]
	if mediaType == "" || max(cfg.Width, cfg.Height) > MaxImageEdge {
		if d.images == nil {
			return Image{}, fmt.Errorf("the picture is %d×%d pixels, over the %d pixels a picture is sent at on its long side, and nothing here can scale it; send a smaller one",
				cfg.Width, cfg.Height, MaxImageEdge)
		}
		data, mediaType, err = d.images.ReencodeImage(ctx, im.Data)
		if err != nil {
			return Image{}, err
		}
		if cfg, _, err = image.DecodeConfig(bytes.NewReader(data)); err != nil {
			return Image{}, fmt.Errorf("the scaled picture could not be read: %v", err)
		}
	}
	if len(data) > MaxImageBytes {
		return Image{}, fmt.Errorf("the picture is %d bytes as sent, over the %d a picture may be", len(data), MaxImageBytes)
	}
	return Image{MediaType: mediaType, Data: data, Width: cfg.Width, Height: cfg.Height}, nil
}

// refusePictures is res as a failed call that carries no pictures.
func refusePictures(res Result, why string) Result {
	text := "the tool's pictures were not sent: " + why
	if res.Text != "" {
		text += "\n\n" + res.Text
	}
	return Result{Text: text, IsError: true, Error: res.Error, Count: res.Count}
}
