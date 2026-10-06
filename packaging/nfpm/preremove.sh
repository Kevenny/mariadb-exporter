#!/bin/sh
# dpkg passes "remove"/"upgrade"; rpm passes the number of versions left
# (0 on erase, 1 on upgrade). Only a real removal stops the service.
set -e

case "$1" in
    remove | 0)
        if [ -d /run/systemd/system ]; then
            systemctl disable --now mariadb_exporter.service >/dev/null 2>&1 || true
        fi
        ;;
esac
