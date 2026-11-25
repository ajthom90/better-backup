#!/bin/sh
# Installs the macOS server package on this Mac and uses it:
#
#   packaging/test/server-macos.sh SERVER-PKG
#
# The server must come up by itself, answer on its port, hand out the
# agent installer it carries, keep its data across an upgrade, and go
# away with the uninstaller while the data stays. For CI runners.
set -eu
pkg="$1"
data="/Library/Application Support/better-backup"
fail() {
    echo "FAILED: $*" >&2
    sudo tail -n 40 /Library/Logs/better-backup-server.log 2>/dev/null || true
    exit 1
}
up() {
    n=0
    while [ $n -lt 40 ]; do
        if curl -ksf -o /dev/null https://localhost:8443/healthz; then return 0; fi
        n=$((n + 1))
        sleep 1
    done
    return 1
}

# Another test's server may still hold the port.
sudo pkill -x bb-server 2>/dev/null || true
sleep 1

echo "== install"
sudo installer -pkg "$pkg" -target / || fail "the installer failed"
up || fail "the server did not answer on port 8443"
sudo test -f "$data/admin.json" || fail "no admin.json in the data folder"
sudo launchctl print system/better-backup-server >/dev/null || fail "launchd does not know the server"
sudo /usr/local/bin/bb status || fail "bb status failed"

echo "== the agent installer it hands out"
token=$(sudo sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' "$data/admin.json")
list=$(curl -ksf -H "Authorization: Bearer $token" https://localhost:8443/api/v1/installers) || fail "listing installers failed"
echo "$list"
case "$list" in
*darwin_universal.pkg*) ;;
*) fail "the server does not offer the macOS agent installer" ;;
esac

echo "== upgrade keeps the data"
before=$(sudo cat "$data/repo.key")
sudo installer -pkg "$pkg" -target / || fail "installing again failed"
up || fail "the server did not come back after the upgrade"
[ "$before" = "$(sudo cat "$data/repo.key")" ] || fail "the repository key changed"

echo "== uninstall keeps the backups"
sudo /usr/local/bin/bb-server-uninstall || fail "the uninstaller failed"
sleep 2
if curl -ksf -o /dev/null https://localhost:8443/healthz; then fail "the server still answers after it was removed"; fi
[ ! -e /usr/local/bin/bb-server ] || fail "bb-server was left behind"
sudo test -f "$data/repo.key" || fail "the uninstaller removed the data"
sudo rm -rf "$data"
echo "== all good"
