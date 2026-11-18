#!/bin/sh
# Builds the DjVu fixtures from synthetic pages. Run inside Debian trixie
# with djvulibre-bin and netpbm installed (see ../README.md), from this
# directory; writes the fixtures one level up.
set -eu
work=$(mktemp -d)
for i in 1 2 3; do
	pbmtext "Page $i" | pnmpad -white -left 20 -right 20 -top 20 -bottom 20 > "$work/p$i.pbm"
	cjb2 -dpi 100 "$work/p$i.pbm" "$work/p$i.djvu"
done
# single.djvu: one page with a text layer.
cp "$work/p1.djvu" ../single.djvu
djvused ../single.djvu -e 'select 1; set-txt t1.txt' -s
# scan.djvu: three bundled pages, no text layer, no outline, no metadata.
djvm -c ../scan.djvu "$work/p1.djvu" "$work/p2.djvu" "$work/p3.djvu"
# book.djvu: three bundled pages; text on pages 1 and 3, none on page 2;
# an outline and metadata.
djvm -c ../book.djvu "$work/p1.djvu" "$work/p2.djvu" "$work/p3.djvu"
djvused ../book.djvu -e 'select 1; set-txt t1.txt; select 3; set-txt t3.txt; select; set-outline outline.txt; set-meta meta.txt' -s
# indirect.djvu: the index of book.djvu converted to an indirect document
# (its pages would be separate files beside it; only the index is kept).
mkdir "$work/ind"
djvmcvt -i ../book.djvu "$work/ind" index.djvu
cp "$work/ind/index.djvu" ../indirect.djvu
rm -rf "$work"
