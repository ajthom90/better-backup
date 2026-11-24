#!/bin/sh
# Stop the agent when the package is removed, not when it is upgraded:
# deb passes "remove" or "upgrade", rpm the number of versions left.
case "$1" in
    upgrade | 1 | 2) exit 0 ;;
esac
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl disable --now better-backup-agent.service 2>/dev/null || true
fi
