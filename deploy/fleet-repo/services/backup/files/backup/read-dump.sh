# restic's --stdin-from-command in the restic container (backup.sh): copies the
# forge dump from the FIFO to stdout, then fails unless the dump exited 0, so
# restic saves no snapshot of a failed or truncated dump.
set -eu
cat /dump/forge-dump.tar
rc="$(cat /dump/dump.rc)"
if [ "$rc" != 0 ]; then
	echo "forgejo dump exited $rc: no snapshot saved" >&2
	exit 1
fi
