#!/bin/sh
# Builds the macOS installer of the better-backup agent:
#
#   packaging/macos/build.sh VERSION [OUTPUT-DIR]
#
# It makes better-backup-agent_VERSION_darwin_universal.pkg, which
# installs, for Intel and Apple silicon Macs alike:
#
#   /usr/local/bin/bb-agent                         the agent and its commands
#   /Library/LaunchDaemons/better-backup-agent.plist  runs it, from startup on
#   /Applications/Better Backup.app                 the app that sets it up
#   /usr/local/bin/bb-agent-uninstall               removes all of this
#
# Run it on a Mac with the Xcode command line tools and Go.
#
# Signing is optional, and off unless these are set:
#
#   MACOS_SIGN_APP   a "Developer ID Application: ..." identity in the
#                    keychain; signs the programs with the hardened runtime
#   MACOS_SIGN_PKG   a "Developer ID Installer: ..." identity; signs the package
#   MACOS_NOTARY_PROFILE   a notarytool keychain profile; has Apple notarize
#                    the signed package, and staples the ticket to it.
#                    Or MACOS_NOTARY_APPLE_ID, MACOS_NOTARY_TEAM_ID and
#                    MACOS_NOTARY_PASSWORD (an app-specific password).
#
# Without them the programs get an ad-hoc signature. Such a package
# installs and works, but macOS warns when it is opened by double-click
# (open it with a right-click, Open, or install it with
# "sudo installer -pkg FILE -target /"), and Full Disk Access has to be
# granted again after each update.
set -eu

version="${1:?usage: build.sh VERSION [OUTPUT-DIR]}"
version="${version#v}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
out="${2:-$repo/dist}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# macOS wants plain numbers for a bundle's version: 1.2.3 of v1.2.3-rc1.
short="$(printf '%s' "$version" | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*$/\1/')"
case "$short" in
[0-9]*) ;;
*) short=0.0.0 ;;
esac

module=github.com/ajthom90/better-backup
ldflags="-s -w -X $module/internal/server.Version=$version -X $module/internal/agent.Version=$version"
export MACOSX_DEPLOYMENT_TARGET=12.0

echo "== building bb-agent $version"
cd "$repo"
for arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$work/bb-agent-$arch" ./cmd/bb-agent
done
lipo -create -output "$work/bb-agent" "$work/bb-agent-arm64" "$work/bb-agent-amd64"

echo "== building the app"
for arch in arm64 amd64; do
    CGO_ENABLED=1 GOOS=darwin GOARCH=$arch \
        CGO_CFLAGS="-mmacosx-version-min=12.0" \
        CGO_LDFLAGS="-framework UniformTypeIdentifiers -mmacosx-version-min=12.0" \
        go build -trimpath -tags desktop,production -ldflags "$ldflags" -o "$work/ui-$arch" ./cmd/bb-agent-ui
done
lipo -create -output "$work/ui" "$work/ui-arm64" "$work/ui-amd64"

echo "== making the icon"
iconset="$work/AppIcon.iconset"
mkdir "$iconset"
for size in 16 32 128 256 512; do
    sips -z $size $size "$repo/packaging/icon.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
    sips -z $((size * 2)) $((size * 2)) "$repo/packaging/icon.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns -o "$work/AppIcon.icns" "$iconset"

echo "== laying out the files"
root="$work/root"
app="$root/Applications/Better Backup.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$root/usr/local/bin" "$root/Library/LaunchDaemons"
cp "$work/ui" "$app/Contents/MacOS/better-backup"
cp "$work/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
sed "s/@SHORT_VERSION@/$short/g" "$here/Info.plist" >"$app/Contents/Info.plist"
printf 'APPL????' >"$app/Contents/PkgInfo"
cp "$work/bb-agent" "$root/usr/local/bin/bb-agent"
cp "$here/uninstall.sh" "$root/usr/local/bin/bb-agent-uninstall"
cp "$here/better-backup-agent.plist" "$root/Library/LaunchDaemons/better-backup-agent.plist"
chmod 755 "$app/Contents/MacOS/better-backup" "$root/usr/local/bin/bb-agent" "$root/usr/local/bin/bb-agent-uninstall"
chmod 644 "$root/Library/LaunchDaemons/better-backup-agent.plist"
plutil -lint "$app/Contents/Info.plist" "$root/Library/LaunchDaemons/better-backup-agent.plist"

echo "== signing"
if [ -n "${MACOS_SIGN_APP:-}" ]; then
    codesign --force --timestamp --options runtime --sign "$MACOS_SIGN_APP" \
        --identifier com.github.ajthom90.better-backup.agent "$root/usr/local/bin/bb-agent"
    codesign --force --timestamp --options runtime --sign "$MACOS_SIGN_APP" "$app"
else
    codesign --force --sign - --identifier com.github.ajthom90.better-backup.agent "$root/usr/local/bin/bb-agent"
    codesign --force --sign - "$app"
fi
codesign --verify --strict "$app" "$root/usr/local/bin/bb-agent"

echo "== building the package"
# Install the app where the package says, even if a copy of it was moved
# elsewhere: without this the installer "relocates" to that copy.
pkgbuild --analyze --root "$root" "$work/components.plist" >/dev/null
/usr/libexec/PlistBuddy -c "Set :0:BundleIsRelocatable false" "$work/components.plist"
pkgbuild --root "$root" --component-plist "$work/components.plist" --scripts "$here/scripts" \
    --identifier com.github.ajthom90.better-backup.agent --version "$short" \
    --install-location / --ownership recommended "$work/agent.pkg"

mkdir "$work/resources"
cp "$here/resources/"*.html "$work/resources/"
cp "$repo/LICENSE" "$work/resources/LICENSE.txt"
sed "s/@SHORT_VERSION@/$short/g" "$here/distribution.xml" >"$work/distribution.xml"
pkg="$out/better-backup-agent_${version}_darwin_universal.pkg"
if [ -n "${MACOS_SIGN_PKG:-}" ]; then
    productbuild --distribution "$work/distribution.xml" --resources "$work/resources" \
        --package-path "$work" --sign "$MACOS_SIGN_PKG" --timestamp "$pkg"
else
    productbuild --distribution "$work/distribution.xml" --resources "$work/resources" \
        --package-path "$work" "$pkg"
fi

if [ -n "${MACOS_SIGN_PKG:-}" ] && [ -n "${MACOS_NOTARY_PROFILE:-}" ]; then
    echo "== notarizing"
    xcrun notarytool submit "$pkg" --keychain-profile "$MACOS_NOTARY_PROFILE" --wait
    xcrun stapler staple "$pkg"
elif [ -n "${MACOS_SIGN_PKG:-}" ] && [ -n "${MACOS_NOTARY_APPLE_ID:-}" ]; then
    echo "== notarizing"
    xcrun notarytool submit "$pkg" --apple-id "$MACOS_NOTARY_APPLE_ID" --team-id "$MACOS_NOTARY_TEAM_ID" \
        --password "$MACOS_NOTARY_PASSWORD" --wait
    xcrun stapler staple "$pkg"
fi

echo "== done: $pkg"
