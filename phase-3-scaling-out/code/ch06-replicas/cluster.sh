#!/usr/bin/env bash
# Chapter 3.6 — a primary and two replicas, on this machine.
#
#   bash phase-3-scaling-out/code/ch06-replicas/cluster.sh start
#   go run ./phase-3-scaling-out/code/ch06-replicas
#   bash phase-3-scaling-out/code/ch06-replicas/cluster.sh stop
#
# "start" creates three throwaway PostgreSQL servers in a temporary
# directory, joined by streaming replication:
#
#   port 54350   the primary
#   port 54351   a replica that applies changes as fast as it can
#   port 54352   a replica held 300 ms behind, standing in for one that is
#                busy or far away
#
# They listen on localhost and accept local connections without a
# password. "stop" shuts them down and deletes them.
#
# Needs the PostgreSQL server programs (initdb, pg_ctl, pg_basebackup),
# and must not run as root. See the chapter for running it in Docker.
set -euo pipefail

BIN=""
for dir in "$(dirname "$(command -v initdb 2>/dev/null || echo /nonexistent/x)")" \
           "$(pg_config --bindir 2>/dev/null || true)" \
           $(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | tail -1); do
    if [ -x "$dir/initdb" ] && [ -x "$dir/pg_basebackup" ]; then BIN=$dir; break; fi
done
if [ -z "$BIN" ]; then
    echo "Cannot find initdb and pg_basebackup. See the header of this script." >&2; exit 1
fi
if [ "$(id -u)" = 0 ]; then
    echo "PostgreSQL will not run as root. See the header of this script." >&2; exit 1
fi

ROOT="${TMPDIR:-/tmp}/course-replicas-$(id -u)"
LISTEN="${LISTEN:-localhost}"      # set LISTEN='*' inside a container whose ports are published
PRIMARY=54350
unset PGPASSWORD PGSERVICE PGDATABASE PGUSER PGHOST PGPORT

stop() {
    for name in slow fast primary; do
        if [ -d "$ROOT/$name" ]; then
            "$BIN/pg_ctl" -D "$ROOT/$name" -m immediate stop >/dev/null 2>&1 || true
        fi
    done
    rm -rf "$ROOT"
}

start() {
    stop
    mkdir -p "$ROOT"

    "$BIN/initdb" -D "$ROOT/primary" -U course -A trust >/dev/null
    cat >> "$ROOT/primary/postgresql.conf" <<EOF
listen_addresses = '$LISTEN'
unix_socket_directories = '$ROOT'
port = $PRIMARY
cluster_name = 'primary'
EOF
    if [ "$LISTEN" != localhost ]; then
        echo "host all all all trust" >> "$ROOT/primary/pg_hba.conf"
    fi
    "$BIN/pg_ctl" -D "$ROOT/primary" -l "$ROOT/primary.log" -w start >/dev/null

    # A replica starts as a copy of the primary (a base backup: Chapter 2.9)
    # and then follows it by receiving its write-ahead log as it is written.
    replica() {   # name, port, extra setting
        "$BIN/pg_basebackup" -h "$ROOT" -p $PRIMARY -U course -D "$ROOT/$1" -R --checkpoint=fast
        cat >> "$ROOT/$1/postgresql.conf" <<EOF
port = $2
cluster_name = '$1'
$3
EOF
        "$BIN/pg_ctl" -D "$ROOT/$1" -l "$ROOT/$1.log" -w start >/dev/null
    }
    replica fast 54351 ""
    replica slow 54352 "recovery_min_apply_delay = '300ms'"

    until [ "$("$BIN/psql" -h "$ROOT" -p $PRIMARY -U course -d postgres -Atc \
               "SELECT count(*) FROM pg_stat_replication WHERE state = 'streaming'")" = 2 ]; do
        sleep 0.2
    done
    echo "primary        postgres://course@127.0.0.1:54350/postgres"
    echo "replica, fast  postgres://course@127.0.0.1:54351/postgres"
    echo "replica, slow  postgres://course@127.0.0.1:54352/postgres   (held 300 ms behind)"
}

case "${1:-}" in
    start) start ;;
    stop)  stop; echo "stopped and removed" ;;
    *)     echo "usage: cluster.sh start | stop" >&2; exit 2 ;;
esac
