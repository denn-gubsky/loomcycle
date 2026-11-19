#!/bin/sh
# Builds the chmcmd-made CHM fixtures from the synthetic pages here (RFC GX
# PR B). Run in a throwaway Debian trixie container; see ../README.md.
set -eu
work=$(mktemp -d)
cp -R book notoc "$work/"
mkdir -p "$work/book/images"
# A 16x16 PNG picture, written here so no binary source is committed.
python3 - "$work/book/images/pic.png" <<'PY'
import struct, sys, zlib
w = h = 16
raw = b"".join(b"\x00" + b"".join(bytes((x * 16, y * 16, 128)) for x in range(w)) for y in range(h))
def chunk(t, d):
    return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))
open(sys.argv[1], "wb").write(png)
PY
# The Cyrillic pages are kept as UTF-8 here and written in windows-1251, the
# way a Russian-language help file of the time was.
iconv -f UTF-8 -t WINDOWS-1251 book/cyr.htm >"$work/book/cyr.htm"
iconv -f UTF-8 -t WINDOWS-1251 notoc/b.htm >"$work/notoc/b.htm"
iconv -f UTF-8 -t WINDOWS-1251 notoc/notoc.hhp >"$work/notoc/notoc.hhp"
mkdir "$work/big"
python3 big/gen.py "$work/big"
(cd "$work/book" && chmcmd book.hhp)
(cd "$work/big" && chmcmd big.hhp)
(cd "$work/notoc" && chmcmd notoc.hhp)
cp "$work/book/book.chm" "$work/notoc/notoc.chm" "$work/big/big.chm" ..
rm -rf "$work"
