# MOBI fixtures (RFC GV P2)

Three books made by Calibre from our own synthetic text, for the MOBI reader's
golden tests (`TestMOBI_CalibreFixturesMatchTheirGoldens`). No text comes from
a real book: `src/book.html` is generated words in three chapters, a section
with a few non-ASCII characters, a list and one 16x16 picture that
`src/make.sh` draws itself. Three footnotes are marked the EPUB 3 way (a
`noteref` link in `<sup>`, a footnote `<aside>` that links back), two plain
cross-references point at chapters, and a fourth chapter's title is a styled
paragraph rather than a heading; `make.sh` builds the contents from the `h2`
headings and that paragraph (`--level1-toc`) and adds no links to it
(`--max-toc-links=0`). What Calibre writes from them is what the footnote
and contents-label rules follow: KF8 keeps `epub_type="noteref"` on a
`kindle:pos` link and `epub_type="footnote"` on the `<aside>`, and its NCX
labels each chapter's file; MOBI 7 keeps neither mark, only
`<sup><a filepos=N>` and a paragraph whose first link points back.

| File | What it is |
|---|---|
| `book-mobi7.mobi` | MOBI 7 only (`--mobi-file-type=old`): Mobipocket HTML, PalmDOC compression, the picture as a JPEG record. |
| `book-kf8.azw3` | KF8 only (AZW3): XHTML skeletons and fragments, a flow table (FDST), the picture as a PNG record. |
| `book-hybrid.mobi` | Both (`--mobi-file-type=both`): a MOBI 7 book, a BOUNDARY record, then a KF8 book that shares the MOBI 7 part's picture. |

Each has the same EXTH metadata (title, two authors, publisher, ISBN,
language, date, subjects, description); Calibre adds a random UUID as the
ASIN (EXTH 113), so a rebuilt fixture changes its `.json` golden. The `.md`
and `.json` files are the goldens; regenerate them with
`go test ./internal/files/extract -run TestMOBI_CalibreFixtures -update`.

They were made (last on 2026-11-19, for the footnotes and contents) in a throwaway container, with Calibre 8.5.0
from Debian trixie (never installed on a developer's host):

```sh
cd internal/files/extract/testdata/mobi
docker run --rm -v "$PWD":/d -w /d/src golang:1.26-trixie sh -c '
  apt-get update &&
  apt-get install -y --no-install-recommends calibre python3 &&
  ./make.sh'
```

Calibre cannot write HUFF/CDIC compression (only Amazon's tools do), so the
HUFF/CDIC books are built in the tests instead (`testHuff` in
`mobi_test.go`), from the format's public description, as are the PalmDOC
texts, the MOBI 7 books in windows-1252 and UTF-8, and the malformed and
encrypted ones.
