#!/usr/bin/env bash
# Builds a release into dist/: the agent for linux/amd64 and linux/arm64,
# checksums.txt, and install.sh with the version and checksums written in.
#
#   scripts/build-release.sh 0.1.0
set -euo pipefail

version=${1:?usage: scripts/build-release.sh VERSION}
version=${version#v}
cd "$(dirname "$0")/.."
rm -rf dist
mkdir dist

for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" \
		-o "dist/boxrunner-linux-$arch" ./cmd/boxrunner
done
(cd dist && sha256sum boxrunner-linux-* >checksums.txt)

awk -v version="$version" -v sums=dist/checksums.txt '
	BEGIN { while ((getline line <sums) > 0) checksums = checksums (checksums ? "\n" : "") line }
	$0 == "VERSION=\"__VERSION__\"" { print "VERSION=\"" version "\""; next }
	$0 == "CHECKSUMS=\"__CHECKSUMS__\"" { print "CHECKSUMS=\"" checksums "\""; next }
	{ print }
' install.sh >dist/install.sh
grep -q __ dist/install.sh && { echo "install.sh still has a placeholder" >&2; exit 1; }
ls -l dist
