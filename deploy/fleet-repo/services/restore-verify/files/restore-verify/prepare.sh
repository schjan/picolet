# Turns the restored forge dump in the scratch volume (/restore) into the
# forge's work directory (/var/lib/gitea), the way README "Reference bundles:
# backup, restore-verify" restores it by hand. Runs in the python image
# (restore-verify.sh): its sqlite3 module imports the SQL dump, which the forge
# image has no tool for.
set -eu
cd /restore

mkdir .dump
tar -xf .restic/forge-dump.tar -C .dump
rm -r .restic

# The dump's layout: data/ is the work directory (with custom/conf/app.ini),
# repos/ the repositories, forgejo-db.sql the database. Attachments are the
# exception: the dump stores them under data/attachments/, which unpacks to
# attachments/ at the top of the work directory, but the forge reads them from
# data/attachments/ in it.
cp -a .dump/data/. .
if [ -d attachments ]; then
	mkdir -p data/attachments
	cp -a attachments/. data/attachments/
	rm -r attachments
fi
mkdir -p git
if [ -d .dump/repos ]; then
	mv .dump/repos git/repositories
fi

# The dump also copies the live SQLite file (data/forgejo.db, the path in the
# forge bundle's forge.env) while the forge runs: never restore that copy. The
# database comes from the consistent SQL dump.
rm -f data/forgejo.db data/forgejo.db-wal data/forgejo.db-shm
python3 -c '
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
with open(sys.argv[2]) as sql:
    db.executescript(sql.read())
db.close()
' data/forgejo.db .dump/forgejo-db.sql

rm -r .dump
# The forge image runs as uid 1000.
chown -R 1000:1000 .
