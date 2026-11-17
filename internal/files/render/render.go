package render

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"
)

// Write writes d as format. A writer stops at MaxOutput and fails with
// ErrTooLarge rather than leave a truncated file.
func Write(ctx context.Context, w io.Writer, d *Doc, format string, opts Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch format {
	case FormatMarkdown:
		return writeMarkdown(ctx, w, d, opts)
	case FormatHTML:
		return writeHTML(ctx, w, d, opts)
	case FormatDOCX:
		return writeDOCX(ctx, w, d, opts)
	case FormatODT:
		return writeODT(ctx, w, d, opts)
	case FormatEPUB:
		return writeEPUB(ctx, w, d, opts)
	case FormatPDF:
		// A PDF is typeset by a helper program from WriteTypst's source;
		// this package runs none (internal/convert does).
		return fmt.Errorf("%w: pdf is typeset by the render helper, not written here", ErrUnsupported)
	}
	return fmt.Errorf("%w: %q", ErrUnsupported, format)
}

// bundleDoc is the bundle's JSON part.
type bundleDoc struct {
	Version int     `json:"version"`
	Doc     *Doc    `json:"doc"`
	Options Options `json:"options"`
}

const (
	bundleVersion  = 1
	bundleDocName  = "doc.json"
	bundleImageDir = "images/"
	// maxDocJSON bounds the JSON part: the bodies' cap and room for the
	// structure around them.
	maxDocJSON = 2 * MaxBodyBytes
)

// bundleEpoch is every entry's time, so the same Doc always makes the same
// bundle, and its digest (the quarantine key, RFC GO) is stable.
var bundleEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// EncodeBundle packs a Doc and its pictures for a conversion worker: one zip
// holding doc.json and images/<name>.
func EncodeBundle(d *Doc, opts Options) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	js, err := json.Marshal(bundleDoc{Version: bundleVersion, Doc: d, Options: opts})
	if err != nil {
		return nil, err
	}
	if err := addEntry(zw, bundleDocName, zip.Deflate, js); err != nil {
		return nil, err
	}
	for _, im := range d.Images {
		// Pictures are compressed already.
		if err := addEntry(zw, bundleImageDir+im.Name, zip.Store, im.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > MaxBundle {
		return nil, fmt.Errorf("%w: the document is %d MiB to render, more than %d MiB", ErrTooLarge, buf.Len()>>20, MaxBundle>>20)
	}
	return buf.Bytes(), nil
}

func addEntry(zw *zip.Writer, name string, method uint16, data []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: bundleEpoch})
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// imageNameRe is the only picture name a bundle may hold.
var imageNameRe = regexp.MustCompile(`^img-[1-9][0-9]{0,4}\.(png|jpg|gif|webp)$`)

// DecodeBundle reads a bundle EncodeBundle wrote. The bundle is untrusted
// here — a worker is sent it — so every cap is checked again: sizes as they
// are read (not as the zip declares them), the picture names, the node kinds
// and the nesting.
func DecodeBundle(r io.ReaderAt, size int64) (*Doc, Options, error) {
	if size > MaxBundle {
		return nil, Options{}, fmt.Errorf("%w: bundle of %d bytes", ErrTooLarge, size)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, Options{}, fmt.Errorf("%w: %v", ErrBadBundle, err)
	}
	if len(zr.File) > MaxImages+1 {
		return nil, Options{}, fmt.Errorf("%w: %d entries", ErrBadBundle, len(zr.File))
	}
	var bd bundleDoc
	images := map[string][]byte{}
	seenDoc := false
	for _, f := range zr.File {
		switch {
		case f.Name == bundleDocName && !seenDoc:
			seenDoc = true
			b, err := readEntry(f, maxDocJSON)
			if err != nil {
				return nil, Options{}, err
			}
			if err := json.Unmarshal(b, &bd); err != nil {
				return nil, Options{}, fmt.Errorf("%w: %v", ErrBadBundle, err)
			}
		case len(f.Name) > len(bundleImageDir) && f.Name[:len(bundleImageDir)] == bundleImageDir:
			name := f.Name[len(bundleImageDir):]
			if !imageNameRe.MatchString(name) || images[name] != nil {
				return nil, Options{}, fmt.Errorf("%w: entry %q", ErrBadBundle, f.Name)
			}
			b, err := readEntry(f, MaxImage)
			if err != nil {
				return nil, Options{}, err
			}
			images[name] = b
		default:
			return nil, Options{}, fmt.Errorf("%w: entry %q", ErrBadBundle, f.Name)
		}
	}
	if !seenDoc || bd.Doc == nil || bd.Version != bundleVersion {
		return nil, Options{}, fmt.Errorf("%w: no document of version %d", ErrBadBundle, bundleVersion)
	}
	d := bd.Doc
	if err := checkDoc(d); err != nil {
		return nil, Options{}, err
	}
	for i := range d.Images {
		im := &d.Images[i]
		data, ok := images[im.Name]
		if !ok {
			return nil, Options{}, fmt.Errorf("%w: picture %q is not in the bundle", ErrBadBundle, im.Name)
		}
		if _, ok := imageExt[im.MediaType]; !ok {
			return nil, Options{}, fmt.Errorf("%w: picture %q has type %q", ErrBadBundle, im.Name, im.MediaType)
		}
		im.Data = data
	}
	return d, bd.Options, nil
}

