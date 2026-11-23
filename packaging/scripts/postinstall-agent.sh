#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable better-backup-agent.service || true
    # The unit starts only after enrollment (see ConditionPathExists).
    systemctl restart better-backup-agent.service || true
fi
echo "Enroll this machine, then start the agent:"
echo "  bb-agent enroll --server https://SERVER:8443 --token TOKEN --fingerprint FP"
echo "  systemctl restart better-backup-agent"
