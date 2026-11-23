package tools

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/transcribe/vision"
)

// pictureTool returns its canned result, pictures included.
type pictureTool struct{ res Result }

func (p *pictureTool) Name() string                 { return "Pic" }
func (p *pictureTool) Description() string          { return "" }
func (p *pictureTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *pictureTool) Execute(context.Context, json.RawMessage) (Result, error) {
	return p.res, nil
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngHeaderOnly is a PNG signature and IHDR claiming w×h: enough for
// DecodeConfig, nothing to decode.
func pngHeaderOnly(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 16, 6
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

// fakeReencoder scales anything to fit by returning a picture of the given
// size, and counts its calls.
type fakeReencoder struct {
	calls int
	out   []byte
	err   error
}

func (f *fakeReencoder) ReencodeImage(context.Context, []byte) ([]byte, string, error) {
	f.calls++
	return f.out, "image/png", f.err
}

// visionReencoder is the OCR path's re-encoder behind this package's
// interface, as the server wires it (through convert.InProcess).
type visionReencoder struct{ r *vision.Reencoder }

func (v visionReencoder) ReencodeImage(ctx context.Context, data []byte) ([]byte, string, error) {
	return v.r.Reencode(ctx, data)
}

func callPic(t *testing.T, d *Dispatcher) Result {
	t.Helper()
	return d.Execute(context.Background(), "Pic", json.RawMessage(`{}`))
}

// A picture that fits is sent as it is, its type taken from its header and
// its size recorded.
func TestToolImages_FittingPictureSentWithHeaderType(t *testing.T) {
	data := encodePNG(t, 40, 30)
	d := NewDispatcher([]Tool{&pictureTool{res: Result{Text: "chart", Images: []Image{{MediaType: "image/jpeg", Data: data}}}}})
	r := callPic(t, d)
	if r.IsError || len(r.Images) != 1 {
		t.Fatalf("result = %+v", r)
	}
	im := r.Images[0]
	if im.MediaType != "image/png" || im.Width != 40 || im.Height != 30 || !bytes.Equal(im.Data, data) {
		t.Errorf("image = %s %d×%d, %d bytes", im.MediaType, im.Width, im.Height, len(im.Data))
	}
}

// A picture over the edge is scaled through the re-encoder, and the size it
// is sent at is the scaled one.
func TestToolImages_OverEdgeScaledThroughReencoder(t *testing.T) {
	re := &fakeReencoder{out: encodePNG(t, MaxImageEdge, 784)}
	d := NewDispatcher([]Tool{&pictureTool{res: Result{Images: []Image{{MediaType: "image/png", Data: encodePNG(t, 2000, 1000)}}}}})
	d.SetImageReencoder(re)
	r := callPic(t, d)
	if r.IsError || re.calls != 1 {
		t.Fatalf("result = %q, reencoder calls = %d", r.Text, re.calls)
	}
	if im := r.Images[0]; im.Width != MaxImageEdge || im.Height != 784 {
		t.Errorf("sent at %d×%d, want %d×784", im.Width, im.Height, MaxImageEdge)
	}
}

// With the real re-encoder, a header claiming a decode over the bound is
// refused from the header, and the call fails saying why, keeping the tool's
// text.
func TestToolImages_HugeHeaderRefusedWithoutDecoding(t *testing.T) {
	d := NewDispatcher([]Tool{&pictureTool{res: Result{Text: "made it", Images: []Image{{MediaType: "image/png", Data: pngHeaderOnly(8000, 8000)}}}}})
	d.SetImageReencoder(visionReencoder{vision.NewReencoder()})
	r := callPic(t, d)
	if !r.IsError || len(r.Images) != 0 {
		t.Fatalf("want a refused call with no pictures, got %+v", r)
	}
	if !strings.Contains(r.Text, "8000×8000") || !strings.Contains(r.Text, "made it") {
		t.Errorf("text = %q, want the header refusal and the tool's own text", r.Text)
	}
}

// Without a re-encoder a picture over the edge is refused, never sent
// unscaled.
func TestToolImages_OverEdgeWithoutReencoderRefused(t *testing.T) {
	d := NewDispatcher([]Tool{&pictureTool{res: Result{Images: []Image{{MediaType: "image/png", Data: encodePNG(t, 1600, 10)}}}}})
	r := callPic(t, d)
	if !r.IsError || len(r.Images) != 0 || !strings.Contains(r.Text, "1568") {
		t.Errorf("result = %+v", r)
	}
}

// What is not a picture of a sendable type, too many pictures, and a picture
// still too large after scaling, are each refused.
func TestToolImages_BoundsRefused(t *testing.T) {
	small := encodePNG(t, 4, 4)
	many := make([]Image, MaxResultImages+1)
	for i := range many {
		many[i] = Image{MediaType: "image/png", Data: small}
	}
	big := &fakeReencoder{out: append(encodePNG(t, 10, 10), make([]byte, MaxImageBytes)...)}
	cases := []struct {
		name string
		res  Result
		re   ImageReencoder
		want string
	}{
		{"not a picture", Result{Images: []Image{{MediaType: "image/png", Data: []byte("%PDF-1.7")}}}, nil, "could not be read"},
		{"empty", Result{Images: []Image{{MediaType: "image/png"}}}, nil, "empty"},
		{"too many", Result{Images: many}, nil, "at most 4"},
		{"too large after scaling", Result{Images: []Image{{MediaType: "image/png", Data: encodePNG(t, 2000, 10)}}}, big, "bytes"},
		{"reencoder fails", Result{Images: []Image{{MediaType: "image/png", Data: encodePNG(t, 2000, 10)}}}, &fakeReencoder{err: errors.New("worker gone")}, "worker gone"},
	}
	for _, c := range cases {
		d := NewDispatcher([]Tool{&pictureTool{res: c.res}})
		if c.re != nil {
			d.SetImageReencoder(c.re)
		}
		r := callPic(t, d)
		if !r.IsError || len(r.Images) != 0 || !strings.Contains(r.Text, c.want) {
			t.Errorf("%s: result = %q (error %v, %d pictures), want a refusal containing %q", c.name, r.Text, r.IsError, len(r.Images), c.want)
		}
	}
}

// A result without pictures is untouched.
func TestToolImages_NoPicturesUnchanged(t *testing.T) {
	d := NewDispatcher([]Tool{&pictureTool{res: Result{Text: "plain"}}})
	if r := callPic(t, d); r.IsError || r.Text != "plain" || r.Images != nil {
		t.Errorf("result = %+v", r)
	}
}

// The edge a tool's picture is sent at is the OCR path's: one rule for every
// picture a model is shown.
func TestToolImages_EdgeIsTheVisionPathEdge(t *testing.T) {
	if MaxImageEdge != vision.MaxImageEdge {
		t.Errorf("tools.MaxImageEdge = %d, vision.MaxImageEdge = %d", MaxImageEdge, vision.MaxImageEdge)
	}
}
