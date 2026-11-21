#!/usr/bin/env bash
# E2E runner: runs every E2E scenario (each top-level test in ./e2e) in its
# own disposable runtime: a Linux user created for it, with its own home,
# lingering user systemd manager, Podman store and socket. The scenario's
# Agent, CLI and Podman calls therefore use the standard rootless
# destinations of that user only, and never the invoking user's
# (`internal/e2eruntime` refuses to run a scenario anywhere else).
#
#   scripts/e2e.sh [run]   build the test binary once, run the scenarios
#                          (E2E_PARALLEL at a time), dispose of every runtime
#   scripts/e2e.sh recover dispose of the runtimes an interrupted run left
#
# Needs Linux with systemd, Podman >= 5.2, useradd with subordinate-ID
# allocation (SUB_UID_COUNT in /etc/login.defs) and root through sudo, unless
# run as root. The runner itself builds as the invoking user and uses root
# only to provision and dispose.
#
# Ownership: before creating a user the runner writes its allocation record
# (root-owned, /var/lib/picolet-e2e/<id>/allocation); the user's passwd
# comment names the same id. Disposal and recovery act only on recorded
# allocations whose account carries that comment, and remove the record last,
# so a failed disposal stays recoverable.
#
# Environment:
#   BUILD_TAGS    Go build tags (required for run; Taskfile.yml and CI set them)
#   E2E_RUN       regexp selecting scenarios (default: all)
#   E2E_PARALLEL  scenarios run at once (default 4)
#   E2E_TIMEOUT   seconds per scenario (default 300)
#   E2E_LOG_DIR   scenario logs (default ./e2e-logs)
#   E2E_IMAGES    images pulled once into a private store and loaded into
#                 every scenario's store (default: the alpine tags the
#                 scenarios use)
#   E2E_KEEP=1    keep the runtimes for debugging; `recover` disposes of them
#   GITHUB_TOKEN, GITHUB_HEAD_REF, GITHUB_REF_NAME   passed to the scenarios
#                 (TestE2EPipeline clones the branch over HTTPS with the token;
#                 without one it clones a bare copy of the local HEAD)
set -euo pipefail

readonly STATE_ROOT=/var/lib/picolet-e2e # e2eruntime.StateRoot
readonly ID_PREFIX=pe2e-
readonly PARALLEL="${E2E_PARALLEL:-4}"
readonly TIMEOUT="${E2E_TIMEOUT:-300}"
readonly IMAGES="${E2E_IMAGES:-docker.io/library/alpine:3.22 docker.io/library/alpine:3.23 docker.io/library/alpine:3.24}"
# The neighbour scenario plants sentinels before the other scenarios run and
# checks them after; see e2e/orphan_startup_test.go.
readonly NEIGHBOUR=TestE2EOrphanNeighbour
readonly STEP_TIMEOUT=60

SUDO=()
if [[ $(id -u) -ne 0 ]]; then SUDO=(sudo --non-interactive); fi
root() { "${SUDO[@]}" "$@"; }
log() { printf 'e2e: %s\n' "$*" >&2; }

# --- allocation records -----------------------------------------------------

record_get() { # record_get <id> <key>
	root sed -n "s/^$2=//p" "$STATE_ROOT/$1/allocation" 2>/dev/null | head -n1
}

record_write() { # record_write <id> <content>
	local dir="$STATE_ROOT/$1"
	printf '%s' "$2" | root tee "$dir/allocation.tmp" >/dev/null
	root chmod 0644 "$dir/allocation.tmp"
	root mv -f "$dir/allocation.tmp" "$dir/allocation"
}

