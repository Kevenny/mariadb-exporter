#!/bin/sh
# Runs before the files are unpacked: the service account must exist so the
# config directory and env file can be owned by its group.
set -e

if ! getent group mariadb_exporter >/dev/null; then
    groupadd --system mariadb_exporter
fi

if ! getent passwd mariadb_exporter >/dev/null; then
    useradd --system --gid mariadb_exporter --no-create-home \
        --home-dir /nonexistent --shell /usr/sbin/nologin mariadb_exporter
fi
