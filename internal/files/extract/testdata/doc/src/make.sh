#!/bin/sh
# Converts the synthetic flat-ODT sources here into Word 97-2003 .doc files
# in the directory above. Run inside a throwaway container (README.md).
set -eu
for f in report cyrillic plain; do
  soffice --headless --convert-to 'doc:MS Word 97' --outdir /tmp/out "$f.fodt"
  cp "/tmp/out/$f.doc" "../$f.doc"
done
ls -l ..