# as_user <id> <cmd...>: runs cmd as the allocation's user with exactly the
# environment e2eruntime.Verify accepts, from the user's home.
as_user() {
	local id=$1 uid home
	shift
	uid=$(record_get "$id" uid)
	home=$(record_get "$id" home)
	local -a env=(
		PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
		LANG=C.UTF-8 USER="$id" LOGNAME="$id" HOME="$home" TMPDIR="$home/tmp"
		XDG_RUNTIME_DIR="/run/user/$uid"
		DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus"
		PODMAN_SOCKET="/run/user/$uid/podman/podman.sock"
		PICOLET_E2E_ALLOCATION="$STATE_ROOT/$id/allocation"
	)
	local var
	for var in GITHUB_TOKEN GITHUB_HEAD_REF GITHUB_REF_NAME PICOLET_E2E_TESTDATA PICOLET_E2E_REPO_URL PICOLET_E2E_BRANCH; do
		if [[ -n ${!var:-} ]]; then env+=("$var=${!var}"); fi
	done
	root runuser -u "$id" -- env -i -C "$home" "${env[@]}" "$@"
}

wait_for() { # wait_for <seconds> <cmd...>
	local deadline=$((SECONDS + $1))
	shift
	until "$@" 2>/dev/null; do
		if ((SECONDS >= deadline)); then return 1; fi
		sleep 0.5
	done
}

# allocate <scenario>: creates the runtime and prints its id once its record
# exists (no account is created before that). On failure the record stays
# for dispose.
allocate() {
	local scenario=$1 id home uid
	id="$ID_PREFIX$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
	home="$STATE_ROOT/$id/home"
	# No -p: an id colliding with a leftover allocation fails here instead of
	# overwriting its record.
	if ! root mkdir -m 0755 "$STATE_ROOT/$id" || ! record_write "$id" "id=$id
user=$id
home=$home
scenario=$scenario
run=$RUN_ID
runner_pid=$$
"; then
		log "$scenario: recording allocation $id failed"
		return 1
	fi
	echo "$id"
	{
		root useradd --create-home --home-dir "$home" --shell /bin/bash --user-group \
			--comment "picolet-e2e $id" "$id" &&
			uid=$(id -u "$id") &&
			record_write "$id" "$(root cat "$STATE_ROOT/$id/allocation")
uid=$uid
" &&
			root grep -q "^$id:" /etc/subuid /etc/subgid &&
			pin_manager_env "$id" "$uid" &&
			root loginctl enable-linger "$id" &&
			root timeout "$STEP_TIMEOUT" systemctl start "user@$uid.service" &&
			wait_for 30 root test -S "/run/user/$uid/bus" &&
			manager_env_ok "$id" &&
			as_user "$id" mkdir -p "$home/tmp" &&
			as_user "$id" timeout "$STEP_TIMEOUT" systemctl --user start podman.socket &&
			wait_for 30 root test -S "/run/user/$uid/podman/podman.sock" &&
			{ [[ ! -f $SHARED/images.tar ]] || as_user "$id" timeout 300 podman load -q -i "$SHARED/images.tar" >/dev/null; }
	} >&2 || {
		log "$scenario: provisioning $id failed"
		return 1
	}
}

# The user manager's environment comes from PAM and from environment.d,
# which on Debian/Ubuntu includes /etc/environment; both may name another
# user's runtime and config (GitHub's runner image puts the runner's
# XDG_RUNTIME_DIR and XDG_CONFIG_HOME there), and the manager's generators,
# Quadlet's included, see that environment. A user@<uid> drop-in starts the
# manager without the PAM values, and the user's own environment.d file,
# sorted last, pins every one of them to the allocated user's.
readonly MANAGER_UNSET="XDG_CONFIG_HOME XDG_DATA_HOME XDG_STATE_HOME XDG_CACHE_HOME"
manager_dropin() { echo "/etc/systemd/system/user@$1.service.d/picolet-e2e.conf"; }

# manager_env <uid> <home>: the environment the manager must hand its units.
manager_env() {
	printf '%s\n' "XDG_RUNTIME_DIR=/run/user/$1" "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$1/bus" \
		"XDG_CONFIG_HOME=$2/.config" "XDG_DATA_HOME=$2/.local/share" \
		"XDG_STATE_HOME=$2/.local/state" "XDG_CACHE_HOME=$2/.cache"
}

