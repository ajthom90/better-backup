#!/bin/sh
# Checks packaging/macos/ci-signing.sh on a Mac runner, with certificates
# made up here in place of Apple's: that it imports the two .p12 files,
# finds the names to sign with, and hands on the notarization key. The
# real thing (Apple's certificates, notarization) only runs where the
# repository's secrets are, on main and on releases.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'security delete-keychain "$work/signing.keychain-db" 2>/dev/null || true; security list-keychains -d user -s $saved; rm -rf "$work"' EXIT
saved="$(security list-keychains -d user | sed -e 's/^ *"//' -e 's/"$//' | tr '\n' ' ')"
fail() {
    echo "FAILED: $*" >&2
    exit 1
}
# The system's openssl: it writes .p12 files the way Keychain Access does.
ssl=/usr/bin/openssl

p12() { # FILE COMMON-NAME
    $ssl req -x509 -newkey rsa:2048 -nodes -keyout "$work/key.pem" -out "$work/cert.pem" -days 2 \
        -subj "/CN=$2/OU=CITEST1234/O=CI Test" 2>/dev/null
    $ssl pkcs12 -export -inkey "$work/key.pem" -in "$work/cert.pem" -name "$2" -out "$1" -passout pass:secret
}
p12 "$work/app.p12" "Developer ID Application: CI Test (CITEST1234)"
p12 "$work/installer.p12" "Developer ID Installer: CI Test (CITEST1234)"
$ssl ecparam -name prime256v1 -genkey -noout | $ssl pkcs8 -topk8 -nocrypt -out "$work/key.p8"

run() { # sets the secrets, runs the script, leaves its settings in $work/env
    : >"$work/env"
    RUNNER_TEMP="$work" GITHUB_ENV="$work/env" "$here/../macos/ci-signing.sh"
}

echo "== without secrets"
(
    unset MACOS_APP_CERT_P12 MACOS_INSTALLER_CERT_P12 MACOS_CERT_PASSWORD APPLE_API_KEY_P8 APPLE_API_KEY_ID APPLE_API_ISSUER_ID APPLE_TEAM_ID
    run
)
[ ! -s "$work/env" ] || fail "settings without secrets: $(cat "$work/env")"

echo "== with secrets"
(
    MACOS_APP_CERT_P12="$(base64 -i "$work/app.p12")"
    MACOS_INSTALLER_CERT_P12="$(base64 -i "$work/installer.p12")"
    export MACOS_APP_CERT_P12 MACOS_INSTALLER_CERT_P12
    export MACOS_CERT_PASSWORD=secret APPLE_TEAM_ID=CITEST1234
    APPLE_API_KEY_P8="$(base64 -i "$work/key.p8")"
    export APPLE_API_KEY_P8 APPLE_API_KEY_ID=ABC123DEFG APPLE_API_ISSUER_ID=00000000-0000-0000-0000-000000000000
    run
)
cat "$work/env"
grep -qx "MACOS_SIGN_APP=Developer ID Application: CI Test (CITEST1234)" "$work/env" || fail "no application identity"
grep -qx "MACOS_SIGN_PKG=Developer ID Installer: CI Test (CITEST1234)" "$work/env" || fail "no installer identity"
grep -qx "MACOS_NOTARY_KEY_ID=ABC123DEFG" "$work/env" || fail "no notarization key"
key="$(sed -n 's/^MACOS_NOTARY_KEY=//p' "$work/env")"
cmp "$key" "$work/key.p8" || fail "the notarization key was not written whole"

echo "== signing with the imported key"
cp /bin/echo "$work/program"
codesign --force --options runtime --sign "Developer ID Application: CI Test (CITEST1234)" \
    --keychain "$work/signing.keychain-db" "$work/program" || fail "codesign cannot use the imported key"
codesign --display --verbose=2 "$work/program" 2>&1 | grep -q "flags=.*runtime" || fail "no hardened runtime"

echo "== ok"
