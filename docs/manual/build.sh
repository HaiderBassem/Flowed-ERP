#!/usr/bin/env bash
# Renders the operator manual to PDF.
#
# Chrome rather than a PDF library: the manual is RTL Arabic with tables, and
# Chrome is the only renderer on hand that shapes Arabic correctly and embeds
# the subset fonts, so the file reads the same on a machine that has none of
# them installed.
set -euo pipefail
cd "$(dirname "$0")"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
"$CHROME" --headless --disable-gpu --no-pdf-header-footer \
  --print-to-pdf="${1:-Flowed-Manual.pdf}" manual.html
