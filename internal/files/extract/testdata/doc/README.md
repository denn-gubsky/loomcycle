# DOC fixtures (RFC GX)

Three Word 97–2003 documents made by LibreOffice from our own synthetic
flat-ODT sources in `src/`, for the DOC reader's golden tests
(`TestDOC_LibreOfficeFixturesMatchTheirGoldens`). No text comes from a real
document.

| File | What it holds |
|---|---|
| `report.doc` | Heading 1–3 paragraphs, a three-column table (a cell with a `\|` and two paragraphs, an empty cell), a footnote, an endnote, a PAGE field, a hand line break, a line starting `#`, and document properties (title, subject, author, keywords, comments, creation date). |
| `cyrillic.doc` | Ukrainian text in heading and body paragraphs, a footnote, and Cyrillic properties. |
| `plain.doc` | No heading styles: a large bold line and a bold line among body paragraphs, for `infer_headings`. |

LibreOffice writes every piece as UTF-16 and keeps its property sets in the
mini stream; the 8-bit (compressed) pieces, Word 6/95 documents, and the
damaged, encrypted and spreadsheet-shaped compound files are built in the
tests instead (`buildCFB` in `cfb_test.go`, `docTestSpec` and `word6Doc` in
`doc_test.go`), from [MS-CFB] and [MS-DOC].

The `.md` and `.json` files are the goldens; regenerate them with
`go test ./internal/files/extract -run TestDOC_LibreOfficeFixtures -update`.

They were made once (2026-11-19) in a throwaway container, with LibreOffice
25.2.3 from Debian trixie (never installed on a developer's host):

```sh
cd internal/files/extract/testdata/doc
docker run --rm -v "$PWD":/d -w /d/src golang:1.26-trixie sh -c '
  apt-get update &&
  apt-get install -y --no-install-recommends libreoffice-writer-nogui &&
  ./make.sh'
```
