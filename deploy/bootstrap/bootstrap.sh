#!/usr/bin/env bash
# bootstrap.sh — one-time rootful setup. Run as root.
# Prerequisite: /etc/picolet/config.yml must exist (see config.example.yml).
set -euo pipefail

PICOLET_IMAGE="${PICOLET_IMAGE:-ghcr.io/schjan/picolet:v0.1.0}"
PICOLET_CONFIG="${PICOLET_CONFIG:-/etc/picolet/config.yml}"

[[ -f "$PICOLET_CONFIG" ]] || { echo "ERROR: $PICOLET_CONFIG not found." >&2; exit 1; }

mkdir -p /etc/containers/systemd /etc/picolet/secrets /var/lib/picolet

cat > /etc/containers/systemd/picolet.container << EOF
[Unit]
Description=Picolet GitOps Agent (bootstrap)
After=network-online.target podman.socket
Wants=network-online.target podman.socket

[Container]
Image=${PICOLET_IMAGE}
ContainerName=picolet
User=root
Volume=/etc/picolet:/etc/picolet:ro,z
Volume=/var/lib/picolet:/var/lib/picolet:z
Volume=/etc/containers/systemd:/etc/containers/systemd:z
Volume=/etc/systemd/system:/etc/systemd/system:z
Volume=/run/dbus/system_bus_socket:/run/dbus/system_bus_socket
Volume=/run/podman/podman.sock:/run/podman/podman.sock
Network=host
Environment=PICOLET_CONFIG=/etc/picolet/config.yml
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

systemctl daemon-reload
systemctl enable --now picolet.service

echo "Picolet starting. Monitor: journalctl -fu picolet.service"
echo "First reconcile will self-restart once — this is expected."
