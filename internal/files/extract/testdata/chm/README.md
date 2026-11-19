# CHM fixtures (RFC GX PR B)

Three compiled HTML Help files made by Free Pascal's `chmcmd` from our own
synthetic pages, for the CHM reader's tests. No text comes from a real help
file or book: the pages under `src/` were written for these tests, `src/big/gen.py`
generates words, and `src/make.sh` draws the one 16x16 picture itself.

| File | What it is | Tests |
|---|---|---|
| `book.chm` | A table of contents with a nested entry, an entry with no page of its own, a repeated entry and a page outside the contents (`extra.htm`, not read); one picture used twice and a remote one (a link); a page in windows-1251 named by its `<meta http-equiv>`. Language 0x409. | `TestCHM_FixturesMatchTheirGoldens`, the worker agreement and sniff tests |
| `notoc.chm` | No contents file: the default topic, then the pages in directory order. Language 0x419 and a windows-1251 title and page with no `<meta>` charset, read in the language's code page. | `TestCHM_FixturesMatchTheirGoldens` |
| `big.chm` | Twelve chapters of generated words, 128 KiB: four LZX frames over two reset intervals (64 KiB each, a 64 KiB window). | `TestCHM_ChmcmdFileOfSeveralFramesReadsBack` (checked against `gen.py`'s words, made again in the test), `TestCHM_LaterTopicDoesNotDecompressFromStart` |

`chmcmd` writes the pages LZX-compressed in section 1, with a reset table
(one entry per 32 KiB frame) and ControlData version 2. Its blocks are
verbatim; it codes the last, short frame as a full one, so the section ends
with a match into padding, which the reader allows in the last frame only.
Aligned-offset and uncompressed blocks, the E8 translation, and the malformed
streams the bounds tests need are built in the tests instead (`lzxEnc` in
`lzx_test.go`, `chmSpec` in `chm_test.go`), from the same description.

The `.md` and `.json` files are the goldens; regenerate them with
`go test ./internal/files/extract -run TestCHM_FixturesMatchTheirGoldens -update`.

They were made once (2026-11-19) in a throwaway container, with `chmcmd`
from Debian trixie's `fp-utils` (Free Pascal 3.2.2), never installed on a
developer's host:

```sh
cd internal/files/extract/testdata/chm
docker run --rm -v "$PWD":/d -w /d/src golang:1.26-trixie sh -c '
  apt-get update &&
  apt-get install -y --no-install-recommends fp-utils python3 &&
  ./make.sh'
```

The ITSF header carries a timestamp, so a rebuilt file may differ in a few
bytes; its text and goldens should not.
