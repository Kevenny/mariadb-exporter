#!/bin/bash
# deploy_mariadb_exporter.sh
#
# Installs and registers mariadb_exporter on a MariaDB server as a systemd
# service, and registers it with PMM as an External Service (see
# mariadb_exporter_pmm_integration.md, sections 6 and 7).
#
# Run as root on each MariaDB server, from the repository's root directory
# (expects to find ./bin/mariadb_exporter already built via `make build`).
#
# Usage:
#   MARIADB_DSN="mariadb://mariadb_exporter:PASSWORD@tcp(localhost:3306)/" \
#     ./packaging/deploy_mariadb_exporter.sh
#
# Security note: the DSN arrives via environment variable and stays visible in
# /proc/<pid>/environ while the script runs, and also lands in the shell
# history. In shared environments, prefer exporting the variable from a file
# with restricted permissions (`set -a; . /root/.mariadb_dsn; set +a`) or use a
# secrets manager, instead of typing the password on the command line.
#
# Accepted environment variables (all optional except MARIADB_DSN):
#   MARIADB_DSN         Exporter connection DSN (required)
#   EXPORTER_VERSION    Version shown in logs (default: dev)
#   EXPORTER_USER       Service system user (default: mariadb_exporter)
#   EXPORTER_PORT       Listen port (default: 9104)
#   PMM_CLUSTER         --pmm.cluster (default: empty — no grouping)
#   PMM_ENV             --pmm.environment (default: production)
#   PMM_REPLICATION_SET --pmm.replication-set (default: empty)
#   WEB_CONFIG_FILE     --web.config.file for TLS/basic auth (default: empty =
#                       unauthenticated HTTP). See packaging/web-config.yml.example

set -euo pipefail

EXPORTER_VERSION="${EXPORTER_VERSION:-dev}"
EXPORTER_USER="${EXPORTER_USER:-mariadb_exporter}"
EXPORTER_PORT="${EXPORTER_PORT:-9104}"
PMM_CLUSTER="${PMM_CLUSTER:-}"
PMM_ENV="${PMM_ENV:-production}"
PMM_REPLICATION_SET="${PMM_REPLICATION_SET:-}"
WEB_CONFIG_FILE="${WEB_CONFIG_FILE:-}"
MARIADB_DSN="${MARIADB_DSN:?set MARIADB_DSN before running this script}"

SERVICE_NAME="mariadb-$(hostname -s)"

if [[ $EUID -ne 0 ]]; then
    echo "this script needs to run as root" >&2
    exit 1
fi

if [[ ! -x ./bin/mariadb_exporter ]]; then
    echo "binary ./bin/mariadb_exporter not found; run 'make build' first" >&2
    exit 1
fi

UNIT_SRC=./packaging/systemd/mariadb_exporter.service
if [[ ! -r "$UNIT_SRC" ]]; then
    echo "$UNIT_SRC not found; run this script from the repository root" >&2
    exit 1
fi

# These values end up inside the systemd unit's ExecStart. Restricting them to
# a safe character set keeps a stray quote or space from injecting extra
# arguments or breaking the unit.
require_match() {
    local name=$1 value=$2 pattern=$3
    if [[ -n "$value" && ! "$value" =~ $pattern ]]; then
        echo "$name=\"$value\" has invalid characters (allowed: $pattern)" >&2
        exit 1
    fi
}
require_match EXPORTER_USER       "$EXPORTER_USER"       '^[a-z_][a-z0-9_-]*$'
require_match EXPORTER_PORT       "$EXPORTER_PORT"       '^[0-9]{1,5}$'
require_match PMM_CLUSTER         "$PMM_CLUSTER"         '^[A-Za-z0-9._-]+$'
require_match PMM_ENV             "$PMM_ENV"             '^[A-Za-z0-9._-]+$'
require_match PMM_REPLICATION_SET "$PMM_REPLICATION_SET" '^[A-Za-z0-9._-]+$'
require_match WEB_CONFIG_FILE     "$WEB_CONFIG_FILE"     '^/[A-Za-z0-9._/-]+$'
require_match SERVICE_NAME        "$SERVICE_NAME"        '^[A-Za-z0-9._-]+$'

if [[ "$MARIADB_DSN" == *$'\n'* || "$MARIADB_DSN" == *$'\r'* ]]; then
    echo "MARIADB_DSN must not contain line breaks" >&2
    exit 1
fi

# 1. Create the system user for the exporter
if ! id "$EXPORTER_USER" &>/dev/null; then
    useradd --system --no-create-home --shell /sbin/nologin "$EXPORTER_USER"
fi

# 2. Install the binary
install -o root -g root -m 0755 \
    ./bin/mariadb_exporter \
    /usr/local/bin/mariadb_exporter

# 3. Create the configuration directory
mkdir -p /etc/mariadb_exporter
chmod 750 /etc/mariadb_exporter
chown root:"$EXPORTER_USER" /etc/mariadb_exporter

# 4. Create the environment file with the DSN.
#
# The file contains the monitoring user's password, so it's created already
# with the restricted permission: a `cat >` followed by chmod would leave a
# window where the file is readable per the umask (typically 644), long enough
# for another local process to read the credential.
ENV_FILE=/etc/mariadb_exporter/mariadb_exporter.env
install -o root -g "$EXPORTER_USER" -m 0640 /dev/null "$ENV_FILE"
# systemd parses EnvironmentFile values with shell-like quoting: unquoted, a
# password containing '"', '\', '$' or '`' would be silently altered and the
# connection would fail. Double-quoting with those four escaped keeps it intact.
DSN_ESCAPED=$(printf '%s' "$MARIADB_DSN" | sed 's/[\\"`$]/\\&/g')
printf 'MARIADB_DSN="%s"\n' "$DSN_ESCAPED" > "$ENV_FILE"

