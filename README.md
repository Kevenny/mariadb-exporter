# mariadb_exporter

Prometheus exporter **dedicated to MariaDB**, built to cover the gaps in
`mysqld_exporter` when the target is MariaDB — especially the plugins and
features that don't exist in MySQL and therefore have no structured
collection.

What `mysqld_exporter` doesn't deliver and this exporter does:

- **`userstat` statistics** — `USER_STATISTICS`, `TABLE_STATISTICS`,
  `INDEX_STATISTICS` and `CLIENT_STATISTICS`, including unused-index detection
- **`query_response_time`** exposed as a native Prometheus histogram
- **`METADATA_LOCK_INFO`** — active and waiting metadata locks
- **`DISKS`** — filesystem usage as seen by the server itself
- **Multi-source replication** via `SHOW ALL SLAVES STATUS`
- **Automatic version detection**: if the instance isn't MariaDB, the exporter
  exits at startup with a clear message instead of failing silently
- **Automatic plugin detection**: a disabled plugin produces zero metrics and
  `mariadb_collector_available=0`, without turning the scrape into an error

---

## Table of contents

- [Installation](#installation)
- [Configuration](#configuration)
- [Monitoring user](#monitoring-user)
- [Per-collector prerequisites](#per-collector-prerequisites)
- [Exposed metrics](#exposed-metrics)
- [Custom metrics](#custom-metrics)
- [PMM integration](#pmm-integration)
- [Endpoints](#endpoints)
- [Security](#security)
- [Development](#development)
- [Troubleshooting](#troubleshooting)

---

## Installation

### From source

Requires Go 1.22 or later.

```bash
git clone https://github.com/Kevenny/mariadb-exporter.git
cd mariadb-exporter
make build          # produces bin/mariadb_exporter
```

### Docker

```bash
make docker

docker run -d --name mariadb_exporter \
  -p 9104:9104 \
  -e MARIADB_DSN="mariadb://mariadb_exporter:password@tcp(10.0.0.10:3306)/" \
  mariadb_exporter:latest
```

### Systemd (Oracle Linux 9 / RHEL / Ubuntu)

```bash
# Binary
sudo install -m 0755 bin/mariadb_exporter /usr/local/bin/mariadb_exporter

# Service user with no shell or home
sudo useradd --system --no-create-home --shell /sbin/nologin mariadb_exporter

# Configuration (contains the password — 640, owned by root, service group)
sudo mkdir -p /etc/mariadb_exporter
sudo install -m 0640 -o root -g mariadb_exporter \
  packaging/systemd/mariadb_exporter.env /etc/mariadb_exporter/mariadb_exporter.env
sudo vi /etc/mariadb_exporter/mariadb_exporter.env   # set MARIADB_DSN

# Unit
sudo install -m 0644 packaging/systemd/mariadb_exporter.service \
  /etc/systemd/system/mariadb_exporter.service

sudo systemctl daemon-reload
sudo systemctl enable --now mariadb_exporter
sudo systemctl status mariadb_exporter
```

---

## Configuration

The DSN is required and can come from the `--datasource.name` flag or the
`MARIADB_DSN` environment variable.

### DSN format

The `mariadb://` prefix is accepted for semantic clarity and converted
internally before reaching the driver, which speaks the MySQL protocol.

```bash
# TCP
mariadb://pmm:password@tcp(localhost:3306)/
mariadb://pmm:password@tcp(192.168.1.10:3306)/

# Unix socket
mariadb://pmm:password@unix(/var/run/mysql/mysql.sock)/

# With driver parameters
mariadb://pmm:password@tcp(localhost:3306)/?timeout=30s&readTimeout=30s

# Without credentials: the driver reads ~/.my.cnf
mariadb://@tcp(localhost:3306)/?readTimeout=30s
```

> The password never appears in logs — the DSN is masked before being logged.

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--web.listen-address` | `:9104` | Listen address and port (**repeatable** for multiple addresses) |
| `--web.config.file` | — | YAML file with TLS and/or basic auth (see [Security](#security)) |
| `--web.systemd-socket` | `false` | Use systemd socket activation instead of opening the port (Linux) |
| `--web.telemetry-path` | `/metrics` | Metrics path |
| `--web.max-requests` | `0` | Maximum concurrent scrapes (0 = unlimited) |
| `--datasource.name` | env `MARIADB_DSN` | Connection DSN |
| `--datasource.max-open` | `3` | Maximum open connections |
| `--datasource.max-idle` | `3` | Maximum idle connections |
| `--datasource.timeout` | `30s` | Query timeout |
| `--collector.userstat` | `true` | `userstat` collector |
| `--collector.tablestat` | `true` | `tablestat` collector |
| `--collector.tablestat.limit` | `500` | Table limit per scrape |
| `--collector.indexstat` | `true` | `indexstat` collector |
| `--collector.indexstat.limit` | `1000` | Index limit per scrape |
| `--collector.clientstat` | `true` | `clientstat` collector |
| `--collector.query_response_time` | `true` | Response time collector |
| `--collector.metadata_locks` | `true` | Metadata locks collector |
| `--collector.disks` | `true` | Disk usage collector |
| `--collector.replication` | `true` | Replication collector |
| `--collector.galera` | `false` | Galera collector (**opt-in**) |
| `--collector.innodb` | `true` | InnoDB collector |
| `--collector.global_status` | `true` | `global_status` collector |
| `--collector.global_variables` | `true` | `global_variables` collector |
| `--custom-metrics` | — | Custom metrics YAML file (repeatable) |
| `--pmm.service-name` | `$(hostname)-mariadb` | Service name in the PMM inventory |
| `--pmm.cluster` | — | Cluster name for grouping in PMM |
| `--pmm.environment` | `production` | Environment (production, staging, dev) |
| `--pmm.replication-set` | — | PMM replication set name (optional) |
| `--log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `--log.format` | `text` | `text` or `json` |
| `--version` | — | Show the version and exit |

The `--pmm.*` flags populate ConstLabels (`service_name`, `cluster`,
`environment`, `replication_set`) on `mariadb_info`, `mariadb_up` and the
exporter's other internal metrics — this is what lets PMM dashboards filter by
these dimensions. Fields left unset simply don't produce a label (no metric
gets an empty label). See [PMM integration](#pmm-integration) for the full
flow.

Any collector can be turned off with the `--no-` prefix, e.g.
`--no-collector.tablestat`.

### Environment variables

The `--web.*` and `--datasource.*` flags also read from the environment:

| Variable | Equivalent flag |
| --- | --- |
| `MARIADB_DSN` | `--datasource.name` |
| `MARIADB_WEB_LISTEN_ADDRESS` | `--web.listen-address` |
| `MARIADB_WEB_TELEMETRY_PATH` | `--web.telemetry-path` |
| `MARIADB_WEB_MAX_REQUESTS` | `--web.max-requests` |
| `MARIADB_DATASOURCE_MAX_OPEN` | `--datasource.max-open` |
| `MARIADB_DATASOURCE_MAX_IDLE` | `--datasource.max-idle` |
| `MARIADB_DATASOURCE_TIMEOUT` | `--datasource.timeout` |
| `MARIADB_PMM_SERVICE_NAME` | `--pmm.service-name` |
| `MARIADB_PMM_CLUSTER` | `--pmm.cluster` |
| `MARIADB_PMM_ENVIRONMENT` | `--pmm.environment` |
| `MARIADB_PMM_REPLICATION_SET` | `--pmm.replication-set` |
| `MARIADB_LOG_LEVEL` | `--log.level` |
| `MARIADB_LOG_FORMAT` | `--log.format` |

An explicit flag always takes precedence over the environment variable.

---

## Monitoring user

Minimum required privileges:

```sql
CREATE USER 'mariadb_exporter'@'127.0.0.1'
  IDENTIFIED BY 'strong_password'
  WITH MAX_USER_CONNECTIONS 5;

GRANT SELECT              ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT PROCESS             ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT REPLICATION CLIENT  ON *.* TO 'mariadb_exporter'@'127.0.0.1';
-- SLAVE MONITOR is required for SHOW ALL SLAVES STATUS starting with MariaDB
-- 10.5: REPLICATION CLIENT became just an alias of BINLOG MONITOR and is no
-- longer enough on its own. Without this grant, the replication collector
-- fails with "Access denied; you need (at least one of) the SLAVE MONITOR
-- privilege(s)".
GRANT SLAVE MONITOR       ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT RELOAD              ON *.* TO 'mariadb_exporter'@'127.0.0.1';

FLUSH PRIVILEGES;
```

What each privilege is used for:

| Privilege | Needed for |
| --- | --- |
| `SELECT` | `information_schema` tables (userstat, QRT, MDL, DISKS) |
| `PROCESS` | `SHOW ENGINE INNODB STATUS`, `information_schema.PROCESSLIST` |
| `REPLICATION CLIENT` | Compatibility (on MariaDB >= 10.5 it's an alias of `BINLOG MONITOR`) |
| `SLAVE MONITOR` | `SHOW ALL SLAVES STATUS` — **required** on MariaDB >= 10.5 |
| `RELOAD` | Administrative flush operations |

---

## Per-collector prerequisites

Every collector with an external dependency publishes
`mariadb_collector_available{collector="name"}`. When the dependency isn't
satisfied, the collector **does not produce an error** — just zero metrics and
a log warning.

| Collector | Prerequisite | How to enable |
| --- | --- | --- |
| `userstat`, `tablestat`, `indexstat`, `clientstat` | `userstat=ON` variable | `SET GLOBAL userstat = ON` |
| `query_response_time` | plugin + `query_response_time_stats=ON` | `INSTALL SONAME 'query_response_time'; SET GLOBAL query_response_time_stats = ON;` |
| `metadata_locks` | `metadata_lock_info` plugin, MariaDB >= 10.0.7 | `INSTALL SONAME 'metadata_lock_info';` |
| `disks` | `disks` plugin | `INSTALL SONAME 'disks';` |
| `galera` | wsrep active + `--collector.galera` | Galera cluster running |
| `replication` | `SLAVE MONITOR` (MariaDB >= 10.5) | `GRANT SLAVE MONITOR ON *.* TO ...` |

To make the changes permanent, use the server's configuration file:

```ini
# /etc/my.cnf.d/monitoring.cnf
[mariadb]
userstat = 1
plugin_load_add = query_response_time
query_response_time_stats = ON
plugin_load_add = metadata_lock_info
plugin_load_add = disks
```

---

## Exposed metrics

### Instance

| Metric | Type | Labels |
| --- | --- | --- |
| `mariadb_info` | gauge (=1) | `version`, `version_comment`, `hostname`, `server_id` |

### `global_status` (explicit list, no wildcard)

`mariadb_connections_total`, `mariadb_max_used_connections`,
`mariadb_aborted_connects_total`, `mariadb_aborted_clients_total`,
`mariadb_bytes_received_total`, `mariadb_bytes_sent_total`,
`mariadb_questions_total`, `mariadb_slow_queries_total`, `mariadb_open_files`,
`mariadb_open_tables`, `mariadb_table_open_cache_hits_total`,
`mariadb_table_open_cache_misses_total`, `mariadb_created_tmp_tables_total`,
`mariadb_created_tmp_disk_tables_total`, `mariadb_select_full_join_total`,
`mariadb_select_scan_total`, `mariadb_sort_merge_passes_total`,
`mariadb_uptime_seconds`

### `global_variables`

`mariadb_max_connections`, `mariadb_innodb_buffer_pool_size_bytes`,
`mariadb_query_cache_size_bytes`, `mariadb_thread_cache_size`,
`mariadb_wait_timeout_seconds`, `mariadb_interactive_timeout_seconds`,
`mariadb_userstat_enabled`

### `userstat` — label `user`

`mariadb_user_total_connections_total`, `mariadb_user_concurrent_connections`,
`mariadb_user_rows_read_total`, `mariadb_user_rows_sent_total`,
`mariadb_user_rows_changed_total`, `mariadb_user_select_commands_total`,
`mariadb_user_update_commands_total`, `mariadb_user_other_commands_total`,
`mariadb_user_access_denied_total`, `mariadb_user_lost_connections_total`

### `tablestat` — labels `schema`, `table`

`mariadb_table_rows_read_total`, `mariadb_table_rows_changed_total`,
`mariadb_table_rows_changed_x_indexes_total`

Tables are sorted by `rows_read DESC` before the cutoff, so the limit keeps
the busiest ones.

### `indexstat` — labels `schema`, `table`, `index`

`mariadb_index_rows_read_total`, `mariadb_index_unused` (gauge=1, present only
for indexes with `rows_read = 0`)

### `clientstat` — label `client`

`mariadb_client_total_connections_total`, `mariadb_client_rows_read_total`,
`mariadb_client_rows_sent_total`

### `query_response_time`

`mariadb_query_response_time_seconds` — native Prometheus histogram, with
`_bucket`, `_count` and `_sum`. Bucket bounds come from the table itself; the
`TOO LONG` row doesn't generate a bucket but is counted in `_count` and
`_sum`.

### `metadata_locks` — labels `lock_mode`, `lock_type`, `table_schema`, `table_name`

`mariadb_metadata_locks_total`, `mariadb_metadata_lock_waiting_total`

### `disks` — labels `disk`, `path`

`mariadb_disk_total_bytes`, `mariadb_disk_used_bytes`,
`mariadb_disk_available_bytes`

### `replication` — labels `connection_name`, `master_host`, `master_port`

`mariadb_slave_sql_running`, `mariadb_slave_io_running`,
`mariadb_slave_seconds_behind_master`, `mariadb_slave_last_errno`,
`mariadb_slave_relay_log_pos`

Multi-source is supported: each replication connection becomes its own
series, distinguished by `connection_name`.

### `innodb`

`mariadb_innodb_buffer_pool_read_requests_total`,
`mariadb_innodb_buffer_pool_reads_total`,
`mariadb_innodb_buffer_pool_pages_total{type}` (`free`, `data`, `dirty`,
`misc`, `total`), `mariadb_innodb_row_lock_waits_total`,
`mariadb_innodb_row_lock_time_avg_milliseconds`, `mariadb_innodb_deadlocks_total`

### `galera` (opt-in) — label `cluster_name`

`mariadb_galera_cluster_size`, `mariadb_galera_cluster_status` (0=non-primary,
1=primary), `mariadb_galera_local_state` (0=joining, 1=donor, 2=joined,
3=synced), `mariadb_galera_flow_control_paused`,
`mariadb_galera_recv_queue_avg`, `mariadb_galera_send_queue_avg`

### Exporter's internal metrics

| Metric | Description |
| --- | --- |
| `mariadb_up` | 1 if connected to the database |
| `mariadb_exporter_build_info{version,build_date,go_version}` | Binary build info |
| `mariadb_scrape_duration_seconds` | Histogram of total scrape duration |
| `mariadb_scrape_success` | 1 if no collector failed |
| `mariadb_scrape_errors_total{collector}` | Errors per collector |
| `mariadb_collector_available{collector}` | 1 if the collector's dependencies are OK |
| `mariadb_collector_scrape_duration_seconds{collector}` | Duration per collector |

---

## Custom metrics

Your own metrics can be defined in YAML, without recompiling:

```yaml
mariadb_active_sessions_by_schema:
  query: |
    SELECT db AS schema_name, command, COUNT(*) AS total
    FROM information_schema.PROCESSLIST
    WHERE db IS NOT NULL
    GROUP BY db, command
  metrics:
    - schema_name:
        usage: "LABEL"
        description: "Database schema"
    - command:
        usage: "LABEL"
        description: "Command type"
    - total:
        usage: "GAUGE"
        description: "Total active sessions by schema and command"
```

```bash
mariadb_exporter --custom-metrics=/etc/mariadb_exporter/custom.yml
```

- `usage` accepts `LABEL`, `COUNTER` and `GAUGE`
- Multiple files: repeat the flag (`--custom-metrics=a.yml --custom-metrics=b.yml`)
- With more than one value column, the column name is suffixed to the metric name
- An invalid YAML is a configuration error and prevents startup, instead of
  failing silently on the first scrape

See [custom_metrics/example.yml](custom_metrics/example.yml) for more examples.

---

## PMM integration

Full, detailed documentation in
[mariadb_exporter_pmm_integration.md](mariadb_exporter_pmm_integration.md).
Flow summary:

```
mariadb_exporter (:9104) → pmm-agent pull → VictoriaMetrics (PMM) → Grafana
```

PMM doesn't accept arbitrary external datasources — internally it's always
VictoriaMetrics. What you register is a **scrape target** (External Service);
the exporter never sends metrics, it just waits to be pulled.

### 1. Configure the `--pmm.*` flags

```bash
mariadb_exporter \
  --pmm.service-name="mariadb-$(hostname -s)" \
  --pmm.cluster="my-cluster" \
  --pmm.environment=production
```

This makes `mariadb_info`, `mariadb_up` and the other internal metrics carry
the `service_name`, `cluster` and `environment` labels, used by the
dashboards' filters.

### 2. Register as an External Service

```bash
pmm-admin add external \
  --service-name="mariadb-$(hostname -s)" \
  --listen-port=9104 \
  --metrics-path=/metrics \
  --scheme=http \
  --group=mariadb \
  --environment=production \
  --cluster=my-cluster
```

Check: `pmm-admin list | grep mariadb` should show the service with status UP.

### 3. Automated deployment

[packaging/deploy_mariadb_exporter.sh](packaging/deploy_mariadb_exporter.sh)
automates steps 1 and 2 on a new host — installs the binary, creates the
systemd service with the `--pmm.*` flags already filled in, and registers
with PMM:

```bash
MARIADB_DSN="mariadb://mariadb_exporter:PASSWORD@tcp(localhost:3306)/" \
PMM_CLUSTER="my-cluster" \
  sudo -E ./packaging/deploy_mariadb_exporter.sh
```

### 4. Versioned dashboards and alerts

Six dashboards in [dashboards/](dashboards/), all with `cluster` /
`environment` / `service_name` template variables fed by the `--pmm.*` flags:

| Dashboard | Covers |
| --- | --- |
| [mariadb_overview.json](dashboards/mariadb_overview.json) | Fleet overview: availability, P99 latency, top users and tables, disk, replication |
| [mariadb_innodb.json](dashboards/mariadb_innodb.json) | Buffer pool (hit ratio, occupancy, pages by type), row locks, deadlocks, on-disk tmp tables, table cache, full scans/joins |
| [mariadb_replication.json](dashboards/mariadb_replication.json) | IO/SQL threads, lag, errors, relay log progress — with multi-source support via the `connection_name` variable |
| [mariadb_galera.json](dashboards/mariadb_galera.json) | Cluster size, Primary/non-Primary component, node local state, flow control, send/recv queues |
| [mariadb_users.json](dashboards/mariadb_users.json) | Access denied, aborted/lost connections, `max_connections` usage, command profile and read efficiency per user, traffic by source host |
| [mariadb_tables.json](dashboards/mariadb_tables.json) | Most-read/written tables, index cost on writes, orphan indexes |

Import via PMM UI → Dashboards → Import, or through the embedded Grafana API:

```bash
for f in dashboards/mariadb_*.json; do
  curl -k -u admin:admin -X POST https://<pmm-server>/graph/api/dashboards/db \
    -H "Content-Type: application/json" \
    -d "{\"dashboard\": $(cat "$f"), \"folderTitle\": \"MariaDB\", \"overwrite\": true}"
done
```

> The panels use `"uid": "${datasource}"` with a `datasource`-type template
> variable. In PMM, select **Metrics** in the picker at the top of the
> dashboard (it's the internal VictoriaMetrics); in a plain Grafana, select
> your Prometheus.

- [dashboards/mariadb_alerts.yml](dashboards/mariadb_alerts.yml) — 5 ready
  alert rules (instance down, high P99 latency, replication lag, disk almost
  full, metadata lock accumulation). Import via PMM UI → Alerting → Alert
  Rules.

Some panels depend on server-side prerequisites:

| Dashboard | Depends on |
| --- | --- |
| Galera | `--collector.galera` (opt-in) and wsrep active |
| Users, Tables & Indexes | `userstat=ON` |
| Replication | `GRANT SLAVE MONITOR` (MariaDB >= 10.5) |
| Orphan indexes | `--custom-metrics` with `mariadb_orphan_indexes` (see [custom_metrics/example.yml](custom_metrics/example.yml)) |

### 5. Post-registration verification

```bash
# Metrics arriving with PMM's labels
curl -s http://localhost:9104/metrics | grep -E "^mariadb_up|^mariadb_info"

# In PMM's Grafana: Dashboards → Advanced Data Exploration
# Datasource: Prometheus (internal VictoriaMetrics)
# Filter by: service_name, cluster, environment
```

---

## Endpoints

| Endpoint | Description |
| --- | --- |
| `GET /metrics` | Metrics in Prometheus format |
| `GET /health` | `200 {"status":"ok"}` when connected; `503 {"status":"error","message":"..."}` when disconnected |
| `GET /` | Index page with links |

`/health` is suitable for a load balancer health check, Kubernetes probe, or
Docker `healthcheck`. If you enable basic auth via `--web.config.file`, the
probe will need to send the credential (see [Security](#security)).

---

## Development

```bash
make build      # builds into bin/
make test       # go test -race with coverage
make vet        # go vet
make lint       # golangci-lint
make fmt        # gofmt -s -w
make docker     # Docker image
```

### Full local environment

`docker-compose.yml` brings up a MariaDB 11.4 with all plugins enabled and
the exporter pointed at it:

```bash
docker compose up -d --build

curl -s localhost:9104/health
curl -s localhost:9104/metrics | grep mariadb_collector_available
```

### Structure

```
cmd/mariadb_exporter/    entrypoint, flags, HTTP server bootstrap
collector/               Collector interface + one file per collector
exporter/                collector orchestration, version/plugin detection
config/                  flag and environment variable parsing
web/                     /metrics, /health and / handlers
packaging/systemd/       unit and EnvironmentFile
custom_metrics/          custom metrics YAML example
```

### How to add a collector

1. Create `collector/my_collector.go` embedding `base` and implementing
   `Collect(ctx, db, ch) error`
2. If it depends on a plugin, also implement `Available() bool` and add the
   detection to `exporter/version_detector.go`
3. Add the flag in `config/config.go` and register it in `buildCollectors`
   (`cmd/mariadb_exporter/main.go`)
4. Write the test with `sqlmock`, covering both the happy path **and** the
   missing-dependency path

---

## Troubleshooting

**The exporter exits with "instance is not MariaDB"**
Expected behavior when pointing at MySQL. This exporter depends on tables and
commands exclusive to MariaDB; for MySQL use `mysqld_exporter`.

**`mariadb_collector_available{collector="userstat"} = 0`**
The `userstat` variable is `OFF`. Enable it with `SET GLOBAL userstat = ON`
and make it permanent in `my.cnf`.

**No `query_response_time` metrics**
Confirm both requirements — plugin installed **and** collection turned on:

```sql
SELECT plugin_name, plugin_status FROM information_schema.plugins
 WHERE plugin_name LIKE 'QUERY_RESPONSE%';
SELECT @@global.query_response_time_stats;
```

**`collector failed collector=replication ... you need (at least one of) the SLAVE MONITOR privilege(s)`**
On MariaDB >= 10.5, `GRANT REPLICATION CLIENT` became just an alias of
`BINLOG MONITOR` and no longer authorizes `SHOW ALL SLAVES STATUS`. Grant the
specific privilege:

```sql
GRANT SLAVE MONITOR ON *.* TO 'mariadb_exporter'@'127.0.0.1';
FLUSH PRIVILEGES;
```

**Unused-indexes panel always empty**
Expected: `INDEX_STATISTICS` only lists indexes that have already been read
at least once, so an index that was never touched doesn't show up with
`rows_read = 0`. Use the `mariadb_orphan_indexes` custom metric from
[custom_metrics/example.yml](custom_metrics/example.yml), which compares
declared indexes against the ones that recorded a read. The *Tables &
Indexes* dashboard already has a panel for it.

**`mariadb_up = 0`**
Check the DSN, the firewall, and whether the user has permission to connect
from the exporter's host. The log carries the driver's error; `/health`
carries the same message over HTTP.

**Too many `tablestat` / `indexstat` series**
Lower the limits: `--collector.tablestat.limit=100`,
`--collector.indexstat.limit=200`. Tables and indexes are sorted by
`rows_read DESC`, so the cutoff keeps the most relevant ones.

**Scrape timeouts**
Increase `--datasource.timeout` and consider disabling the more expensive
collectors on instances with many tables (`--no-collector.tablestat`).

---

## Security

### TLS and authentication (`--web.config.file`)

By default `/metrics`, `/health` and `/` are served over **unauthenticated
HTTP** — the standard behavior for Prometheus exporters. To enable HTTPS
and/or basic auth, point `--web.config.file` to a YAML file:

```bash
mariadb_exporter --web.config.file=/etc/mariadb_exporter/web-config.yml
```

```yaml
# /etc/mariadb_exporter/web-config.yml  (0640 root:mariadb_exporter)

# Passwords as bcrypt hashes. Generate with: htpasswd -nBC 10 "" | tr -d ':\n'
basic_auth_users:
  prometheus: $2a$10$YlKXKGr6Zy0o67vkjDv4MOLwG/2vXPCq1ZayOB55cQJqevY3fbfRu

tls_server_config:
  cert_file: /etc/mariadb_exporter/tls/exporter.crt
  key_file:  /etc/mariadb_exporter/tls/exporter.key
  min_version: TLS12
```

Full, commented template, including mTLS: see
[packaging/web-config.yml.example](packaging/web-config.yml.example). The
format is Prometheus's
[exporter-toolkit](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md).

The file is validated **at startup**, before opening the listener: a
malformed bcrypt hash or a missing certificate aborts the process with a
clear error, instead of leaving the exporter listening unprotected until the
first request.

When enabling TLS, adjust whoever does the scrape:

```bash
# PMM
pmm-admin add external --scheme=https ...
```

```yaml
# Prometheus
scrape_configs:
  - job_name: 'mariadb'
    scheme: https
    basic_auth:
      username: prometheus
      password: senha_do_prometheus
    tls_config:
      ca_file: /etc/prometheus/tls/ca.crt   # if the cert is self-signed
    static_configs:
      - targets: ['mariadb-host01:9104']
```

> **Basic auth applies to all paths, including `/health`.** The toolkit
> doesn't allow excluding a path. If you use `/health` as a load balancer,
> Kubernetes, or Docker probe, the probe needs to send the credential — or
> leave authentication out and protect the exporter at the network level.

### If you don't use TLS/auth

- **Expose the exporter only on the monitoring network.** Prefer
  `--web.listen-address=127.0.0.1:9104` when pmm-agent runs on the same host,
  or restrict it via firewall/security group.
- The metrics reveal database user, schema, table and index names. It's not
  data content, but it is structural information — treat it as sensitive.
- `/health` deliberately returns a generic message on failure: the driver's
  error may contain the DSN (with the password) or internal addresses. The
  detail goes only to the exporter's log.

### Credentials

- The password is never logged: the DSN is masked before any log line.
- systemd's `EnvironmentFile` contains the password and must be `0640
  root:<service>` (the deploy script already creates it this way, with no
  open permission window).
- Prefer a dedicated MariaDB user with the minimum privileges from the
  [Monitoring user](#monitoring-user) section and `MAX_USER_CONNECTIONS 5`.

### Robustness against hostile server data

The exporter treats content coming from the database as untrusted:

- Labels with bytes that don't form valid UTF-8 (latin1 names, blobs) are
  sanitized. Without this, `client_golang` would panic and take down the
  entire process during a scrape.
- Non-finite values (`NaN`, `Inf`) are rejected instead of exposed as a
  metric, since they silently break alerts and graphs.
- Out-of-range counts in the `query_response_time` histogram are discarded,
  avoiding a `uint64` wraparound that would corrupt every rate query.
- A panic inside a collector is isolated to that collector, counted in
  `mariadb_scrape_errors_total` — the other collectors keep working.
- The custom metrics YAML is validated at startup (metric and label names,
  duplicate or ambiguous columns), turning what would be a runtime panic into
  an explicit configuration failure.

---

## Implementation notes

Points where the server's reality differs from the original specification,
and how they were handled — all validated against a real MariaDB 11.4.12.

**`USER_STATISTICS` has no `ROWS_CHANGED` column**
The specification asks for the `mariadb_user_rows_changed_total` metric, but
MariaDB's `information_schema.USER_STATISTICS` table breaks that data down
into `ROWS_DELETED`, `ROWS_INSERTED` and `ROWS_UPDATED` (unlike
`TABLE_STATISTICS`, which actually has `ROWS_CHANGED`). The metric is
computed as the sum of the three columns, preserving the requested semantics.
Without this, the `userstat` collector would fail with `Unknown column
'ROWS_CHANGED'`.

**Two metric names diverge from Prometheus convention**
`promtool check metrics` flags two style warnings:

- `mariadb_innodb_buffer_pool_pages_total` is a gauge with a `_total` suffix
- `mariadb_innodb_row_lock_time_avg_milliseconds` uses milliseconds instead
  of the base unit `seconds`

Both names are literally required by the specification (section 2.2) and
were kept: renaming them would break dashboards that query these names. The
rest of the output passes `promtool` with no remarks.

**Source of InnoDB metrics**
The specification cites `SHOW ENGINE INNODB STATUS` as the source. The
requested values (buffer pool, row locks) are all available as counters in
`SHOW GLOBAL STATUS`, which is structured and stable across versions —
parsing `INNODB STATUS`'s free-form text would be fragile. The text is used
only as a fallback for `mariadb_innodb_deadlocks_total`, which doesn't exist
as a status variable on all builds.

**`prometheus/common` version**
The specification lists `v0.52.0`, which was never published to the upstream
repository. The project uses `v0.52.3`, the closest real release.

---

## License

Internal use.
