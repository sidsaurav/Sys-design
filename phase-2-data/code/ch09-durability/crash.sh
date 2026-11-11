#!/usr/bin/env bash
# Chapter 2.9 — what "committed" means when the server dies.
#
# Starts a throwaway PostgreSQL server in a temporary directory, lets eight
# clients commit payments as fast as they can, kills the server without
# warning, starts it again, and compares what the clients were told had
# been committed with what is actually there. It does this twice: with
# synchronous_commit on (the default) and off.
#
# Needs the PostgreSQL server programs (initdb, pg_ctl, pgbench), and must
# not run as root. With the Docker container from the phase overview:
#
#   docker exec -i -u postgres course-pg bash -s < phase-2-data/code/ch09-durability/crash.sh
#
# With PostgreSQL installed on your machine, as an ordinary user:
#
#   bash phase-2-data/code/ch09-durability/crash.sh
#
# It uses its own port and touches nothing outside its temporary directory,
# which it removes when it finishes. Takes about 20 seconds.
set -euo pipefail

# ---- find the server programs -------------------------------------------
BIN=""
for dir in "$(dirname "$(command -v initdb 2>/dev/null || echo /nonexistent/x)")" \
           "$(pg_config --bindir 2>/dev/null || true)" \
           $(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | tail -1); do
    if [ -x "$dir/initdb" ] && [ -x "$dir/pgbench" ]; then BIN=$dir; break; fi
done
if [ -z "$BIN" ]; then
    echo "Cannot find initdb and pgbench. See the header of this script for how to run it." >&2; exit 1
fi
if [ "$(id -u)" = 0 ]; then
    echo "PostgreSQL will not run as root. See the header of this script for how to run it." >&2; exit 1
fi

# ---- a throwaway server --------------------------------------------------
WORK=$(mktemp -d)
export PGHOST=$WORK PGPORT=54329 PGUSER=drill PGDATABASE=postgres
unset PGPASSWORD PGSERVICE

stop_hard() { "$BIN/pg_ctl" -D "$WORK/data" -m immediate stop >/dev/null 2>&1 || true; }
start()     { "$BIN/pg_ctl" -D "$WORK/data" -l "$WORK/server.log" -w start >/dev/null; }
trap 'stop_hard; rm -rf "$WORK"' EXIT

"$BIN/initdb" -D "$WORK/data" -U drill -A trust >/dev/null
cat >> "$WORK/data/postgresql.conf" <<EOF
listen_addresses = ''
unix_socket_directories = '$WORK'
port = $PGPORT
EOF
start

echo "INSERT INTO payments (amount_cents) VALUES (1999);" > "$WORK/pay.sql"

# ---- the experiment ------------------------------------------------------
crash_test() {   # $1 = on or off
    psql -q -c "SET client_min_messages = warning" -c "DROP TABLE IF EXISTS payments" \
            -c "CREATE TABLE payments (id bigserial PRIMARY KEY, amount_cents int NOT NULL)"

    # Eight clients each commit one payment per transaction, as fast as
    # they can. pgbench -l writes a line to a log file for every commit
    # that the server confirmed.
    ( cd "$WORK" && PGOPTIONS="-c synchronous_commit=$1" \
        "$BIN/pgbench" -n -f pay.sql -c 8 -j 2 -T 60 -l --log-prefix=confirmed >/dev/null 2>&1 || true ) &
    sleep 3

    # Stop the server the hard way: no checkpoint, nothing flushed, every
    # process killed. The manual: "An immediate-mode shutdown is equivalent
    # to a server crash".
    stop_hard
    wait

    confirmed=$(cat "$WORK"/confirmed.* | wc -l)
    rm -f "$WORK"/confirmed.*
    start      # crash recovery: replay the write-ahead log
    survived=$(psql -Atc "SELECT count(*) FROM payments")

    printf '  synchronous_commit = %-3s   %7d confirmed to the clients   %7d in the table after the crash   ' \
        "$1" "$confirmed" "$survived"
    if [ "$survived" -ge "$confirmed" ]; then
        echo "none lost"
    else
        echo "$((confirmed - survived)) LOST"
    fi
}

echo "Eight clients commit payments for three seconds. Then the server is killed and restarted."
echo
crash_test on
crash_test off
