#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
    systemctl enable better-backup-agent.service || true
    systemctl restart better-backup-agent.service || true
fi
if [ ! -e /var/lib/better-backup-agent/agent.json ] && [ ! -e /var/lib/better-backup-agent/enroll.conf ]; then
    echo "better-backup agent installed. Enroll this machine with your server:"
    echo "  sudo bb-agent enroll"
    echo "It asks for the enrollment code from \"Add an agent\" in the server's web UI."
fi
