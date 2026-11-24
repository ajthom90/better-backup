#!/bin/sh
# Installs the better-backup agent on Linux or macOS, and enrolls the
# machine when given an enrollment code:
#
#   curl -fsSL https://raw.githubusercontent.com/ajthom90/better-backup/main/packaging/install.sh | sudo sh -s -- CODE
#
# Get the code from "Add an agent" in the server's web UI. Without a
# code it only installs; enroll later with: sudo bb-agent enroll
#
# It installs the newest release: the .deb or .rpm package where the
# system uses those, the .pkg on macOS, and otherwise the plain program
# as a service. Settings, as environment variables:
#
#   BB_VERSION      the release to install, such as v0.3.0
#   BB_AGENT_NAME   the name the server lists this machine under
#   BB_PACKAGE      a package file already on this machine, to install
#                   instead of downloading one
set -eu

repo=ajthom90/better-backup
code="${1:-${BB_ENROLL_CODE:-}}"

say() { printf '%s\n' "$*"; }
die() {
    say "error: $*" >&2
    exit 1
}

[ "$(id -u)" = 0 ] || die "run this as root, for example with sudo"
case "$code" in
"" | bb1.*) ;;
*) die "that is not an enrollment code; copy the whole code from \"Add an agent\" in the server's web UI" ;;
esac

os="$(uname -s)"
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "there is no better-backup agent for $(uname -m) machines yet" ;;
esac
case "$os" in
Linux)
    if command -v dpkg >/dev/null 2>&1; then
        kind=deb
    elif command -v rpm >/dev/null 2>&1; then
        kind=rpm
    else
        kind=tar.gz
    fi
    ;;
Darwin) kind=pkg ;;
*) die "this script installs on Linux and macOS; for $os see https://github.com/$repo#readme" ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fetch() { # fetch URL FILE
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        die "this needs curl or wget"
    fi
}
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

file="${BB_PACKAGE:-}"
if [ -z "$file" ]; then
    tag="${BB_VERSION:-}"
    if [ -z "$tag" ]; then
        fetch "https://api.github.com/repos/$repo/releases/latest" "$tmp/latest.json" ||
            die "could not ask GitHub for the newest release; set BB_VERSION, for example BB_VERSION=v0.3.0"
        tag="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
        [ -n "$tag" ] || die "better-backup has no release yet; see https://github.com/$repo/releases"
    fi
    version="${tag#v}"
    case "$kind" in
    pkg) name="better-backup-agent_${version}_darwin_universal.pkg" ;;
    *) name="better-backup-agent_${version}_linux_${arch}.${kind}" ;;
    esac
    base="https://github.com/$repo/releases/download/$tag"
    say "Downloading $name"
    file="$tmp/$name"
    fetch "$base/$name" "$file" || die "could not download $base/$name"
    # Check the download against the release's list of checksums.
    if fetch "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
        want="$(awk -v n="$name" '$2 == n { print $1 }' "$tmp/checksums.txt")"
        if [ -z "$want" ] && fetch "$base/$name.sha256" "$tmp/one.sha256" 2>/dev/null; then
            want="$(cut -d' ' -f1 "$tmp/one.sha256")"
        fi
        if [ -n "$want" ] && [ "$want" != "$(sha256 "$file")" ]; then
            die "the download does not match its checksum; not installing it"
        fi
    fi
else
    [ -f "$file" ] || die "no such file: $file"
    case "$file" in
    *.deb) kind=deb ;;
    *.rpm) kind=rpm ;;
    *.pkg) kind=pkg ;;
    *.tar.gz) kind=tar.gz ;;
    *) die "do not know how to install $file" ;;
    esac
fi

say "Installing the better-backup agent"
case "$kind" in
deb)
    if command -v apt-get >/dev/null 2>&1; then
        # apt wants a path with a slash to take it for a file.
        case "$file" in /*) ;; *) file="./$file" ;; esac
        DEBIAN_FRONTEND=noninteractive apt-get install -y "$file" >/dev/null
    else
        dpkg -i "$file" >/dev/null
    fi
    ;;
rpm)
    if command -v dnf >/dev/null 2>&1; then
        dnf install -y "$file" >/dev/null
    elif command -v zypper >/dev/null 2>&1; then
        zypper --non-interactive install --allow-unsigned-rpm "$file" >/dev/null
    else
        rpm -U --force "$file"
    fi
    ;;
pkg)
    installer -pkg "$file" -target / >/dev/null
    ;;
tar.gz)
    tar -xzf "$file" -C "$tmp" bb-agent
    bb-agent service stop >/dev/null 2>&1 || true
    install -m 755 "$tmp/bb-agent" /usr/local/bin/bb-agent
    bb-agent service install >/dev/null 2>&1 || true # already installed, on an update
    bb-agent service start
    ;;
esac
agent="$(command -v bb-agent || echo /usr/local/bin/bb-agent)"
say "Installed: $("$agent" version)"

# The agent starts with the installation. Wait for it to answer.
i=0
while [ $i -lt 30 ] && ! "$agent" status >/dev/null 2>&1; do
    i=$((i + 1))
    sleep 1
done

if "$agent" status 2>/dev/null | grep -q "^  Server:"; then
    say "This machine is already enrolled:"
    "$agent" status || true
elif [ -n "$code" ]; then
    if [ -n "${BB_AGENT_NAME:-}" ]; then
        "$agent" enroll "$code" --name "$BB_AGENT_NAME"
    else
        "$agent" enroll "$code"
    fi
else
    say ""
    say "Now enroll this machine with your server:"
    say "  sudo bb-agent enroll"
    say "It asks for the enrollment code from \"Add an agent\" in the server's web UI."
    [ "$kind" = pkg ] && say "Or open the Better Backup app, in Applications."
fi
exit 0
