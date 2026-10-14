#!/bin/sh
# Rebuilds fonts/*.woff2 from the OFL sources in google/fonts at a pinned commit.
# Needs curl and Python with fonttools and brotli (pip3 install fonttools brotli).
set -eu
cd "$(dirname "$0")"
commit=e9678a5385322c63730122ba464765baad83cfd8
raw="https://raw.githubusercontent.com/google/fonts/$commit/ofl"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Google's latin subset, plus the Czech letters in the author's name and the
# symbols the job feed and the docs use, so none of them falls back mid-word.
unicodes="U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2190-2193,U+2212,U+2215,U+FEFF,U+FFFD,U+010C-010F,U+011A-011B,U+0147-0148,U+0158-0161,U+0164-0165,U+016E-016F,U+017D-017E,U+25CF,U+2713,U+2717"

make() { # source-path output axes...
  src=$1 out=$2
  shift 2
  curl -fsSL "$raw/$src" -o "$tmp/full.ttf"
  python3 -m fontTools.varLib.instancer "$tmp/full.ttf" "$@" -o "$tmp/limited.ttf" -q
  python3 -m fontTools.subset "$tmp/limited.ttf" --unicodes="$unicodes" --layout-features='*' \
    --flavor=woff2 --output-file="fonts/$out"
}

mkdir -p fonts
make "bricolagegrotesque/BricolageGrotesque%5Bopsz,wdth,wght%5D.ttf" bricolage-grotesque.woff2 wght=500:800 wdth=100
make "geist/Geist%5Bwght%5D.ttf" geist.woff2 wght=400:600
make "geistmono/GeistMono%5Bwght%5D.ttf" geist-mono.woff2 wght=400:500
curl -fsSL "$raw/bricolagegrotesque/OFL.txt" -o fonts/OFL-bricolage-grotesque.txt
curl -fsSL "$raw/geist/OFL.txt" -o fonts/OFL-geist.txt
ls -l fonts
