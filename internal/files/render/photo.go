package render

import "fmt"

// A photo beside the title (RFC HB D4): a picture placed left or right of
// the title block (the title, its meta line and the blocks before the first
// heading), or above the title. It is the call's picture, or the document's
// first picture before its first heading; either way the core picks it, so
// no document text steers where it goes.

// StylePhoto is where and how big the photo is.
type StylePhoto struct {
	// Place is left, right (the default) or above.
	Place string `json:"place,omitempty"`
	// WidthMM is the photo's width, 15–60 mm (default 32).
	WidthMM float64 `json:"width_mm,omitempty"`
	// Shape is square (the default) or circle; Word and ODT draw a square.
	Shape string `json:"shape,omitempty"`
}

const minPhotoMM, maxPhotoMM = 15.0, 60.0

func (p *StylePhoto) check() error {
	if p == nil {
		return nil
	}
	switch p.Place {
	case "", "left", "right", "above":
	default:
		return fmt.Errorf("%w: photo.place must be left, right or above", ErrBadStyle)
	}
	if p.WidthMM != 0 && (p.WidthMM < minPhotoMM || p.WidthMM > maxPhotoMM) {
		return fmt.Errorf("%w: photo.width_mm must be between %g and %g", ErrBadStyle, minPhotoMM, maxPhotoMM)
	}
	switch p.Shape {
	case "", "square", "circle":
	default:
		return fmt.Errorf("%w: photo.shape must be square or circle", ErrBadStyle)
	}
	return nil
}

// photoPlace is left, right or above.
func (s *Style) photoPlace() string {
	if s == nil || s.Photo == nil || s.Photo.Place == "" {
		return "right"
	}
	return s.Photo.Place
}

func (s *Style) photoMM() float64 {
	if s == nil || s.Photo == nil || s.Photo.WidthMM == 0 {
		return 32
	}
	return s.Photo.WidthMM
}

func (s *Style) photoCircle() bool { return s != nil && s.Photo != nil && s.Photo.Shape == "circle" }

// photo is the style's photo, when the Doc holds it as a picture it shows.
func (d *Doc) photo() (Image, bool) {
	if d.Style == nil || d.Style.PhotoImage == "" {
		return Image{}, false
	}
	return d.image(d.Style.PhotoImage)
}

// AddPhoto gives the Doc the call's photo as a picture of its own, as
// AddLogo does the logo, and reports whether it took it.
func (d *Doc) AddPhoto(mediaType string, data []byte) bool {
	ext, ok := imageExt[mediaType]
	if d.Style == nil || !ok || len(data) == 0 || len(data) > MaxImage || len(d.Images) >= MaxImages {
		return false
	}
	name := fmt.Sprintf("img-%d.%s", len(d.Images)+1, ext)
	if _, taken := d.image(name); taken {
		return false
	}
	d.Images = append(d.Images, Image{Name: name, MediaType: mediaType, Data: data})
	d.Style.PhotoImage = name
	return true
}

// TakePhoto makes the Doc's first picture before its first heading the
// photo, taking it out of the flow: the first picture among the root's own
// blocks (alone, or alone in its paragraph), else a first section that is
// only a picture (an image chunk). It reports whether it found one.
func (d *Doc) TakePhoto() bool {
	if d.Style == nil || d.Style.PhotoImage != "" {
		return false
	}
	for i, n := range d.Blocks {
		if name, ok := d.lonePicture(n); ok {
			d.Blocks = append(d.Blocks[:i:i], d.Blocks[i+1:]...)
			d.Style.PhotoImage = name
			return true
		}
	}
	if len(d.Sections) > 0 {
		s := d.Sections[0]
		if len(s.Children) == 0 && len(s.Blocks) == 1 {
			if name, ok := d.lonePicture(s.Blocks[0]); ok {
				d.Sections = d.Sections[1:]
				d.Style.PhotoImage = name
				return true
			}
		}
	}
	return false
}

// lonePicture is a block that is a picture and nothing else.
func (d *Doc) lonePicture(n Node) (string, bool) {
	if n.Kind == KindParagraph && len(n.Children) == 1 {
		n = n.Children[0]
	}
	if n.Kind != KindImage {
		return "", false
	}
	if _, ok := d.image(n.Asset); !ok {
		return "", false
	}
	return n.Asset, true
}
