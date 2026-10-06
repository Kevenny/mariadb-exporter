#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

# The service is not enabled automatically: it needs the DSN first.
cat <<'EOF'
mariadb_exporter installed. Next steps:
  1. Set MARIADB_DSN in /etc/mariadb_exporter/mariadb_exporter.env
  2. systemctl enable --now mariadb_exporter
EOF
