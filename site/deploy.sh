#!/usr/bin/env bash
# Builds the site, adds the installer of a release (the latest by default) at
# /install.sh and at its versioned path, checks everything, then deploys.
#
#   site/deploy.sh [vX.Y.Z]
set -euo pipefail
cd "$(dirname "$0")"

tag=${1:-$(gh release view -R dominikzabcik/boxrunner --json tagName -q .tagName)}
# check builds dist/ itself, so the installer is added after it.
bun run check
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
gh release download "$tag" -R dominikzabcik/boxrunner -p install.sh -D "$tmp"
grep -q "^VERSION=\"${tag#v}\"$" "$tmp/install.sh" || { echo "install.sh of $tag does not name version ${tag#v}" >&2; exit 1; }
cp "$tmp/install.sh" dist/install.sh
mkdir -p "dist/$tag"
cp "$tmp/install.sh" "dist/$tag/install.sh"
bunx wrangler deploy