// readEntry reads one entry, at most max bytes whatever its header claims.
func readEntry(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadBundle, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadBundle, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w: entry %q is larger than %d bytes", ErrTooLarge, f.Name, max)
	}
	return b, nil
}

// knownKinds is every node kind a writer reads.
var knownKinds = map[string]bool{
	KindParagraph: true, KindHeading: true, KindList: true, KindItem: true, KindTable: true, KindRow: true,
	KindCell: true, KindCode: true, KindQuote: true, KindImage: true, KindDiagram: true, KindRule: true,
	KindText: true, KindEmph: true, KindStrong: true, KindStrike: true, KindCodeSpan: true, KindLink: true, KindBreak: true,
}

// checkDoc holds a decoded Doc to the shape a Builder makes.
func checkDoc(d *Doc) error {
	if len(d.Images) > MaxImages {
		return fmt.Errorf("%w: %d pictures", ErrBadBundle, len(d.Images))
	}
	if err := checkEmbed(d.Embed); err != nil {
		return err
	}
	sections := 0
	var nodes func(ns []Node, depth int) error
	nodes = func(ns []Node, depth int) error {
		if len(ns) > 0 && depth > maxDepth+2 {
			return fmt.Errorf("%w: nested deeper than %d", ErrBadBundle, maxDepth)
		}
		for _, n := range ns {
			if !knownKinds[n.Kind] {
				return fmt.Errorf("%w: node kind %q", ErrBadBundle, n.Kind)
			}
			if n.Anchor != "" && !anchorRe.MatchString(n.Anchor) {
				return fmt.Errorf("%w: anchor %q", ErrBadBundle, n.Anchor)
			}
			if err := nodes(n.Children, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	var walk func(ss []*Section, depth int) error
	walk = func(ss []*Section, depth int) error {
		if depth > maxDepth && len(ss) > 0 {
			return fmt.Errorf("%w: sections nested deeper than %d", ErrBadBundle, maxDepth)
		}
		for _, s := range ss {
			if s == nil {
				return fmt.Errorf("%w: empty section", ErrBadBundle)
			}
			if sections++; sections > MaxSections {
				return fmt.Errorf("%w: more than %d sections", ErrTooLarge, MaxSections)
			}
			if !anchorRe.MatchString(s.Anchor) {
				return fmt.Errorf("%w: anchor %q", ErrBadBundle, s.Anchor)
			}
			if err := nodes(s.Blocks, 0); err != nil {
				return err
			}
			if err := walk(s.Children, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if d.Anchor != "" && !anchorRe.MatchString(d.Anchor) {
		return fmt.Errorf("%w: anchor %q", ErrBadBundle, d.Anchor)
	}
	if err := nodes(d.Blocks, 0); err != nil {
		return err
	}
	return walk(d.Sections, 0)
}

// anchorRe is an anchor a writer puts in an attribute or a URL fragment.
var anchorRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,127}$`)

// ValidAnchor reports whether a is an anchor the writers accept.
func ValidAnchor(a string) bool { return anchorRe.MatchString(a) }

// RenderBundle decodes a bundle and writes it as format: what a conversion
// job runs.
func RenderBundle(ctx context.Context, r io.ReaderAt, size int64, format string, w io.Writer) error {
	if !Supported(format) {
		return fmt.Errorf("%w: %q", ErrUnsupported, format)
	}
	d, opts, err := DecodeBundle(r, size)
	if err != nil {
		return err
	}
	return Write(ctx, w, d, format, opts)
}

// checkEmbed holds a decoded record to the shape the core makes: its
// version, ids and anchors a writer may put into a file's syntax, at most a
// chunk per section, and fields that are a JSON object.
func checkEmbed(e *Embed) error {
	if e == nil {
		return nil
	}
	if e.Version != EmbedVersion || !embedIDRe.MatchString(e.DocumentID) || len(e.Chunks) > MaxSections+1 || len(e.Relations) > 4*MaxSections {
		return fmt.Errorf("%w: embedded record", ErrBadBundle)
	}
	switch e.Scope {
	case "agent", "user", "tenant":
	default:
		return fmt.Errorf("%w: embedded record scope %q", ErrBadBundle, e.Scope)
	}
	for _, c := range e.Chunks {
		if !embedIDRe.MatchString(c.ID) || !anchorRe.MatchString(c.Anchor) || (c.Parent != "" && !embedIDRe.MatchString(c.Parent)) {
			return fmt.Errorf("%w: embedded chunk %q", ErrBadBundle, c.ID)
		}
		if len(c.Fields) > 0 && c.Fields[0] != '{' {
			return fmt.Errorf("%w: embedded chunk %q fields", ErrBadBundle, c.ID)
		}
	}
	for _, r := range e.Relations {
		if !embedIDRe.MatchString(r.From) || !embedIDRe.MatchString(r.To) || !embedKindRe.MatchString(r.Kind) || (r.Status != "" && !embedKindRe.MatchString(r.Status)) {
			return fmt.Errorf("%w: embedded relation", ErrBadBundle)
		}
	}
	return nil
}