pin_manager_env() { # pin_manager_env <id> <uid>
	local id=$1 uid=$2 dropin unset="" var
	dropin=$(manager_dropin "$uid")
	for var in $MANAGER_UNSET; do unset+=" -u $var"; done
	[[ -x /usr/lib/systemd/systemd ]] || {
		log "no /usr/lib/systemd/systemd to start the user manager with"
		return 1
	}
	as_user "$id" mkdir -p "$(record_get "$id" home)/.config/environment.d" &&
		manager_env "$uid" "$(record_get "$id" home)" |
		as_user "$id" tee "$(record_get "$id" home)/.config/environment.d/zz-picolet-e2e.conf" >/dev/null &&
		root install -d -m 0755 "$(dirname "$dropin")" &&
		printf '%s\n' "# picolet-e2e allocation $id" "[Service]" "ExecStart=" \
			"ExecStart=/usr/bin/env$unset XDG_RUNTIME_DIR=/run/user/$uid DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus /usr/lib/systemd/systemd --user" |
		root tee "$dropin" >/dev/null &&
		root systemctl daemon-reload
}

# manager_env_ok <id>: the running user manager hands its units exactly the
# allocated user's runtime, bus and XDG base dirs.
manager_env_ok() {
	local id=$1 env want
	env=$(as_user "$id" systemctl --user show-environment) || return 1
	while read -r want; do
		if ! grep -qxF "$want" <<<"$env"; then
			log "$id: the user manager's environment lacks $want:"
			printf '%s\n' "$env" >&2
			return 1
		fi
	done < <(manager_env "$(record_get "$id" uid)" "$(record_get "$id" home)")
}

# no_processes <uid>: true when uid runs no process; a failing pgrep (not
# "none found") is not taken for none.
no_processes() {
	local rc=0
	root pgrep -u "$1" >/dev/null || rc=$?
	((rc == 1))
}

# dispose <id>: stops the runtime's manager and processes, then removes its
# account (userdel also removes its group and subordinate IDs), home and
# record. Every step is bounded; a failure keeps the record and returns 1.
dispose() {
	local id=$1 uid acct comment acct_uid failed=0
	uid=$(record_get "$id" uid)
	acct=$(getent passwd "$id" || true)
	if [[ -n $acct ]]; then
		comment=$(cut -d: -f5 <<<"$acct")
		acct_uid=$(cut -d: -f3 <<<"$acct")
		if [[ $comment != "picolet-e2e $id" ]]; then
			log "$id: account exists but was not created by the E2E runner (comment '$comment'); not touching it"
			return 1
		fi
		# Interrupted before the record got its uid: the account names it.
		uid=${uid:-$acct_uid}
		if [[ $uid != "$acct_uid" ]]; then
			log "$id: recorded uid $uid is not the account's uid $acct_uid; not touching it"
			return 1
		fi
	fi
	if [[ -n $acct ]]; then
		root timeout "$STEP_TIMEOUT" loginctl disable-linger "$id" || failed=1
		root timeout "$STEP_TIMEOUT" systemctl stop "user@$uid.service" || failed=1
		root timeout "$STEP_TIMEOUT" loginctl terminate-user "$id" 2>/dev/null || true
		root pkill -KILL -u "$uid" 2>/dev/null || true
		wait_for 30 no_processes "$uid" || {
			log "$id: processes of uid $uid survive"
			failed=1
		}
		root timeout "$STEP_TIMEOUT" systemctl stop "user-runtime-dir@$uid.service" || failed=1
		root timeout "$STEP_TIMEOUT" find /tmp /var/tmp /dev/shm -xdev -uid "$uid" -delete 2>/dev/null || true
	fi
	# The drop-in goes while the account still holds the uid (a parallel
	# useradd may reuse it once userdel frees it), and only if it is ours.
	if ((failed == 0)) && [[ -n $uid ]] && root grep -qx "# picolet-e2e allocation $id" "$(manager_dropin "$uid")" 2>/dev/null; then
		root rm -f "$(manager_dropin "$uid")" &&
			root rmdir --ignore-fail-on-non-empty "$(dirname "$(manager_dropin "$uid")")" &&
			root systemctl daemon-reload || failed=1
	fi
	if ((failed == 0)) && getent passwd "$id" >/dev/null; then
		# Exit 12: the mail spool was missing; the account is still removed.
		root timeout "$STEP_TIMEOUT" userdel --remove "$id" 2>/dev/null || true
		if getent passwd "$id" >/dev/null; then
			log "$id: userdel failed"
			failed=1
		fi
	fi
	# Check only: editing these files here would race the parallel useradds.
	if ((failed == 0)) && root grep -q "^$id:" /etc/subuid /etc/subgid; then
		log "$id: subordinate IDs survive userdel"
		failed=1
	fi
	if ((failed == 0)) && getent group "$id" >/dev/null; then root groupdel "$id" || failed=1; fi
	if ((failed == 0)) && [[ -n $uid && -e /run/user/$uid ]]; then
		log "$id: /run/user/$uid survives"
		failed=1
	fi
	if ((failed == 0)); then
		root rm -rf --one-file-system "${STATE_ROOT:?}/$id" || failed=1
	fi
	if ((failed != 0)); then
		log "$id: disposal failed; the record stays in $STATE_ROOT/$id, run scripts/e2e.sh recover"
		return 1
	fi
}