# 5. Assemble PMM arguments conditionally — cluster and replication-set are
# optional and shouldn't turn into empty flags in ExecStart.
PMM_ARGS="--pmm.environment=${PMM_ENV} --pmm.service-name=${SERVICE_NAME}"
if [[ -n "$PMM_CLUSTER" ]]; then
    PMM_ARGS="${PMM_ARGS} --pmm.cluster=${PMM_CLUSTER}"
fi
if [[ -n "$PMM_REPLICATION_SET" ]]; then
    PMM_ARGS="${PMM_ARGS} --pmm.replication-set=${PMM_REPLICATION_SET}"
fi

# TLS/basic auth are opt-in via WEB_CONFIG_FILE. When active, the exporter
# starts serving HTTPS, so both the local health check and the PMM
# registration need to use the right scheme.
WEB_ARGS=""
SCHEME="http"
CURL_TLS_OPT=""
if [[ -n "$WEB_CONFIG_FILE" ]]; then
    if [[ ! -r "$WEB_CONFIG_FILE" ]]; then
        echo "WEB_CONFIG_FILE=$WEB_CONFIG_FILE does not exist or is not readable" >&2
        exit 1
    fi
    WEB_ARGS="--web.config.file=${WEB_CONFIG_FILE}"
    # Only assume HTTPS if the file actually configures TLS: the same file may
    # enable only basic auth, keeping HTTP.
    if grep -qE "^[[:space:]]*tls_server_config:" "$WEB_CONFIG_FILE"; then
        SCHEME="https"
        # The certificate may be self-signed; verification is left to whoever
        # does the scrape, not to this local health check.
        CURL_TLS_OPT="-k"
    fi
fi

# 6. Install the versioned systemd unit (single source of the hardening
# directives) and put the host-specific settings in a drop-in.
install -o root -g root -m 0644 "$UNIT_SRC" /etc/systemd/system/mariadb_exporter.service
install -d -o root -g root -m 0755 /etc/systemd/system/mariadb_exporter.service.d
cat > /etc/systemd/system/mariadb_exporter.service.d/deploy.conf <<EOF
# Generated by deploy_mariadb_exporter.sh — rerun the script to change it.
[Service]
User=${EXPORTER_USER}
Group=${EXPORTER_USER}
ExecStart=
ExecStart=/usr/local/bin/mariadb_exporter \\
  --web.listen-address=:${EXPORTER_PORT} \\
  --web.telemetry-path=/metrics \\
  ${WEB_ARGS} \\
  ${PMM_ARGS} \\
  --log.level=info \\
  --log.format=json
EOF

# 7. Enable and start
systemctl daemon-reload
systemctl enable mariadb_exporter
systemctl restart mariadb_exporter

# 8. Check health before registering with PMM — avoids a "Connection check
# failed" from pmm-admin when the exporter hasn't come up yet.
#
# With basic auth enabled, /health also requires a credential (the toolkit
# doesn't allow excluding paths), so a 401 here indicates the exporter is up
# and responding — which is enough to proceed with registration.
sleep 2
HEALTH_CODE=$(curl -s ${CURL_TLS_OPT} -o /dev/null -w "%{http_code}" \
    "${SCHEME}://localhost:${EXPORTER_PORT}/health" || true)
case "$HEALTH_CODE" in
    200)
        echo "exporter healthy at ${SCHEME}://:${EXPORTER_PORT}"
        ;;
    401)
        echo "exporter up at ${SCHEME}://:${EXPORTER_PORT} (401 — basic auth active, expected)"
        ;;
    *)
        echo "exporter did not respond on /health (HTTP ${HEALTH_CODE}); aborting PMM registration" >&2
        systemctl status mariadb_exporter --no-pager || true
        exit 1
        ;;
esac

# 9. Register with PMM (idempotent: if the service already exists, pmm-admin
# warns and the script should not fail because of it).
PMM_REGISTER_ARGS=(
    --service-name="${SERVICE_NAME}"
    --listen-port="${EXPORTER_PORT}"
    --metrics-path="/metrics"
    --scheme="${SCHEME}"
    --group=mariadb
    --environment="${PMM_ENV}"
)
[[ -n "$PMM_CLUSTER" ]] && PMM_REGISTER_ARGS+=(--cluster="${PMM_CLUSTER}")
[[ -n "$PMM_REPLICATION_SET" ]] && PMM_REGISTER_ARGS+=(--replication-set="${PMM_REPLICATION_SET}")

if command -v pmm-admin &>/dev/null; then
    pmm-admin add external "${PMM_REGISTER_ARGS[@]}" || \
        echo "warning: pmm-admin add external failed (the service may already exist) — check with 'pmm-admin list'" >&2

    if [[ -n "$WEB_CONFIG_FILE" ]] && grep -qE "^[[:space:]]*basic_auth_users:" "$WEB_CONFIG_FILE"; then
        echo "WARNING: basic auth is active, but 'pmm-admin add external' does not accept credentials." >&2
        echo "         Configure the username and password on the service via the PMM UI (Inventory > the service)," >&2
        echo "         otherwise the scrape will fail with 401." >&2
    fi
else
    echo "warning: pmm-admin not found on this host; skipped PMM registration" >&2
fi

echo "deploy completed on $(hostname) — version ${EXPORTER_VERSION}, service ${SERVICE_NAME}"
