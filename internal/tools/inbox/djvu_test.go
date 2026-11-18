package inbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

// TestSniff_DjVuIsNotAnImage: a DjVu file is typed image/vnd.djvu by its own
// bytes (RFC GV §4.1), whatever its name says, and kept with the documents:
// it is a book, never an image a model could be shown.
func TestSniff_DjVuIsNotAnImage(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	for _, fixture := range []string{"single.djvu", "book.djvu"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "djvu", fixture))
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range []inbox.Item{
			{Fallback: "book", MediaType: "application/octet-stream", Origin: "o"},
			{Name: "scan.bin", Origin: "o"},
			{Name: "scan.djv", MediaType: "image/x-djvu", Origin: "o"},
		} {
			res := inbox.Capture(r.Ctx, item, data)
			if res.Record.MediaType != extract.DjVuMediaType {
				t.Errorf("%s %+v: media type = %q", fixture, item, res.Record.MediaType)
			}
			if !strings.HasPrefix(res.Record.Path, "documents/") {
				t.Errorf("%s %+v: path = %q, want documents/", fixture, item, res.Record.Path)
			}
			if item.Name == "" && !strings.HasSuffix(res.Record.Path, ".djvu") {
				t.Errorf("%s: a nameless DjVu was named %q", fixture, res.Record.Path)
			}
		}
	}
	for _, name := range []string{"a.djvu", "a.DJV"} {
		if mt := inbox.Sniff("", []byte("not the bytes"), name); mt != extract.DjVuMediaType {
			t.Errorf("Sniff(%s) = %q", name, mt)
		}
	}
	if inbox.DirFor(extract.DjVuMediaType) != inbox.DirDocuments || inbox.DirFor("image/png") != inbox.DirImages {
		t.Error("DirFor does not keep DjVu with the documents and images with the images")
	}
	if (extract.ImageFile{}).Match(extract.DjVuMediaType, "") || (extract.ImageFile{}).Match("", "scan.djvu") {
		t.Error("the image extractor matches a DjVu file")
	}
	if _, ok := extract.Default().For(extract.DjVuMediaType, "scan.djvu"); ok {
		t.Error("the pure-Go registry reads DjVu; only a converter with the helpers may")
	}
}
