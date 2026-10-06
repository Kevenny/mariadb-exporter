#!/bin/sh
set -e

GRAFANA="https://pmm-server:8443/graph"
AUTH="admin:admin"

folder_uid=mariadb-exporter
curl -sk -u "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"uid\":\"$folder_uid\",\"title\":\"MariaDB Exporter\"}" \
  "$GRAFANA/api/folders" >/dev/null || true

for f in /dashboards/*.json; do
  printf '{"dashboard":%s,"folderUid":"%s","overwrite":true}' "$(cat "$f")" "$folder_uid" \
    | curl -sk -u "$AUTH" -H 'Content-Type: application/json' --data-binary @- \
        -o /dev/null -w "$(basename "$f"): HTTP %{http_code}\n" \
        "$GRAFANA/api/dashboards/db"
done
