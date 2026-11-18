# DjVu fixtures (RFC GV P3)

Synthetic DjVu documents for the DjVu extractor's tests. No page comes from a
real book: each page is the text "Page N" drawn by netpbm's `pbmtext`, and
every text layer, outline and metadata value is written by hand in `src/`.

| File | What it is |
|---|---|
| `single.djvu` | One page (`FORM:DJVU`) with a text layer (`src/t1.txt`). |
| `book.djvu` | Three bundled pages (`FORM:DJVM`): text on pages 1 (`src/t1.txt`) and 3 (`src/t3.txt`, Cyrillic), none on page 2; an outline (`src/outline.txt`, page links `#1` and `#3`) and metadata (`src/meta.txt`). |
| `scan.djvu` | The same three pages bundled with no text layer, outline or metadata: a scan. |
| `indirect.djvu` | `book.djvu` converted to an indirect document; only its index file, which names the pages as separate files. |

They were made once, in a container, with DjVuLibre 3.5.28 from Debian trixie
(`djvulibre-bin`, never installed on a developer's host) and netpbm:

```sh
cd internal/files/extract/testdata/djvu
docker run --rm -v "$PWD":/d -w /d/src golang:1.26-trixie sh -c '
  apt-get update &&
  apt-get install -y --no-install-recommends djvulibre-bin netpbm &&
  ./make.sh'
```

`src/make.sh` holds every command: `pbmtext | pnmpad` for each page image,
`cjb2 -dpi 100` to encode it, `djvm -c` to bundle the pages, `djvused -e
'select N; set-txt …; select; set-outline …; set-meta …' -s` for the text
layers, outline and metadata, and `djvmcvt -i` for the indirect index.

The tests that run DjVuLibre itself skip where `djvutxt` is not on `PATH`,
unless `LOOMCYCLE_REQUIRE_DJVULIBRE` is set (CI sets it).
