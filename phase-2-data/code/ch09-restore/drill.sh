#!/usr/bin/env bash
# Chapter 2.9 — a restore drill.
#
# Starts a throwaway PostgreSQL server in a temporary directory and plays
# out a bad day:
#
#   1. a shop with 200,000 orders takes its nightly backups: a logical dump
#      (pg_dump) and a physical base backup (pg_basebackup), and archives
#      its write-ahead log continuously;
#   2. the next day, 50,000 more orders arrive;
#   3. a migration with a missing WHERE clause sets every order's total to
#      zero, and nobody notices until 1,000 more orders have come in;
#   4. the database is restored twice: from last night's dump, and from the
#      base backup plus the archived log, stopped just before the mistake.
#
# Needs the PostgreSQL server programs (initdb, pg_ctl, pg_basebackup),
# and must not run as root. With the Docker container from the phase
# overview:
#
#   docker exec -i -u postgres course-pg bash -s < phase-2-data/code/ch09-restore/drill.sh
#
# With PostgreSQL installed on your machine, as an ordinary user:
#
#   bash phase-2-data/code/ch09-restore/drill.sh
#
# It uses its own ports and touches nothing outside its temporary
# directory, which it removes when it finishes. Takes about 30 seconds.
set -euo pipefail

# ---- find the server programs -------------------------------------------
BIN=""
for dir in "$(dirname "$(command -v initdb 2>/dev/null || echo /nonexistent/x)")" \
           "$(pg_config --bindir 2>/dev/null || true)" \
           $(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | tail -1); do
    if [ -x "$dir/initdb" ] && [ -x "$dir/pg_basebackup" ]; then BIN=$dir; break; fi
done
if [ -z "$BIN" ]; then
    echo "Cannot find initdb and pg_basebackup. See the header of this script for how to run it." >&2; exit 1
fi
if [ "$(id -u)" = 0 ]; then
    echo "PostgreSQL will not run as root. See the header of this script for how to run it." >&2; exit 1
fi

# ---- a throwaway server, with WAL archiving switched on ------------------
WORK=$(mktemp -d)
ARCHIVE=$WORK/wal-archive
mkdir "$ARCHIVE"
PORT=54330            # the "production" server
RESTORED_PORT=54331   # the server restored from the base backup
export PGHOST=$WORK PGPORT=$PORT PGUSER=drill PGDATABASE=shop
unset PGPASSWORD PGSERVICE

trap '"$BIN/pg_ctl" -D "$WORK/data" -m immediate stop >/dev/null 2>&1 || true
      "$BIN/pg_ctl" -D "$WORK/restored" -m immediate stop >/dev/null 2>&1 || true
      rm -rf "$WORK"' EXIT

"$BIN/initdb" -D "$WORK/data" -U drill -A trust >/dev/null
cat >> "$WORK/data/postgresql.conf" <<EOF
listen_addresses = ''
unix_socket_directories = '$WORK'
port = $PORT
archive_mode = on                          # every finished WAL file...
archive_command = 'cp %p $ARCHIVE/%f'      # ...is copied to the archive
EOF
"$BIN/pg_ctl" -D "$WORK/data" -l "$WORK/server.log" -w start >/dev/null
"$BIN/createdb" shop

sql()     { psql -X -q -At -v ON_ERROR_STOP=1 "$@"; }
seconds() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.1f s", b - a }'; }
now()     { sql -d postgres -c "SELECT extract(epoch FROM clock_timestamp())"; }   # the server's clock: portable
size()    { du -sh "$1" | cut -f1; }
state()   { # usage: state [database] [port]
            sql -d "${1:-shop}" -p "${2:-$PORT}" -c \
            "SELECT to_char(count(*), 'FM999,999,990') || ' orders, worth \$' ||
                    to_char(coalesce(sum(total_cents), 0) / 100, 'FM999,999,999,990') FROM orders"; }

# ---- 1. the shop, and last night's backups -------------------------------
echo "1. Last night"
sql -c "CREATE TABLE orders (
            id          bigserial PRIMARY KEY,
            customer_id int NOT NULL,
            total_cents int NOT NULL,
            created_at  timestamptz NOT NULL DEFAULT now())" \
    -c "CREATE INDEX ON orders (customer_id)" \
    -c "INSERT INTO orders (customer_id, total_cents)
        SELECT 1 + g % 20000, 500 + (g * 7919) % 20000 FROM generate_series(1, 200000) g"
