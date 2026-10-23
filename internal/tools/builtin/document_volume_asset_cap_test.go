package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// One import reads at most 64 volume image references, each file once
// however often it is named, and each just before its chunk is attached: a
// document naming many images never holds all their bytes at once.
func TestImport_VolumeImageRefsAreCappedAndLoadedLazily(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx, docs, _ := volumeDocCtx(t, base, []string{"Document", "Read"})
	pic := testPNG(t)
	const distinct = 70
	for i := range distinct {
		if err := os.WriteFile(filepath.Join(docs, fmt.Sprintf("img-%d.png", i)), pic, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	reads := map[string]int{}
	total := 0
	lazy := true
	ctx = withVolumeAssetReader(ctx, func(ctx context.Context, r dataRef, max int64) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		// Every file read before this one is already attached.
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_assets`)
		if err != nil {
			t.Error(err)
		} else if n := asInt(res.Rows[0][0]); n < total {
			lazy = false
		}
		reads[r.path]++
		total++
		return readVolumeBytes(ctx, r, max)
	})

	var md strings.Builder
	md.WriteString("# Альбом\n")
	n := 0
	ref := func(p string) {
		n++
		fmt.Fprintf(&md, "\n## Фото %d\n\n![фото](volume:docs:%s)\n", n, p)
	}
	for range 3 {
		ref("logo.png") // one file named three times
	}
	for i := range distinct {
		ref(fmt.Sprintf("img-%d.png", i))
	}
	out := importMarkdown(t, d, ctx, md.String())

	if asInt(out["image_chunks"]) != 64 || asInt(out["skipped"]) != n-64 {
		t.Errorf("image_chunks=%v skipped=%v, want 64 and %d", out["image_chunks"], out["skipped"], n-64)
	}
	if reads["logo.png"] != 1 {
		t.Errorf("logo.png was read %d times, want once", reads["logo.png"])
	}
	if total != 1+61 {
		t.Errorf("%d files were read, want 62 (the three repeats share one read; nothing past the 64th reference)", total)
	}
	if !lazy {
		t.Error("files were read before the images named earlier were attached: the import held them all at once")
	}
	// The repeats are images too, and the 65th reference stays a link.
	if c, _, asset := importedChunk(t, d, ctx, asStr(out["document_id"]), "Фото 3"); c["type"] != "image" || asset == nil {
		t.Errorf("the third repeat is not an image chunk: %v", c)
	}
	if c, body, _ := importedChunk(t, d, ctx, asStr(out["document_id"]), "Фото 65"); c["type"] == "image" || !strings.HasPrefix(body, "[фото](volume:") {
		t.Errorf("the 65th reference = %v %q, want a plain link", c, body)
	}
}
