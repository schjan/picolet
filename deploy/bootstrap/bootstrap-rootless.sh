#!/usr/bin/env bash
# bootstrap-rootless.sh — one-time rootless setup. Run as your normal user (NOT root).
# Prerequisite: ~/.config/picolet/config.yml must exist with systemd_user: true
set -euo pipefail

PICOLET_IMAGE="${PICOLET_IMAGE:-ghcr.io/schjan/picolet:v0.1.0}"
CONFIG_DIR="${HOME}/.config/picolet"
DATA_DIR="${HOME}/.local/share/picolet"
QUADLET_DIR="${HOME}/.config/containers/systemd"
PICOLET_CONFIG="${PICOLET_CONFIG:-${CONFIG_DIR}/config.yml}"

[[ -n "${XDG_RUNTIME_DIR:-}" ]] || { echo "ERROR: XDG_RUNTIME_DIR is not set. Run from a login session." >&2; exit 1; }
[[ -f "$PICOLET_CONFIG" ]] || { echo "ERROR: $PICOLET_CONFIG not found." >&2; exit 1; }

mkdir -p "${CONFIG_DIR}/secrets" "${DATA_DIR}" "${QUADLET_DIR}" \
         "${HOME}/.config/systemd/user"

cat > "${QUADLET_DIR}/picolet.container" << EOF
[Unit]
Description=Picolet GitOps Agent (bootstrap)
After=network-online.target podman.socket
Wants=network-online.target podman.socket

[Container]
Image=${PICOLET_IMAGE}
ContainerName=picolet
Volume=${CONFIG_DIR}:/etc/picolet:ro
Volume=${DATA_DIR}:/var/lib/picolet
Volume=${QUADLET_DIR}:/etc/containers/systemd
Volume=${HOME}/.config/systemd/user:/etc/systemd/system
Volume=${XDG_RUNTIME_DIR}/systemd/private:${XDG_RUNTIME_DIR}/systemd/private
Volume=${XDG_RUNTIME_DIR}/podman/podman.sock:/run/podman/podman.sock
Network=host
Environment=PICOLET_CONFIG=/etc/picolet/config.yml
Environment=XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR}
Restart=always
RestartSec=10s
StopTimeout=30
HealthCmd=/picolet healthcheck
HealthInterval=30s
HealthRetries=3
HealthStartPeriod=90s
HealthTimeout=5s
HealthOnFailure=restart

[Service]
Restart=on-failure
TimeoutStartSec=90
TimeoutStopSec=40

[Install]
WantedBy=default.target
EOF

# Enable lingering so user services survive logout
loginctl enable-linger "$(whoami)"

podman pull "${PICOLET_IMAGE}"
systemctl --user daemon-reload
systemctl --user enable --now picolet.service

echo "Picolet starting (rootless). Monitor: journalctl --user -fu picolet.service"
echo "First reconcile will self-restart once — this is expected."
