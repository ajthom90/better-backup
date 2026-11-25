#!/bin/sh
# Prepares a GitHub Actions Mac runner to sign and notarize what
# packaging/macos/build.sh builds. A workflow step runs it with these
# repository secrets in its environment:
#
#   MACOS_APP_CERT_P12        base64 of a .p12 with the "Developer ID
#                             Application" certificate and its key
#   MACOS_INSTALLER_CERT_P12  base64 of a .p12 with the "Developer ID
#                             Installer" certificate and its key
#   MACOS_CERT_PASSWORD       the password of both .p12 files
#   APPLE_API_KEY_P8          base64 of an App Store Connect API key (.p8)
#   APPLE_API_KEY_ID          that key's ID
#   APPLE_API_ISSUER_ID       the issuer ID shown with the key
#   APPLE_TEAM_ID             the team the certificates belong to
#
# It puts the certificates in a keychain of their own and, through
# $GITHUB_ENV, hands build.sh the settings it reads (MACOS_SIGN_APP and
# so on) in the steps that follow.
#
# Without the secrets (a fork, a pull request from one) it does nothing,
# and build.sh makes an unsigned installer as before. With the
# certificates but no API key the installer is signed but not notarized.
set -eu

warn() { echo "::warning::$*"; }
die() {
    echo "::error::$*"
    exit 1
}

if [ -z "${MACOS_APP_CERT_P12:-}" ] && [ -z "${MACOS_INSTALLER_CERT_P12:-}" ]; then
    echo "No signing certificates: the installer will be unsigned."
    exit 0
fi
[ -n "${MACOS_APP_CERT_P12:-}" ] || die "MACOS_INSTALLER_CERT_P12 is set but MACOS_APP_CERT_P12 is not"
[ -n "${MACOS_INSTALLER_CERT_P12:-}" ] || die "MACOS_APP_CERT_P12 is set but MACOS_INSTALLER_CERT_P12 is not"
[ -n "${MACOS_CERT_PASSWORD:-}" ] || die "MACOS_CERT_PASSWORD is not set"

tmp="${RUNNER_TEMP:?run this in GitHub Actions}"
envfile="${GITHUB_ENV:?run this in GitHub Actions}"
keychain="$tmp/signing.keychain-db"
password="$(openssl rand -hex 16)"

security create-keychain -p "$password" "$keychain"
security set-keychain-settings -lut 21600 "$keychain"
security unlock-keychain -p "$password" "$keychain"

import() { # NAME BASE64
    printf '%s' "$2" | base64 --decode >"$tmp/$1.p12" || die "$1 is not base64"
    security import "$tmp/$1.p12" -P "$MACOS_CERT_PASSWORD" -f pkcs12 -k "$keychain" \
        -T /usr/bin/codesign -T /usr/bin/productbuild -T /usr/bin/productsign -T /usr/bin/pkgbuild ||
        die "cannot import $1: is MACOS_CERT_PASSWORD its password?"
    rm -f "$tmp/$1.p12"
}
import MACOS_APP_CERT_P12 "$MACOS_APP_CERT_P12"
import MACOS_INSTALLER_CERT_P12 "$MACOS_INSTALLER_CERT_P12"

# Let Apple's tools use the keys without asking, and find the keychain.
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$password" "$keychain" >/dev/null
# shellcheck disable=SC2046 # one word per keychain is what is wanted
security list-keychains -d user -s "$keychain" $(security list-keychains -d user | sed -e 's/^ *"//' -e 's/"$//')

# The full names of the two certificates, such as
# "Developer ID Application: Jane Doe (ABCDE12345)".
identity() { # KIND
    security find-certificate -a -c "$1: " "$keychain" |
        sed -n 's/^ *"labl"<blob>="\(.*\)"$/\1/p' |
        { if [ -n "${APPLE_TEAM_ID:-}" ]; then grep -F "($APPLE_TEAM_ID)"; else cat; fi; } |
        head -n 1
}
app="$(identity "Developer ID Application")"
pkg="$(identity "Developer ID Installer")"
[ -n "$app" ] || die "MACOS_APP_CERT_P12 holds no \"Developer ID Application\" certificate${APPLE_TEAM_ID:+ of team $APPLE_TEAM_ID}"
[ -n "$pkg" ] || die "MACOS_INSTALLER_CERT_P12 holds no \"Developer ID Installer\" certificate${APPLE_TEAM_ID:+ of team $APPLE_TEAM_ID}"
# A certificate without its private key imports fine and signs nothing.
security find-identity "$keychain" | grep -qF "\"$app\"" ||
    die "MACOS_APP_CERT_P12 has the certificate but not its private key: export it from Keychain Access under My Certificates"
security find-identity "$keychain" | grep -qF "\"$pkg\"" ||
    die "MACOS_INSTALLER_CERT_P12 has the certificate but not its private key: export it from Keychain Access under My Certificates"
echo "Signing as: $app"
echo "            $pkg"
{
    echo "MACOS_SIGN_APP=$app"
    echo "MACOS_SIGN_PKG=$pkg"
    echo "MACOS_SIGN_KEYCHAIN=$keychain"
} >>"$envfile"

if [ -z "${APPLE_API_KEY_P8:-}" ] || [ -z "${APPLE_API_KEY_ID:-}" ] || [ -z "${APPLE_API_ISSUER_ID:-}" ]; then
    warn "APPLE_API_KEY_P8, APPLE_API_KEY_ID or APPLE_API_ISSUER_ID is not set: the installer is signed but not notarized."
    exit 0
fi
key="$tmp/AuthKey_$APPLE_API_KEY_ID.p8"
(umask 077 && printf '%s' "$APPLE_API_KEY_P8" | base64 --decode >"$key") || die "APPLE_API_KEY_P8 is not base64"
grep -q "BEGIN PRIVATE KEY" "$key" || die "APPLE_API_KEY_P8 is not the base64 of a .p8 key file"
{
    echo "MACOS_NOTARY_KEY=$key"
    echo "MACOS_NOTARY_KEY_ID=$APPLE_API_KEY_ID"
    echo "MACOS_NOTARY_ISSUER=$APPLE_API_ISSUER_ID"
} >>"$envfile"
echo "Notarizing with API key $APPLE_API_KEY_ID."
