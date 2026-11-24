#!/bin/sh
# Removes the better-backup agent from this Mac. The backups on the
# server are not touched. Pass --keep-identity to leave the agent's
# enrollment in place, so that installing again picks up where it was.
set -u
if [ "$(id -u)" != 0 ]; then
    echo "Run this with sudo: sudo bb-agent-uninstall" >&2
    exit 1
fi
launchctl bootout system/better-backup-agent 2>/dev/null
pkill -x better-backup 2>/dev/null
rm -f /Library/LaunchDaemons/better-backup-agent.plist /usr/local/bin/bb-agent /var/run/better-backup-agent.sock
rm -rf "/Applications/Better Backup.app"
if [ "${1:-}" != "--keep-identity" ]; then
    rm -rf "/Library/Application Support/better-backup-agent"
fi
rm -f /Library/Logs/better-backup-agent.log
pkgutil --forget com.github.ajthom90.better-backup.agent >/dev/null 2>&1
rm -f /usr/local/bin/bb-agent-uninstall
echo "The better-backup agent was removed. If this Mac is not coming back, also remove"
echo "it in the server's web UI. Its backups stay on the server until you delete them."