# --- scenarios --------------------------------------------------------------

# interruptible <cmd...>: runs cmd as a job, so a signal to the runner runs
# its trap at once instead of after cmd.
interruptible() {
	"$@" &
	wait "$!"
}

# run_test <id> <name> <run-pattern> <log>: runs the test binary in the
# runtime; on failure captures the runtime's diagnostics before disposal.
run_test() {
	local id=$1 name=$2 pattern=$3 out=$4 uid rc=0
	uid=$(record_get "$id" uid)
	as_user "$id" timeout --kill-after=30 "$TIMEOUT" "$SHARED/e2e.test" \
		-test.run "$pattern" -test.v -test.count=1 -test.timeout "$((TIMEOUT - 30))s" >>"$out" 2>&1 || rc=$?
	if ((rc != 0)); then
		{
			echo "=== diagnostics of $id (uid $uid) after exit $rc"
			as_user "$id" systemctl --user list-units --all --no-pager || true
			as_user "$id" podman ps -a || true
			root journalctl "_UID=$uid" --no-pager -n 300 || true
		} >>"$out" 2>&1
	fi
	return "$rc"
}

# scenario <name>: one runtime, one run, one disposal; status in $WORK/status.
scenario() {
	local name=$1 out="$LOG_DIR/$1.log" id rc=0
	: >"$out"
	if id=$(allocate "$name" 2>>"$out"); then
		run_test "$id" "$name" "^$name\$" "$out" || rc=$?
	else
		rc=1
	fi
	if [[ -n ${id:-} && ${E2E_KEEP:-} != 1 ]]; then dispose "$id" 2>>"$out" || echo "disposal" >"$WORK/status/$name.cleanup"; fi
	echo "$rc" >"$WORK/status/$name"
}

report() {
	local name status failed=0
	for name in "${SCENARIOS[@]}"; do
		status=$(cat "$WORK/status/$name" 2>/dev/null || echo "not run")
		if [[ $status == 0 ]]; then
			log "PASS $name"
		else
			log "FAIL $name (exit $status): $LOG_DIR/$name.log"
			failed=1
		fi
		if [[ -f $WORK/status/$name.cleanup ]]; then
			log "CLEANUP FAILED $name: see $LOG_DIR/$name.log"
			failed=1
		fi
	done
	return "$failed"
}