echo "   the shop has $(state)"

t=$(now); "$BIN/pg_dump" -Fc -f "$WORK/shop.dump" shop
echo "   logical backup  (pg_dump)        took $(seconds "$t" "$(now)"), $(size "$WORK/shop.dump")"
t=$(now); "$BIN/pg_basebackup" -D "$WORK/base-backup" --checkpoint=fast
echo "   physical backup (pg_basebackup)  took $(seconds "$t" "$(now)"), $(size "$WORK/base-backup")"

# ---- 2. the next day ------------------------------------------------------
echo
echo "2. The next day"
sql -c "INSERT INTO orders (customer_id, total_cents)
        SELECT 1 + g % 20000, 500 + (g * 7919) % 20000 FROM generate_series(200001, 250000) g"
before=$(state)
last_good_moment=$(sql -c "SELECT now()")
echo "   50,000 more orders arrive: $before"
sleep 1

# ---- 3. the mistake --------------------------------------------------------
echo
echo "3. The mistake:  UPDATE orders SET total_cents = 0;   -- the WHERE clause was forgotten"
sql -c "UPDATE orders SET total_cents = 0"
sql -c "INSERT INTO orders (customer_id, total_cents)
        SELECT 1 + g % 20000, 500 + (g * 7919) % 20000 FROM generate_series(250001, 251000) g"
echo "   by the time anyone notices, 1,000 more orders have arrived: $(state)"

# The archive receives a WAL file only when the file is full. Close the
# current one so that the archive is up to date, and wait for it to arrive.
segment=$(sql -c "SELECT pg_walfile_name(pg_switch_wal())")
until [ -f "$ARCHIVE/$segment" ]; do sleep 0.2; done
echo "   (the WAL archive holds $(ls "$ARCHIVE" | wc -l) files, $(size "$ARCHIVE"))"

# ---- 4a. restore from the dump ---------------------------------------------
echo
echo "4. Restore A — last night's dump, into a new database"
t=$(now)
"$BIN/createdb" restored_from_dump
"$BIN/pg_restore" -d restored_from_dump "$WORK/shop.dump"
echo "   took $(seconds "$t" "$(now)"):  $(state restored_from_dump)"
echo "   every order since the backup is missing"

# ---- 4b. point-in-time recovery ---------------------------------------------
echo
echo "5. Restore B — the base backup, plus the archived WAL replayed up to $last_good_moment"
t=$(now)
cp -r "$WORK/base-backup" "$WORK/restored"
chmod 700 "$WORK/restored"
cat >> "$WORK/restored/postgresql.conf" <<EOF
port = $RESTORED_PORT
archive_mode = off
restore_command = 'cp $ARCHIVE/%f %p'            # where to fetch archived WAL from
recovery_target_time = '$last_good_moment'       # stop replaying here
recovery_target_action = 'promote'               # then open for business
EOF
touch "$WORK/restored/recovery.signal"             # "start in recovery mode"
"$BIN/pg_ctl" -D "$WORK/restored" -l "$WORK/restored.log" -w start >/dev/null
until [ "$(sql -p $RESTORED_PORT -c "SELECT pg_is_in_recovery()")" = f ]; do sleep 0.2; done
after=$(state shop $RESTORED_PORT)
echo "   took $(seconds "$t" "$(now)"):  $after"
if [ "$after" = "$before" ]; then
    echo "   exactly the state just before the mistake"
else
    echo "   NOT the state before the mistake — the drill failed" >&2; exit 1
fi

# The damaged server was never touched: the restore went into a new
# directory. So the orders placed after the mistake can still be rescued.
echo "   the 1,000 orders placed after the mistake are still in the damaged database:" \
     "$(sql -c "SELECT count(*) FROM orders WHERE id > 250000") rows to copy across"
