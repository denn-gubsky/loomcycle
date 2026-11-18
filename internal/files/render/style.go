package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A Style is a tenant's house style (RFC GR P4c, D19–D20, §12.1): data only,
// every value checked against a closed set or a range, applied by every
// writer but Markdown. It is never code: nothing in it is read as markup or a
// renderer instruction, and a header or footer line is written as text.

// StyleVersion is the version of a style's shape.
const StyleVersion = 1

// The ranges a style's values are held to.
const (
	minStyleSize, maxStyleSize       = 8.0, 14.0
	minHeadingScale, maxHeadingScale = 0.8, 1.5
	minMarginMM, maxMarginMM         = 10.0, 50.0
	minLogoMM, maxLogoMM             = 10.0, 80.0
	maxStyleLine                     = 120
	maxStyleJSON                     = 16 << 10
)

// Style is a parsed, checked style. A zero field is the built-in value; use
// the accessors, which apply the defaults.
type Style struct {
	Version     int         `json:"loomcycle_style"`
	Font        string      `json:"font,omitempty"`
	Size        float64     `json:"size,omitempty"`
	HeadScale   float64     `json:"heading_scale,omitempty"`
	Colors      StyleColors `json:"colors,omitempty"`
	Page        StylePage   `json:"page,omitempty"`
	Header      string      `json:"header,omitempty"`
	Footer      string      `json:"footer,omitempty"`
	PageNumbers *bool       `json:"page_numbers,omitempty"`
	LogoWidthMM float64     `json:"logo_width_mm,omitempty"`
	// Logo is the picture above the title: an image of the Doc, by name.
	// Set by the core from the style document, never by the JSON.
	Logo string `json:"logo,omitempty"`
	// Source names the style document and its revision, so a changed
	// style changes what a render is made of (the render log's reuse).
	Source string `json:"source,omitempty"`
}

// StyleColors are a style's colours, each #rrggbb.
type StyleColors struct {
	Text    string `json:"text,omitempty"`
	Heading string `json:"heading,omitempty"`
	Link    string `json:"link,omitempty"`
}

// StylePage is a style's page settings for the paged formats.
type StylePage struct {
	Size     string  `json:"size,omitempty"`
	MarginMM float64 `json:"margin_mm,omitempty"`
}

// ErrBadStyle is a style that does not parse or holds a value out of range;
// the message names the field.
var ErrBadStyle = errors.New("render: style")

var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// ParseStyle reads a style document's JSON: the root body as it is, or one
// fenced block of it. Unknown keys are refused, so a misspelled key is never
// silently ignored.
func ParseStyle(body string) (*Style, error) {
	raw := strings.TrimSpace(body)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) < 2 || strings.TrimSpace(lines[len(lines)-1]) != "```" {
			return nil, fmt.Errorf("%w: the body is a fenced block that is not closed", ErrBadStyle)
		}
		raw = strings.Join(lines[1:len(lines)-1], "\n")
	}
	if len(raw) > maxStyleJSON {
		return nil, fmt.Errorf("%w: more than %d KiB", ErrBadStyle, maxStyleJSON>>10)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var s Style
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBadStyle, styleJSONError(err))
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: more than one JSON value", ErrBadStyle)
	}
	if s.Logo != "" || s.Source != "" {
		return nil, fmt.Errorf("%w: logo and source are not keys of a style (the logo is the document's first picture)", ErrBadStyle)
	}
	if err := s.check(); err != nil {
		return nil, err
	}
	return &s, nil
}

// styleJSONError is a decoder error as the field it is about.
func styleJSONError(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return fmt.Sprintf("%s: not a %s", te.Field, te.Type)
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field ") {
		return "unknown key " + strings.TrimPrefix(msg, "json: unknown field ")
	}
	return "not JSON: " + msg
}

// check holds every value to its range; it is also what a worker holds a
// bundled style to.
func (s *Style) check() error {
	bad := func(field, want string) error { return fmt.Errorf("%w: %s must be %s", ErrBadStyle, field, want) }
	if s.Version != StyleVersion {
		return bad("loomcycle_style", fmt.Sprint(StyleVersion))
	}
	switch s.Font {
	case "", "sans", "serif":
	default:
		return bad("font", "sans or serif")
	}
	if s.Size != 0 && (s.Size < minStyleSize || s.Size > maxStyleSize) {
		return bad("size", fmt.Sprintf("between %g and %g", minStyleSize, maxStyleSize))
	}
	if s.HeadScale != 0 && (s.HeadScale < minHeadingScale || s.HeadScale > maxHeadingScale) {
		return bad("heading_scale", fmt.Sprintf("between %g and %g", minHeadingScale, maxHeadingScale))
	}
	for name, c := range map[string]string{"colors.text": s.Colors.Text, "colors.heading": s.Colors.Heading, "colors.link": s.Colors.Link} {
		if c != "" && !hexColorRe.MatchString(c) {
			return bad(name, "a colour as #rrggbb")
		}
	}
	switch s.Page.Size {
	case "", "a4", "letter":
	default:
		return bad("page.size", "a4 or letter")
	}
	if s.Page.MarginMM != 0 && (s.Page.MarginMM < minMarginMM || s.Page.MarginMM > maxMarginMM) {
		return bad("page.margin_mm", fmt.Sprintf("between %g and %g", minMarginMM, maxMarginMM))
	}
	for name, l := range map[string]string{"header": s.Header, "footer": s.Footer} {
		if !styleLineOK(l) {
			return bad(name, fmt.Sprintf("one line of at most %d characters, without control characters", maxStyleLine))
		}
	}
	if s.LogoWidthMM != 0 && (s.LogoWidthMM < minLogoMM || s.LogoWidthMM > maxLogoMM) {
		return bad("logo_width_mm", fmt.Sprintf("between %g and %g", minLogoMM, maxLogoMM))
	}
	if s.Logo != "" && !imageNameRe.MatchString(s.Logo) {
		return bad("logo", "a picture of the document")
	}
	if utf8.RuneCountInString(s.Source) > 256 {
		return bad("source", "at most 256 characters")
	}
	return nil
}

