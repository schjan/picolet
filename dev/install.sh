#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "${SCRIPT_DIR}" rev-parse --show-toplevel)"
DEV_DATA_DIR="${REPO_ROOT}/.picolet-dev"
QUADLET_DIR="${HOME}/.config/containers/systemd"
BRANCH="$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD)"

# Create directories
mkdir -p "${DEV_DATA_DIR}/secrets" "${DEV_DATA_DIR}/state" \
         "${QUADLET_DIR}" \
         "${HOME}/.config/containers/systemd/picolet" \
         "${HOME}/.config/systemd/user"

# Copy example secrets (no-clobber)
cp -n "${REPO_ROOT}/testdata/example-fleet/secrets/"* \
      "${DEV_DATA_DIR}/secrets/" 2>/dev/null || true

# Generate config from template
sed "s|BRANCH_PLACEHOLDER|${BRANCH}|g" \
    "${SCRIPT_DIR}/config-container.yml.tmpl" > "${DEV_DATA_DIR}/config.yml"

# Generate quadlet files from templates
sed "s|REPO_ROOT_PLACEHOLDER|${REPO_ROOT}|g" \
    "${SCRIPT_DIR}/picolet-dev.build.tmpl" > "${QUADLET_DIR}/picolet-dev.build"

sed -e "s|REPO_ROOT_PLACEHOLDER|${REPO_ROOT}|g" \
    -e "s|DEV_DATA_PLACEHOLDER|${DEV_DATA_DIR}|g" \
    "${SCRIPT_DIR}/picolet-dev.container.tmpl" > "${QUADLET_DIR}/picolet-dev.container"

# Reload and start
systemctl --user daemon-reload
systemctl --user restart picolet-dev.service

echo "picolet-dev started. Use 'task dev:logs' to follow logs."
