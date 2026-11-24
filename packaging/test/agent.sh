#!/bin/sh
# Installs an agent package on this machine and puts it through its
# paces, for real: the service the package set up, a server started
# here, enrollment, a backup, an upgrade, a silent enrollment, removal.
# CI runs it on the runners; it changes the machine it runs on.
#
#   packaging/test/agent.sh PACKAGE TOOLS-DIR [SHOTS-DIR]
#
# PACKAGE is the .deb or .pkg, TOOLS-DIR holds bb-server and bb built
# for this machine, SHOTS-DIR receives screenshots (macOS).
set -eu

pkg="$1"
tools="$(cd "$2" && pwd)"
shots="${3:-}"
os="$(uname -s)"
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
export BB_DATA="$work/data"

step() { printf '\n=== %s\n' "$*"; }
fail() {
    printf 'FAILED: %s\n' "$*" >&2
    echo "--- agent status"
    sudo bb-agent status || true
    echo "--- agent log"
    if [ "$os" = Darwin ]; then
        tail -n 50 /Library/Logs/better-backup-agent.log || true
    else
        sudo journalctl -u better-backup-agent -n 50 --no-pager || true
    fi
    echo "--- server log"
    tail -n 30 "$work/server.log" || true
    exit 1
}
# until SECONDS COMMAND...: retry until it succeeds.
until_ok() {
    n="$1"
    shift
    while [ "$n" -gt 0 ]; do
        if "$@" >/dev/null 2>&1; then return 0; fi
        n=$((n - 1))
        sleep 1
    done
    return 1
}
status_has() { sudo bb-agent status 2>&1 | grep -q "$1"; }
service_running() {
    if [ "$os" = Darwin ]; then
        sudo launchctl print system/better-backup-agent 2>/dev/null | grep -q "state = running"
    else
        systemctl is-active --quiet better-backup-agent
    fi
}
new_code() { "$tools/bb" token create --for https://127.0.0.1:8443 | grep -o 'bb1\.[A-Za-z0-9_-]*'; }

step "start a server"
"$tools/bb-server" >"$work/server.log" 2>&1 &
server=$!
trap 'kill $server 2>/dev/null || true' EXIT
until_ok 30 "$tools/bb" status || fail "the server did not start"

step "install with the script, no code"
sudo env BB_PACKAGE="$pkg" sh "$here/../install.sh" || fail "install.sh failed"
until_ok 20 service_running || fail "the service is not running after the install"
until_ok 20 status_has "waiting to be enrolled" || fail "the agent does not say it is waiting to be enrolled"
sudo bb-agent status

if [ "$os" = Linux ]; then
    step "only root may manage the agent"
    if bb-agent status >"$work/out" 2>&1; then fail "an ordinary user was let in"; fi
    grep -q "needs root" "$work/out" || fail "wrong refusal: $(cat "$work/out")"
else
    step "administrators may manage the agent without sudo"
    bb-agent status | grep -q "waiting to be enrolled" || fail "the admin group cannot reach the agent"
    ls -l /var/run/better-backup-agent.sock
fi

step "enroll"
sudo bb-agent enroll "$(new_code)" --name ci-machine || fail "enroll failed"
until_ok 20 status_has "connected (" || fail "not connected after enrolling"
"$tools/bb" agents

step "back up now"
mkdir -p "$work/src"
head -c 5000000 /dev/urandom >"$work/src/data.bin"
echo hello >"$work/src/hello.txt"
"$tools/bb" policy add --name files --agent ci-machine --path "$work/src" --daily-at 03:00
until_ok 20 status_has "files, daily at 03:00" || fail "the agent does not list its policy"
sudo bb-agent backup || fail "backup failed to start"
until_ok 60 status_has "files, daily at 03:00: last backup" || fail "no backup finished"
sudo bb-agent status
sudo bb-agent status --json | grep -q '"last_status": "success"' || fail "the backup did not succeed"
"$tools/bb" snapshots

if [ "$os" = Darwin ]; then
    step "the app"
    app="/Applications/Better Backup.app"
    "$app/Contents/MacOS/better-backup" version || fail "the app does not run"
    codesign --verify --strict "$app" /usr/local/bin/bb-agent || fail "bad signature"
    lipo -archs "$app/Contents/MacOS/better-backup" /usr/local/bin/bb-agent
    open "$app"
    until_ok 20 pgrep -x better-backup || fail "the app did not start"
    sleep 8
    pgrep -x better-backup >/dev/null || fail "the app quit by itself"
    if [ -n "$shots" ]; then
        mkdir -p "$shots"
        screencapture -x "$shots/app-connected.png" || true
    fi
    osascript -e 'quit app "Better Backup"' || pkill -x better-backup || true
    sudo bb-agent status --json | grep '"full_disk_access"' || fail "no Full Disk Access report"
fi

step "upgrade in place: still running, still enrolled"
if [ "$os" = Darwin ]; then
    sudo installer -pkg "$pkg" -target / || fail "reinstall failed"
else
    sudo dpkg -i "$pkg" || fail "reinstall failed"
fi
until_ok 20 service_running || fail "the service is not running after the upgrade"
until_ok 30 status_has "connected (" || fail "not connected after the upgrade"

step "the setup page in a browser"
sudo bb-agent setup --no-browser >"$work/setup.log" 2>&1 &
until_ok 10 grep -q "http://127.0.0.1" "$work/setup.log" || fail "setup printed no address"
addr="$(grep -o 'http://127.0.0.1:[0-9]*/?key=[0-9a-f]*' "$work/setup.log")"
curl -fsS -c "$work/jar" -L "$addr" | grep -q "<title>Better Backup</title>" || fail "the page did not load"
curl -fsS -b "$work/jar" "${addr%%/\?*}/api/state" | grep -q '"connected":true' || fail "the page's state is wrong"
if curl -fsS "${addr%%/\?*}/api/state" >/dev/null 2>&1; then fail "the page answered without its key"; fi
sudo pkill -f "bb-agent setup" || true

step "unenroll, then enroll from a file an installer left"
sudo bb-agent unenroll
status_has "waiting to be enrolled" || fail "still enrolled after unenroll"
if [ "$os" = Darwin ]; then
    state="/Library/Application Support/better-backup-agent"
    # As a management tool would: enroll.conf beside the package.
    mkdir "$work/silent"
    cp "$pkg" "$work/silent/"
    printf 'code=%s\nname=silent-install\n' "$(new_code)" >"$work/silent/enroll.conf"
    sudo installer -pkg "$work/silent/$(basename "$pkg")" -target / || fail "silent install failed"
else
    state=/var/lib/better-backup-agent
    printf 'code=%s\nname=silent-install\n' "$(new_code)" | sudo tee "$state/enroll.conf" >/dev/null
fi
until_ok 40 status_has "silent-install" || fail "the agent did not enroll from enroll.conf"
until_ok 20 status_has "connected (" || fail "not connected after the silent enrollment"
if sudo test -e "$state/enroll.conf"; then fail "enroll.conf was left behind"; fi
sudo bb-agent status

if [ "$os" = Linux ]; then
    step "members of the better-backup group may manage the agent"
    sudo groupadd better-backup
    sudo usermod -aG better-backup "$(id -un)"
    sudo systemctl restart better-backup-agent
    until_ok 20 status_has "connected (" || fail "not connected after the restart"
    ls -l /var/run/better-backup-agent.sock
    # A new session, so that the new group counts.
    sudo -u "$(id -un)" bb-agent status | grep -q "silent-install" || fail "the group was not let in"
fi

step "remove"
if [ "$os" = Darwin ]; then
    sudo bb-agent-uninstall || fail "uninstall failed"
    if sudo launchctl print system/better-backup-agent >/dev/null 2>&1; then fail "the service is still loaded"; fi
    for f in /usr/local/bin/bb-agent "/Applications/Better Backup.app" /Library/LaunchDaemons/better-backup-agent.plist "$state"; do
        if [ -e "$f" ]; then fail "$f was left behind"; fi
    done
else
    sudo apt-get remove -y better-backup-agent || fail "removal failed"
    if service_running; then fail "the service still runs"; fi
    if command -v bb-agent >/dev/null 2>&1; then fail "bb-agent was left behind"; fi
fi
printf '\nAll good.\n'
