#!/bin/sh
if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now better-backup-server.service 2>/dev/null || true
fi
