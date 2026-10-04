# Reads restic's `snapshots --json` output on stdin and prints the ID of the
# newest snapshot; fails when there is none or it is older than argv[1] days.
# Runs in the python image (restore-verify.sh): restore-verify must not pass on
# an old snapshot while the backups have stopped.
import datetime
import json
import re
import sys


def parse_time(value):
    # restic writes RFC 3339 with nanoseconds; fromisoformat takes microseconds.
    value = re.sub(r"(\.\d{6})\d+", r"\1", value).replace("Z", "+00:00")
    return datetime.datetime.fromisoformat(value)


max_age = datetime.timedelta(days=float(sys.argv[1]))
snapshots = json.load(sys.stdin) or []
if not snapshots:
    sys.exit("no forge snapshot in the repository")

newest = max(snapshots, key=lambda s: parse_time(s["time"]))
age = datetime.datetime.now(datetime.timezone.utc) - parse_time(newest["time"])
if age > max_age:
    sys.exit(f"the newest forge snapshot {newest['short_id']} is {age} old: the backups have stopped")
print(newest["id"])
