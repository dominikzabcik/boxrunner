#!/usr/bin/env bash
# Builds boxrunner.app, the menu bar app, for Apple silicon and Intel, signed
# ad hoc, into build/. Needs Xcode's command line tools.
#
#   app/macos/build-app.sh [VERSION]
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
version=${1:-0.0.0}
app=$here/build/boxrunner.app

swift build --package-path "$here" -c release --arch arm64 --arch x86_64
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$here/.build/apple/Products/Release/BoxrunnerBar" "$app/Contents/MacOS/boxrunner"
cat >"$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key><string>boxrunner</string>
	<key>CFBundleIdentifier</key><string>me.zabcik.boxrunner.menubar</string>
	<key>CFBundleName</key><string>boxrunner</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$version</string>
	<key>CFBundleVersion</key><string>$version</string>
	<key>LSMinimumSystemVersion</key><string>14.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHumanReadableCopyright</key><string>MIT License</string>
</dict>
</plist>
PLIST
codesign --force --sign - --options runtime "$app"
echo "$app"