prepare_shared() {
	: "${BUILD_TAGS:?BUILD_TAGS must be set (Taskfile.yml defines it; run task e2e)}"
	go test -c -race -tags "e2e,$BUILD_TAGS" -o "$WORK/e2e.test" ./e2e
	SHARED="$STATE_ROOT/run-$RUN_ID"
	root install -d -m 0755 -o root -g root "$STATE_ROOT" "$SHARED"
	printf 'run=%s\nrunner_pid=%s\n' "$RUN_ID" "$$" | root tee "$SHARED/runner" >/dev/null
	root install -m 0755 -o root -g root "$WORK/e2e.test" "$SHARED/e2e.test"
	root cp -r testdata "$SHARED/testdata"
	root chmod -R a+rX,go-w "$SHARED/testdata"
	export PICOLET_E2E_TESTDATA="$SHARED/testdata"
	if [[ -z ${GITHUB_TOKEN:-} ]]; then
		# Without a token TestE2EPipeline clones the local HEAD (committed
		# changes only) instead of GitHub over SSH, which a scenario user has
		# no agent for.
		local branch
		branch=$(git rev-parse --abbrev-ref HEAD)
		if [[ $branch == HEAD ]]; then branch=e2e-head; fi
		git clone -q --bare --no-local . "$WORK/picolet.git"
		git -C "$WORK/picolet.git" branch -f "$branch" "$(git rev-parse HEAD)"
		root cp -r "$WORK/picolet.git" "$SHARED/picolet.git"
		root chmod -R a+rX,go-w "$SHARED/picolet.git"
		export PICOLET_E2E_REPO_URL="file://$SHARED/picolet.git" PICOLET_E2E_BRANCH="$branch"
	fi
	# A private store, so neither the invoking user's nor root's default
	# store is touched; a failed pull leaves the scenarios to pull themselves.
	if read -ra images <<<"$IMAGES" && ((${#images[@]} > 0)); then
		if root podman --root "$WORK/images/root" --runroot "$WORK/images/run" pull -q "${images[@]}" >/dev/null &&
			root podman --root "$WORK/images/root" --runroot "$WORK/images/run" save -q -m -o "$SHARED/images.tar" "${images[@]}"; then
			root chmod 0644 "$SHARED/images.tar"
		else
			log "pulling $IMAGES failed; every scenario pulls on its own"
		fi
		root rm -rf --one-file-system "$WORK/images"
	fi
}

# kill_tree <pid>: kills pid and its descendants (some are root's: sudo,
# useradd, runuser), children first.
kill_tree() {
	local child
	for child in $(pgrep -P "$1" || true); do kill_tree "$child"; done
	root kill -KILL "$1" 2>/dev/null || true
}

# cleanup_run disposes of every allocation this run recorded and its shared
# directory; it runs on exit and on interruption.
cleanup_run() {
	local rc=$? dir id pid
	trap - EXIT INT TERM
	# Stop every job and everything it started (a useradd in flight
	# included) before disposing, so no disposal races them.
	for pid in $(jobs -p); do kill_tree "$pid"; done
	wait 2>/dev/null || true
	if [[ ${E2E_KEEP:-} != 1 ]]; then
		for dir in "$STATE_ROOT/$ID_PREFIX"*/; do
			id=$(basename "$dir")
			[[ -f $dir/allocation && $(record_get "$id" run) == "$RUN_ID" ]] || continue
			dispose "$id" || rc=1
		done
		if [[ -n ${SHARED:-} ]]; then root rm -rf --one-file-system "$SHARED" || rc=1; fi
	fi
	rm -rf "$WORK" 2>/dev/null || root rm -rf "$WORK" || rc=1
	exit "$rc"
}

cmd_run() {
	RUN_ID="$(date +%s)-$$"
	WORK=$(mktemp -d)
	LOG_DIR="${E2E_LOG_DIR:-$PWD/e2e-logs}"
	mkdir -p "$LOG_DIR" "$WORK/status"
	trap cleanup_run EXIT
	trap 'log interrupted; exit 130' INT TERM

	prepare_shared
	local listed
	listed=$("$WORK/e2e.test" -test.list '^Test')
	mapfile -t SCENARIOS < <(grep -E "${E2E_RUN:-.}" <<<"$listed" || true)
	if ((${#SCENARIOS[@]} == 0)); then
		log "no scenario matches E2E_RUN=${E2E_RUN:-}"
		exit 1
	fi
	log "running ${#SCENARIOS[@]} scenarios, $PARALLEL at a time; logs in $LOG_DIR"

	local neighbour_id="" name rc
	local -a rest=()
	for name in "${SCENARIOS[@]}"; do
		if [[ $name == "$NEIGHBOUR" ]]; then
			: >"$LOG_DIR/$name.log"
			if neighbour_id=$(allocate "$name" 2>>"$LOG_DIR/$name.log") &&
				interruptible run_test "$neighbour_id" "$name" "^$name\$/^plant\$" "$LOG_DIR/$name.log"; then
				echo 0 >"$WORK/status/$name"
			else
				echo 1 >"$WORK/status/$name"
			fi
		else
			rest+=("$name")
		fi
	done

	for name in "${rest[@]}"; do
		while (($(jobs -pr | wc -l) >= PARALLEL)); do wait -n || true; done
		scenario "$name" &
	done
	wait

	if [[ -n $neighbour_id ]]; then
		if [[ $(cat "$WORK/status/$NEIGHBOUR") == 0 ]]; then
			rc=0
			interruptible run_test "$neighbour_id" "$NEIGHBOUR" "^$NEIGHBOUR\$/^verify\$" "$LOG_DIR/$NEIGHBOUR.log" || rc=$?
			echo "$rc" >"$WORK/status/$NEIGHBOUR"
		fi
		if [[ ${E2E_KEEP:-} != 1 ]]; then
			dispose "$neighbour_id" 2>>"$LOG_DIR/$NEIGHBOUR.log" || echo disposal >"$WORK/status/$NEIGHBOUR.cleanup"
		fi
	fi

	for name in "${SCENARIOS[@]}"; do
		if [[ $(cat "$WORK/status/$name" 2>/dev/null) != 0 ]]; then
			echo "===== $name" >&2
			cat "$LOG_DIR/$name.log" >&2
		fi
	done
	report
}

# cmd_recover disposes of every recorded allocation and shared run directory
# whose runner is gone.
cmd_recover() {
	local dir id pid rc=0
	[[ -d $STATE_ROOT ]] || return 0
	for dir in "$STATE_ROOT/$ID_PREFIX"*/ "$STATE_ROOT/run-"*/; do
		[[ -d $dir ]] || continue
		id=$(basename "$dir")
		if [[ $id != run-* && ! -f $dir/allocation ]]; then
			# Interrupted between creating the directory and writing the
			# record, before any account was created.
			if ! getent passwd "$id" >/dev/null; then root rm -rf --one-file-system "$dir" || rc=1; fi
			continue
		fi
		if [[ $id == run-* ]]; then
			pid=$(root sed -n 's/^runner_pid=//p' "$dir/runner" 2>/dev/null || true)
		else
			pid=$(record_get "$id" runner_pid)
		fi
		if [[ -n $pid ]] && root kill -0 "$pid" 2>/dev/null; then
			log "$id: its runner (pid $pid) is still running; skipped"
			continue
		fi
		log "recovering $id"
		if [[ $id == run-* ]]; then
			root rm -rf --one-file-system "$dir" || rc=1
		else
			dispose "$id" || rc=1
		fi
	done
	return "$rc"
}

cd "$(dirname "$0")/.."
case "${1:-run}" in
run) cmd_run ;;
recover) cmd_recover ;;
*)
	echo "usage: $0 [run|recover]" >&2
	exit 2
	;;
esac
