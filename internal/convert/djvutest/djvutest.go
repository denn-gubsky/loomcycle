// Package djvutest installs stand-ins for DjVuLibre's djvutxt, djvused and
// ddjvu, so the DjVu helper wiring (RFC GV P3, P4) is tested on a host
// without DjVuLibre. The stand-ins answer in the programs' own shapes:
// djvutxt --detail=page one `(page …)` per page of its --page list, djvused
// one fixed outline or metadata answer per script, ddjvu a TIFF written to
// its last argument (PageImage, or the page's own from SetPage). Each logs
// its arguments, one call per line, so a test sees exactly what the runtime
// asked.
package djvutest

import (
	"bytes"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/image/tiff"
)

// PageWidth and PageHeight are the size of the page ddjvu's stand-in draws
// unless SetPage gives a page its own.
const (
	PageWidth  = 120
	PageHeight = 160
)

// Fake is the installed stand-ins.
type Fake struct {
	Dir string
	log string
}

// Install writes the stand-ins into a fresh directory, puts it first on
// PATH for the test, and returns it. Unix only: they are sh scripts.
func Install(t testing.TB) *Fake {
	t.Helper()
	dir := t.TempDir()
	f := &Fake{Dir: dir, log: filepath.Join(dir, "calls.log")}
	// The helper runner passes the child only PATH, so the log's path is
	// written into each script.
	txt := `#!/bin/sh
printf 'djvutxt %s\n' "$*" >> '` + f.log + `'
spec=
for a; do case $a in --page=*) spec=${a#--page=};; esac; done
IFS=,
for part in $spec; do
	lo=${part%-*}; hi=${part#*-}; n=$lo
	while [ "$n" -le "$hi" ]; do
		printf '(page 0 0 100 100 "Text of page %s, read by the stand-in for djvutxt, long enough to count as a text layer.\\n")\n' "$n"
		n=$((n+1))
	done
done
`
	sed := `#!/bin/sh
printf 'djvused %s\n' "$*" >> '` + f.log + `'
case $2 in
print-meta) printf 'title\t"A Stand-in Title"\nauthor\t"Ada Example"\n' ;;
print-outline) printf '(bookmarks ("Chapter From The Stand-in" "#1"))\n' ;;
*) exit 10 ;;
esac
`
	ddjvu := `#!/bin/sh
printf 'ddjvu %s\n' "$*" >> '` + f.log + `'
page=; out=
for a; do case $a in -page=*) page=${a#-page=};; esac; out=$a; done
src='` + dir + `'/page-$page.tiff
[ -f "$src" ] || src='` + dir + `'/page.tiff
cp "$src" "$out"
`
	if err := os.WriteFile(filepath.Join(dir, "page.tiff"), PageImage(t, PageWidth, PageHeight), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"djvutxt": txt, "djvused": sed, "ddjvu": ddjvu} {
		// #nosec G306 -- a test helper's stand-in program under the test's temp
		// dir: it must be executable, and only its owner may run it.
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// PageImage is a w×h grey TIFF, as ddjvu -format=tiff writes a page.
func PageImage(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetGray(x, y, color.Gray{Y: uint8(255 - (x+y)%64)})
		}
	}
	var b bytes.Buffer
	if err := tiff.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// SetPage makes ddjvu's stand-in write data for page n.
func (f *Fake) SetPage(t testing.TB, n int, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.Dir, "page-"+strconv.Itoa(n)+".tiff"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Calls are the stand-ins' invocations so far, one line each: the program's
// name and its arguments.
func (f *Fake) Calls(t testing.TB) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}
