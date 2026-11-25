#!/bin/sh
# Removes the better-backup server from this Mac. The backups, the
# repository key and the server's settings are kept, so that installing
# again carries on where it was. Pass --delete-backups to remove them
# too: that cannot be undone.
set -u
if [ "$(id -u)" != 0 ]; then
    echo "Run this with sudo: sudo bb-server-uninstall" >&2
    exit 1
fi
launchctl bootout system/better-backup-server 2>/dev/null
rm -f /Library/LaunchDaemons/better-backup-server.plist /usr/local/bin/bb-server /usr/local/bin/bb
rm -rf /usr/local/bin/installers
rm -f /Library/Logs/better-backup-server.log
pkgutil --forget com.github.ajthom90.better-backup.server >/dev/null 2>&1
if [ "${1:-}" = "--delete-backups" ]; then
    rm -rf "/Library/Application Support/better-backup"
    echo "The better-backup server and its backups were removed."
else
    echo "The better-backup server was removed. Its backups and settings are still in"
    echo "/Library/Application Support/better-backup (and wherever Settings put the backups)."
fi
rm -f /usr/local/bin/bb-server-uninstall
