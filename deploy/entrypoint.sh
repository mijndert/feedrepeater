#!/bin/sh
# Restores the database from the replica if it is missing, then runs
# feedrepeater under Litestream so every write is streamed off the host.
set -eu

: "${FR_DB_PATH:=/data/feedrepeater.db}"

if [ -z "${LITESTREAM_REPLICA_URL:-}" ]; then
    echo "LITESTREAM_REPLICA_URL is not set; running without replication" >&2
    exec /usr/local/bin/feedrepeater
fi

if [ ! -f "$FR_DB_PATH" ]; then
    echo "no local database; attempting restore from replica" >&2
    # -if-replica-exists means a first-ever boot starts with an empty database
    # instead of failing.
    litestream restore -if-replica-exists -config /etc/litestream.yml "$FR_DB_PATH"
fi

exec litestream replicate -config /etc/litestream.yml -exec /usr/local/bin/feedrepeater