func styleLineOK(l string) bool {
	if !utf8.ValidString(l) || utf8.RuneCountInString(l) > maxStyleLine {
		return false
	}
	for _, r := range l {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// The built-in look (D13–D15), and the style's values over it.

func (s *Style) serif() bool { return s != nil && s.Font == "serif" }

// BaseSize is the body size in points.
func (s *Style) BaseSize() float64 {
	if s == nil || s.Size == 0 {
		return 10.5
	}
	return s.Size
}

func (s *Style) headScale() float64 {
	if s == nil || s.HeadScale == 0 {
		return 1
	}
	return s.HeadScale
}

func (s *Style) textColor() string {
	return styleColor(s, func(c StyleColors) string { return c.Text }, "#1b1b1b")
}
func (s *Style) headingColor() string {
	return styleColor(s, func(c StyleColors) string { return c.Heading }, "#1b1b1b")
}
func (s *Style) linkColor() string {
	return styleColor(s, func(c StyleColors) string { return c.Link }, "#1d4ed8")
}

func styleColor(s *Style, pick func(StyleColors) string, def string) string {
	if s != nil {
		if c := pick(s.Colors); c != "" {
			return strings.ToLower(c)
		}
	}
	return def
}

// marginMM is the paged formats' margin, the built-in 25.4 mm (one inch)
// when the style sets none.
func (s *Style) marginMM() float64 {
	if s == nil || s.Page.MarginMM == 0 {
		return 25.4
	}
	return s.Page.MarginMM
}

// pageNumbers reports whether a paged format numbers its pages in the footer.
func (s *Style) pageNumbers() bool { return s == nil || s.PageNumbers == nil || *s.PageNumbers }

func (s *Style) logoMM() float64 {
	if s == nil || s.LogoWidthMM == 0 {
		return 30
	}
	return s.LogoWidthMM
}

// PageSize is the paged formats' size: the call's own option, then the
// style's, then A4.
func (s *Style) PageSize(opt string) string {
	if opt != "" {
		return opt
	}
	if s != nil && s.Page.Size != "" {
		return s.Page.Size
	}
	return "a4"
}

// header and footer are the style's lines, "" without a style.
func (s *Style) header() string {
	if s == nil {
		return ""
	}
	return s.Header
}

func (s *Style) footer() string {
	if s == nil {
		return ""
	}
	return s.Footer
}

// logo is the style's picture, when the Doc holds it as one it can show.
func (d *Doc) logo() (Image, bool) {
	if d.Style == nil || d.Style.Logo == "" {
		return Image{}, false
	}
	return d.image(d.Style.Logo)
}

// cssFonts are the font stacks a style names, the reader's own behind Noto.
const (
	cssSans  = `"Noto Sans","Segoe UI",Calibri,Arial,sans-serif`
	cssSerif = `"Noto Serif",Georgia,"Times New Roman",serif`
)

// cssOverrides are the stylesheet rules a style sets over the built-in look,
// for the HTML page (scope article) and the EPUB book (scope body). Every
// value in them was checked: a hex colour, a number in range, one of two
// font stacks.
func (s *Style) cssOverrides(scope string) string {
	if s == nil {
		return ""
	}
	font := cssSans
	if s.serif() {
		font = cssSerif
	}
	k := s.headScale()
	var b strings.Builder
	fmt.Fprintf(&b, "%s{font-family:%s;font-size:%gpt;color:%s}\n", scope, font, s.BaseSize(), s.textColor())
	fmt.Fprintf(&b, "h1,h2,h3,h4,h5,h6{color:%s}\n", s.headingColor())
	fmt.Fprintf(&b, "h1{font-size:%.3gem}h2{font-size:%.3gem}h3{font-size:%.3gem}h4,h5,h6{font-size:%.3gem}\n", 2*k, 1.5*k, 1.25*k, 1.05*k)
	fmt.Fprintf(&b, "a{color:%s}\n", s.linkColor())
	b.WriteString(".style-header,.style-footer{color:#5b5b5b;font-size:.8em;margin:0 0 1.5em}.style-footer{margin:2em 0 0;border-top:1px solid #d6d3d1;padding-top:.5em}\n")
	fmt.Fprintf(&b, "img.logo{width:%gmm;max-width:100%%;height:auto;display:block;margin:0 0 1em}\n", s.logoMM())
	return b.String()
}
