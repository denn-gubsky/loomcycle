package builtin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// A volume image reference in import_md (RFC FF): `![alt](volume:<name>:<path>)`
// or `![alt](volume:<path>)` as a chunk's whole body is the bytes-by-reference
// twin of the data-URL form. The runtime reads the file and attaches it as the
// chunk's image, so an extracted document's pictures become image chunks
// without base64 passing through the model.
//
// The file is read with the run's own authority, exactly as a Crypto `from`
// reads one: the run must hold Read, the volume must be one it is bound to, the
// path must stay inside it, and Read's pre-hooks judge the call. A reference
// that cannot be read, is not an image, or is over the asset cap is imported as
// a plain Markdown link and counted as skipped; it never fails the import.

var mdVolumeImageRe = regexp.MustCompile(`^!\[([^\]]*)\]\((volume:[^\s()<>]+)\)$`)

// classifyVolumeImage reports a body that is entirely a volume image
// reference, with its alt text and reference.
func classifyVolumeImage(body string) (alt, ref string, ok bool) {
	m := mdVolumeImageRe.FindStringSubmatch(strings.TrimSpace(body))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// volumeAssetReader reads at most max bytes of a volume file for import_md;
// over max is an error. Errors are model-facing reasons.
type volumeAssetReader func(ctx context.Context, r dataRef, max int64) ([]byte, error)

type volumeAssetReaderKey struct{}

// withVolumeAssetReader lets a tool that already holds its own grant on a
// volume (the File tool and the run's inbox) supply the read for an import it
// makes. Unexported: only this package's tools set it.
func withVolumeAssetReader(ctx context.Context, fn volumeAssetReader) context.Context {
	return context.WithValue(ctx, volumeAssetReaderKey{}, fn)
}

// documentVolumeRead is import_md's own read: Read's grant and hooks, then
// the volume fence.
func documentVolumeRead(ctx context.Context, r dataRef, max int64) ([]byte, error) {
	if res, ok := checkRefGrant(ctx, r, false, "Document", "Embed the image as a data: URL"); !ok {
		return nil, errors.New(res.Text)
	}
	return readVolumeBytes(ctx, r, max)
}

// readVolumeBytes reads a volume file through the volume's resolution and
// fence, as Read does. It checks no grant: its callers do.
func readVolumeBytes(ctx context.Context, r dataRef, max int64) ([]byte, error) {
	if inboxMissing(ctx, r) {
		return nil, errors.New("nothing has been kept in this run's inbox yet")
	}
	root, fence, err := effectiveRoot(ctx, r.volume, false)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveInsideRoot(root, r.path)
	if err == nil {
		err = outsideFence(fence, resolved)
	}
	if err != nil {
		return nil, err
	}
	vfs, err := openVolumeFS(root, fence)
	if err != nil {
		return nil, err
	}
	defer vfs.Close()
	rel, err := vfs.rel(resolved)
	if err != nil {
		return nil, err
	}
	f, err := vfs.open(rel)
	if err != nil {
		return nil, fmt.Errorf("no file at %s", r.raw)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file", r.raw)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte limit", r.raw, info.Size(), max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s failed", r.raw)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over the %d-byte limit", r.raw, max)
	}
	return b, nil
}

// loadVolumeImage resolves a chunk's volume image reference into the bytes
// importAsset attaches. On any failure the chunk becomes a plain link to the
// reference, and the reason is returned.
func (d *Document) loadVolumeImage(ctx context.Context, c *mdChunk) (reason string, ok bool) {
	ref := c.assetRef
	c.assetRef = ""
	fail := func(why string) (string, bool) {
		if c.typ == "image" {
			c.typ = ""
		}
		c.body = "[" + c.body + "](" + ref + ")"
		return ref + ": " + why, false
	}
	r, err := parseDataRef(ref)
	if err != nil || r.kind != refVolume {
		return fail("not a volume reference")
	}
	read := documentVolumeRead
	if fn, ok := ctx.Value(volumeAssetReaderKey{}).(volumeAssetReader); ok && fn != nil {
		read = fn
	}
	b, err := read(ctx, r, int64(d.maxAssetBytes()))
	if err != nil {
		return fail(err.Error())
	}
	// The type is what the bytes are, not what the name says.
	mt := http.DetectContentType(b)
	if !validImageMediaTypes[mt] {
		return fail(fmt.Sprintf("%s is not a PNG, JPEG, GIF or WebP image", mt))
	}
	c.assetMediaType, c.assetData = mt, base64.StdEncoding.EncodeToString(b)
	return "", true
}
