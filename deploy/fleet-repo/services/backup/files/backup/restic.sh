# Runs restic in the restic container with the repository and its credentials
# from the Podman secret restic_env (shell KEY=value lines, see README
# "Reference bundles: backup, restore-verify"). Used by backup.sh and by the
# restore-verify bundle.
set -eu
set -a
. /run/secrets/restic_env
set +a
exec restic "$@"
