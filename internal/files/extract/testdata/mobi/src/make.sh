#!/bin/sh
# Builds the Calibre-made MOBI fixtures from book.html (RFC GV P2). Run in a
# throwaway Debian trixie container; see ../README.md.
set -eu
# A 16x16 PNG picture, written here so no binary source is committed.
python3 - <<'PY'
import struct, zlib
w = h = 16
raw = b"".join(b"\x00" + b"".join(bytes((x * 16, y * 16, 128)) for x in range(w)) for y in range(h))
def chunk(t, d):
    return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))
open("pic.png", "wb").write(png)
PY
conv() {
  out=$1
  shift
  ebook-convert book.html "../$out" "$@" \
    --title="The Lantern Ledger" --authors="Ada Quill & Ben Rowe" \
    --publisher="Loom Test Press" --isbn=9780000000002 --language=en \
    --pubdate=2021-05-04 --tags="Fiction,Test Fixtures" \
    --comments="A synthetic book for the MOBI reader's tests."
}
conv book-mobi7.mobi --mobi-file-type=old
conv book-kf8.azw3
conv book-hybrid.mobi --mobi-file-type=both
rm -f pic.png
