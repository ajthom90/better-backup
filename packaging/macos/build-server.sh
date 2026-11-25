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
# Run it on a Mac with the Xcode command line tools and Go. Signing and
# notarization work as in build.sh and read the same settings
# (MACOS_SIGN_APP, MACOS_SIGN_PKG, MACOS_SIGN_KEYCHAIN, MACOS_NOTARY_*).
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
if [ -n "${MACOS_SIGN_APP:-}" ]; then
    # The hardened runtime and a timestamp from Apple: notarization
    # wants both.
    set -- --force --timestamp --options runtime --sign "$MACOS_SIGN_APP"
    if [ -n "${MACOS_SIGN_KEYCHAIN:-}" ]; then
        set -- "$@" --keychain "$MACOS_SIGN_KEYCHAIN"
    fi
    for prog in bb-server bb; do
        codesign "$@" --identifier "com.github.ajthom90.better-backup.$prog" "$root/usr/local/bin/$prog"
        codesign --display --verbose=2 "$root/usr/local/bin/$prog" 2>&1 | grep -E "^(Authority|TeamIdentifier|Timestamp|CodeDirectory)" || true
        codesign --display --verbose=2 "$root/usr/local/bin/$prog" 2>&1 | grep -q "flags=.*runtime" || {
            echo "$prog is not signed with the hardened runtime" >&2
            exit 1
        }
    done
else
    for prog in bb-server bb; do
        codesign --force --sign - --identifier "com.github.ajthom90.better-backup.$prog" "$root/usr/local/bin/$prog"
    done
fi
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
    set -- --sign "$MACOS_SIGN_PKG" --timestamp
    if [ -n "${MACOS_SIGN_KEYCHAIN:-}" ]; then
        set -- "$@" --keychain "$MACOS_SIGN_KEYCHAIN"
    fi
    productbuild --distribution "$work/distribution.xml" --resources "$work/resources" \
        --package-path "$work" "$@" "$pkg"
    pkgutil --check-signature "$pkg"
else
    productbuild --distribution "$work/distribution.xml" --resources "$work/resources" \
        --package-path "$work" "$pkg"
fi

# Notarization, as in build.sh: which sign-in notarytool uses, as its
# arguments.
set --
if [ -n "${MACOS_NOTARY_KEY:-}" ]; then
    set -- --key "$MACOS_NOTARY_KEY" --key-id "$MACOS_NOTARY_KEY_ID" --issuer "$MACOS_NOTARY_ISSUER"
elif [ -n "${MACOS_NOTARY_PROFILE:-}" ]; then
    set -- --keychain-profile "$MACOS_NOTARY_PROFILE"
elif [ -n "${MACOS_NOTARY_APPLE_ID:-}" ]; then
    set -- --apple-id "$MACOS_NOTARY_APPLE_ID" --team-id "$MACOS_NOTARY_TEAM_ID" --password "$MACOS_NOTARY_PASSWORD"
fi
if [ $# -gt 0 ] && { [ -z "${MACOS_SIGN_APP:-}" ] || [ -z "${MACOS_SIGN_PKG:-}" ]; }; then
    echo "not notarizing: that needs both MACOS_SIGN_APP and MACOS_SIGN_PKG" >&2
    set --
fi
if [ $# -gt 0 ]; then
    echo "== notarizing (Apple usually answers within a few minutes)"
    # notarytool can end well although Apple said no, so read the answer.
    xcrun notarytool submit "$pkg" "$@" --wait --timeout 45m --output-format json >"$work/notary.json" || true
    cat "$work/notary.json"
    echo
    id="$(plutil -extract id raw -o - "$work/notary.json" 2>/dev/null || true)"
    status="$(plutil -extract status raw -o - "$work/notary.json" 2>/dev/null || true)"
    if [ "$status" != Accepted ]; then
        echo "Apple did not notarize the package (status: ${status:-none})." >&2
        if [ -n "$id" ]; then
            xcrun notarytool log "$id" "$@" >&2 || true
        fi
        exit 1
    fi
    xcrun stapler staple "$pkg"
    xcrun stapler validate "$pkg"
    # What Gatekeeper will say on the Mac that opens it.
    spctl --assess --type install --verbose=2 "$pkg"
fi

echo "== done: $pkg"
