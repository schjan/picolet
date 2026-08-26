# Containerized Dev Workflow for Picolet

## Prerequisites (already done)
- `picolet apply` and `picolet down` commands exist
- `data_dir` config field works
- `dev` role + dev-host in example fleet
- `task dev` and `task dev:teardown` work for `go run` workflow
- `SystemdManager` interface has `Close()`

## Goal
Add `task dev:install` to build and deploy picolet itself as a rootless container on the Pi using Podman Quadlet `.build` and `.container` units. This enables testing picolet running inside a container managing the host's Podman and systemd -- the actual production deployment model.

## What to build

### 1. Quadlet template files in `dev/`

**`dev/picolet-dev.build.tmpl`** -- builds picolet image from source:
```ini
[Build]
ImageTag=localhost/picolet-dev
SetWorkingDirectory=REPO_ROOT_PLACEHOLDER
```

**`dev/picolet-dev.container.tmpl`** -- runs picolet with host access:
```ini
[Container]
Image=picolet-dev.build
ContainerName=picolet-dev

# Podman socket (manage host containers)
Volume=%t/podman/podman.sock:/run/podman/podman.sock

# D-Bus user session (manage host systemd units)
Volume=%t/bus:/run/dbus/user_bus_socket
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/dbus/user_bus_socket

# Quadlet + systemd output dirs (picolet writes here)
Volume=%h/.config/containers/systemd/picolet:/etc/containers/systemd/picolet
Volume=%h/.config/systemd/user:/etc/systemd/system

# Mount repo for git polling via file:// protocol
Volume=REPO_ROOT_PLACEHOLDER:/repo:ro

# Config and secrets
Volume=DEV_DATA_PLACEHOLDER/config.yml:/etc/picolet/config.yml:ro
Volume=DEV_DATA_PLACEHOLDER/secrets:/etc/picolet/secrets:ro

[Install]
WantedBy=default.target
```

**`dev/config-container.yml.tmpl`** -- agent config for containerized mode:
```yaml
hostname: dev-host
repo_url: "file:///repo"
repo_branch: BRANCH_PLACEHOLDER
poll_interval: 24h
rootless: false
systemd_user: true
secrets_dir: /etc/picolet/secrets
podman_socket: /run/podman/podman.sock
```

Key config choices:
- `rootless: false` -- picolet runs as UID 0 inside container (system paths work)
- `systemd_user: true` -- connect to host's user D-Bus (mounted into container)
- `repo_url: file:///repo` -- git polls from mounted local repo (committed changes only)

### 2. `dev/install.sh` -- generates and installs quadlet files

```bash
#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEV_DATA_DIR="${REPO_ROOT}/.picolet-dev"
QUADLET_DIR="${HOME}/.config/containers/systemd"
BRANCH="$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD)"

mkdir -p "${DEV_DATA_DIR}/secrets" "${QUADLET_DIR}" \
         "${HOME}/.config/containers/systemd/picolet"

# Copy secrets from example fleet
cp -n "${REPO_ROOT}/testdata/example-fleet/secrets/e2e_secret.txt" \
      "${DEV_DATA_DIR}/secrets/" 2>/dev/null || true

# Generate config
sed -e "s|BRANCH_PLACEHOLDER|${BRANCH}|" \
    "${REPO_ROOT}/dev/config-container.yml.tmpl" > "${DEV_DATA_DIR}/config.yml"

# Generate quadlet files (substitute repo path)
sed -e "s|REPO_ROOT_PLACEHOLDER|${REPO_ROOT}|" \
    "${REPO_ROOT}/dev/picolet-dev.build.tmpl" > "${QUADLET_DIR}/picolet-dev.build"

sed -e "s|REPO_ROOT_PLACEHOLDER|${REPO_ROOT}|" \
    -e "s|DEV_DATA_PLACEHOLDER|${DEV_DATA_DIR}|" \
    "${REPO_ROOT}/dev/picolet-dev.container.tmpl" > "${QUADLET_DIR}/picolet-dev.container"

systemctl --user daemon-reload
systemctl --user start picolet-dev.service

echo "picolet-dev started. Use 'task dev:logs' to follow logs."
```

### 3. Taskfile tasks

```yaml
dev:install:
  desc: Build & deploy picolet as a rootless container (Linux only)
  platforms: [linux]
  cmds:
    - bash dev/install.sh

dev:logs:
  desc: Follow picolet dev container logs (Linux only)
  platforms: [linux]
  cmds:
    - journalctl --user -u picolet-dev.service -f
```

Update `dev:teardown` to also handle the containerized setup:
```yaml
dev:teardown:
  desc: Remove all dev resources and picolet container (Linux only)
  platforms: [linux]
  cmds:
    - go run -tags "{{.BUILD_TAGS}}" ./cmd/picolet down
        --config {{.DEV_DATA_DIR}}/config.yml 2>/dev/null || true
    - systemctl --user stop picolet-dev.service 2>/dev/null || true
    - rm -f ~/.config/containers/systemd/picolet-dev.build
    - rm -f ~/.config/containers/systemd/picolet-dev.container
    - systemctl --user daemon-reload
    - podman rm -f picolet-dev 2>/dev/null || true
    - podman rmi localhost/picolet-dev 2>/dev/null || true
    - rm -rf {{.DEV_DATA_DIR}}
```

### 4. Verification
1. `task dev:install` -- builds image, starts picolet-dev container
2. `task dev:logs` -- picolet starts, clones from mounted repo, reconciles dev-host
3. Containers deployed by picolet appear: `podman ps` shows picolet-e2e-test
4. `task dev:teardown` -- everything cleaned up (picolet-dev + deployed workloads)

### Notes
- The `.build` and `.container` quadlet files go in `~/.config/containers/systemd/` (NOT in picolet's managed subdir `picolet/`) to avoid picolet managing itself
- `%t` and `%h` are systemd specifiers expanded at runtime (XDG_RUNTIME_DIR and HOME)
- Raspberry Pi OS doesn't use SELinux, so no `:Z` volume labels needed
- The container runs as UID 0 which maps to the host user via rootless Podman user namespace
