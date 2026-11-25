#!/bin/sh
# Builds the macOS installer of the better-backup server:
#
#   packaging/macos/build-server.sh VERSION [OUTPUT-DIR] [AGENT-PKG]
#
# It makes better-backup-server_VERSION_darwin_universal.pkg, which
# installs, for Intel and Apple silicon Macs alike:
#
#   /usr/local/bin/bb-server                           the server
#   /usr/local/bin/bb                                  its command line
#   /Library/LaunchDaemons/better-backup-server.plist  runs it, from startup on
#   /usr/local/bin/installers/                         AGENT-PKG, when given: the
#                                                      agent installer the web UI hands out
#   /usr/local/bin/bb-server-uninstall                 removes all of this
#
# The server keeps its data in /Library/Application Support/better-backup.
# Run it on a Mac with the Xcode command line tools and Go. Signing works
# as in build.sh: MACOS_SIGN_APP, MACOS_SIGN_PKG and MACOS_NOTARY_*.
set -eu

version="${1:?usage: build-server.sh VERSION [OUTPUT-DIR] [AGENT-PKG]}"
version="${version#v}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
out="${2:-$repo/dist}"
agentpkg="${3:-}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

short="$(printf '%s' "$version" | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*$/\1/')"
case "$short" in
[0-9]*) ;;
*) short=0.0.0 ;;
esac

module=github.com/ajthom90/better-backup
ldflags="-s -w -X $module/internal/server.Version=$version -X $module/internal/agent.Version=$version"
export MACOSX_DEPLOYMENT_TARGET=12.0

echo "== building bb-server and bb $version"
cd "$repo"
for prog in bb-server bb; do
    for arch in arm64 amd64; do
        CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$work/$prog-$arch" ./cmd/$prog
    done
    lipo -create -output "$work/$prog" "$work/$prog-arm64" "$work/$prog-amd64"
done

echo "== laying out the files"
root="$work/root"
mkdir -p "$root/usr/local/bin" "$root/Library/LaunchDaemons"
cp "$work/bb-server" "$work/bb" "$root/usr/local/bin/"
cp "$here/server/uninstall.sh" "$root/usr/local/bin/bb-server-uninstall"
cp "$here/server/better-backup-server.plist" "$root/Library/LaunchDaemons/better-backup-server.plist"
chmod 755 "$root/usr/local/bin/bb-server" "$root/usr/local/bin/bb" "$root/usr/local/bin/bb-server-uninstall"
chmod 644 "$root/Library/LaunchDaemons/better-backup-server.plist"
plutil -lint "$root/Library/LaunchDaemons/better-backup-server.plist"
if [ -n "$agentpkg" ]; then
    mkdir -p "$root/usr/local/bin/installers"
    cp "$agentpkg" "$root/usr/local/bin/installers/"
fi

echo "== signing"
for prog in bb-server bb; do
    if [ -n "${MACOS_SIGN_APP:-}" ]; then
        codesign --force --timestamp --options runtime --sign "$MACOS_SIGN_APP" \
            --identifier "com.github.ajthom90.better-backup.$prog" "$root/usr/local/bin/$prog"
    else
        codesign --force --sign - --identifier "com.github.ajthom90.better-backup.$prog" "$root/usr/local/bin/$prog"
    fi
done
codesign --verify --strict "$root/usr/local/bin/bb-server" "$root/usr/local/bin/bb"

echo "== building the package"
pkgbuild --root "$root" --scripts "$here/server/scripts" \
    --identifier com.github.ajthom90.better-backup.server --version "$short" \
    --install-location / --ownership recommended "$work/server.pkg"

mkdir "$work/resources"
cp "$here/server/resources/"*.html "$work/resources/"
cp "$repo/LICENSE" "$work/resources/LICENSE.txt"
sed "s/@SHORT_VERSION@/$short/g" "$here/server/distribution.xml" >"$work/distribution.xml"
pkg="$out/better-backup-server_${version}_darwin_universal.pkg"
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
